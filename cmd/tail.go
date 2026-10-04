package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/spf13/cobra"
)

const tailPrefix = "edbg-tail-"

var (
	tailBus        string
	tailPattern    string
	tailSource     string
	tailDetailType string
	tailCompact    bool
	tailCloudTrail bool
)

var tailCmd = &cobra.Command{
	Use:   "tail",
	Short: "Live-tail EventBridge events",
	Long: `Creates a temporary SQS queue and EventBridge rule, prints matching events,
and removes both on exit. If a run crashes, use "edbg cleanup".

In a terminal, single-key controls are available: p pause/resume, c clear, q quit.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		pattern, err := buildPattern(tailPattern, tailSource, tailDetailType, tailCloudTrail)
		if err != nil {
			return err
		}
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		return runTail(ctx, awsclient.NewEventBridgeClient(cfg), awsclient.NewSQSClient(cfg), pattern)
	},
}

func init() {
	tailCmd.Flags().StringVar(&tailBus, "bus", "default", "event bus name")
	tailCmd.Flags().StringVar(&tailPattern, "pattern", "", "raw EventBridge event pattern (JSON)")
	tailCmd.Flags().StringVar(&tailSource, "source", "", "match events with this source")
	tailCmd.Flags().StringVar(&tailDetailType, "detail-type", "", "match events with this detail-type")
	tailCmd.Flags().BoolVar(&tailCompact, "compact", false, "one line per event")
	tailCmd.Flags().BoolVar(&tailCloudTrail, "include-cloudtrail", false, "with no filter, also show \"AWS API Call via CloudTrail\" events")
	rootCmd.AddCommand(tailCmd)
}

// cloudTrailDetailType marks the management-API events CloudTrail copies onto the default bus.
// They are high-volume noise for most debugging, so the match-everything pattern leaves them out.
const cloudTrailDetailType = "AWS API Call via CloudTrail"

func buildPattern(raw, source, detailType string, includeCloudTrail bool) (string, error) {
	if raw != "" {
		if !json.Valid([]byte(raw)) {
			return "", fmt.Errorf("--pattern is not valid JSON")
		}
		return raw, nil
	}
	p := map[string]any{}
	if source != "" {
		p["source"] = []string{source}
	}
	if detailType != "" {
		p["detail-type"] = []string{detailType}
	}
	if len(p) == 0 {
		// An empty pattern is rejected, so match any event with a source.
		p["source"] = []map[string]string{{"prefix": ""}}
		if !includeCloudTrail {
			p["detail-type"] = []map[string]string{{"anything-but": cloudTrailDetailType}}
		}
	}
	b, _ := json.Marshal(p)
	return string(b), nil
}

// tailSession owns the temporary queue, rule and target. Close removes whatever was created.
type tailSession struct {
	eb       awsclient.EventBridgeAPI
	q        awsclient.SQSAPI
	bus      string
	name     string
	queueURL string
	ruleMade bool
	targetOn bool
}

func startTail(ctx context.Context, eb awsclient.EventBridgeAPI, q awsclient.SQSAPI, bus, pattern string) (*tailSession, error) {
	var idb [4]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, err
	}
	s := &tailSession{eb: eb, q: q, bus: bus, name: tailPrefix + hex.EncodeToString(idb[:])}
	if err := s.create(ctx, pattern); err != nil {
		s.Close() // don't leave half-built resources behind
		return nil, err
	}
	return s, nil
}

func (s *tailSession) create(ctx context.Context, pattern string) error {
	qOut, err := s.q.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(s.name),
		Tags:      map[string]string{"created-by": "edbg"},
	})
	if err != nil {
		return fmt.Errorf("create queue: %w", err)
	}
	s.queueURL = aws.ToString(qOut.QueueUrl)

	attrs, err := s.q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(s.queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return fmt.Errorf("get queue arn: %w", err)
	}
	queueARN := attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]

	rOut, err := s.eb.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:         aws.String(s.name),
		EventBusName: aws.String(s.bus),
		EventPattern: aws.String(pattern),
		State:        ebtypes.RuleStateEnabled,
		Tags:         []ebtypes.Tag{{Key: aws.String("created-by"), Value: aws.String("edbg")}},
	})
	if err != nil {
		return fmt.Errorf("put rule: %w", err)
	}
	s.ruleMade = true

	policy, _ := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]string{"Service": "events.amazonaws.com"},
			"Action":    "sqs:SendMessage",
			"Resource":  queueARN,
			"Condition": map[string]any{"ArnEquals": map[string]string{"aws:SourceArn": aws.ToString(rOut.RuleArn)}},
		}},
	})
	if _, err := s.q.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   aws.String(s.queueURL),
		Attributes: map[string]string{string(sqstypes.QueueAttributeNamePolicy): string(policy)},
	}); err != nil {
		return fmt.Errorf("set queue policy: %w", err)
	}

	if _, err := s.eb.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:         aws.String(s.name),
		EventBusName: aws.String(s.bus),
		Targets:      []ebtypes.Target{{Id: aws.String("edbg"), Arn: aws.String(queueARN)}},
	}); err != nil {
		return fmt.Errorf("put targets: %w", err)
	}
	s.targetOn = true
	return nil
}

// Close uses a fresh context because the run's context is already cancelled on quit.
func (s *tailSession) Close() {
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Println("Cleaning up...")
	if s.targetOn {
		if _, err := s.eb.RemoveTargets(cctx, &eventbridge.RemoveTargetsInput{
			Rule: aws.String(s.name), EventBusName: aws.String(s.bus), Ids: []string{"edbg"},
		}); err != nil {
			fmt.Printf("warning: remove targets: %v\n", err)
		}
	}
	if s.ruleMade {
		if _, err := s.eb.DeleteRule(cctx, &eventbridge.DeleteRuleInput{
			Name: aws.String(s.name), EventBusName: aws.String(s.bus),
		}); err != nil {
			fmt.Printf("warning: delete rule: %v\n", err)
		}
	}
	if s.queueURL != "" {
		if _, err := s.q.DeleteQueue(cctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(s.queueURL)}); err != nil {
			fmt.Printf("warning: delete queue: %v\n", err)
		}
	}
}

// Poll long-polls the queue until ctx is cancelled, passing each event to handle.
func (s *tailSession) Poll(ctx context.Context, handle func(tailEvent)) error {
	for ctx.Err() == nil {
		out, err := s.q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(s.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive: %w", err)
		}
		for _, m := range out.Messages {
			handle(parseTailEvent(aws.ToString(m.Body)))
			_, _ = s.q.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl: aws.String(s.queueURL), ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
	return nil
}

// openView builds the terminal view; in a real terminal it also enables single-key controls.
// Call restore before printing plain text, because raw mode must be off first.
func openView(cancel context.CancelFunc) (v *tailViewer, keys bool, restore func()) {
	tty := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	var w io.Writer = os.Stdout
	if tty {
		w = crlfWriter{os.Stdout}
	}
	v = newTailViewer(w, tailCompact, tty, termWidth())
	restore = func() {}
	if tty {
		restore, tty = enableKeys(cancel, v)
	}
	return v, tty, restore
}

func runTail(ctx context.Context, eb awsclient.EventBridgeAPI, q awsclient.SQSAPI, pattern string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s, err := startTail(ctx, eb, q, tailBus, pattern)
	if err != nil {
		return err
	}
	defer s.Close() // runs after the terminal is restored below

	if outputJSON() {
		return s.Poll(ctx, func(e tailEvent) { fmt.Println(e.jsonLine()) })
	}
	v, keys, restore := openView(cancel)
	defer restore()
	v.Header(tailBus, pattern, s.name, keys)
	err = s.Poll(ctx, v.Event)
	restore()
	fmt.Println("\n" + v.Summary())
	return err
}
