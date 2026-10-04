package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/gayanMadurapperuma/edbg/internal/output"
	"github.com/spf13/cobra"
)

var (
	dlqMax     int
	dlqRedrive bool
)

var dlqCmd = &cobra.Command{
	Use:   "dlq-inspect <queue-url|queue-name>",
	Short: "Peek at DLQ messages without consuming them",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		client := awsclient.NewSQSClient(cfg)

		queueURL, err := resolveQueueURL(ctx, client, args[0])
		if err != nil {
			return err
		}
		if err := peekQueue(ctx, client, queueURL, dlqMax); err != nil {
			return err
		}
		if dlqRedrive {
			return redriveQueue(ctx, client, queueURL)
		}
		return nil
	},
}

func init() {
	dlqCmd.Flags().IntVar(&dlqMax, "max", 10, "maximum number of messages to show")
	dlqCmd.Flags().BoolVar(&dlqRedrive, "redrive", false, "move messages back to their source queue (asks for confirmation)")
	rootCmd.AddCommand(dlqCmd)
}

func resolveQueueURL(ctx context.Context, client awsclient.SQSAPI, nameOrURL string) (string, error) {
	if strings.HasPrefix(nameOrURL, "http://") || strings.HasPrefix(nameOrURL, "https://") {
		return nameOrURL, nil
	}
	out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(nameOrURL)})
	if err != nil {
		return "", fmt.Errorf("resolve queue %q: %w", nameOrURL, err)
	}
	return aws.ToString(out.QueueUrl), nil
}

// peekQueue receives with VisibilityTimeout=0 so messages stay available to other consumers.
// Receives can repeat messages, so it dedupes by MessageId and stops when a poll adds nothing new.
func peekQueue(ctx context.Context, client awsclient.SQSAPI, queueURL string, max int) error {
	seen := map[string]bool{}
	for len(seen) < max {
		n := max - len(seen)
		if n > 10 {
			n = 10
		}
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   int32(n),
			VisibilityTimeout:     0,
			WaitTimeSeconds:       2,
			AttributeNames:        []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			return fmt.Errorf("receive messages: %w", err)
		}
		added := 0
		for _, m := range out.Messages {
			id := aws.ToString(m.MessageId)
			if seen[id] {
				continue
			}
			seen[id] = true
			added++
			printDLQMessage(len(seen), m)
		}
		if added == 0 {
			break
		}
	}
	if outputJSON() {
		return nil
	}
	if len(seen) == 0 {
		fmt.Println("No messages found.")
	} else {
		fmt.Printf("\n%d message(s) shown (not consumed).\n", len(seen))
	}
	return nil
}

func printDLQMessage(i int, m sqstypes.Message) {
	if outputJSON() {
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
		return
	}
	fmt.Println(output.Cyan(fmt.Sprintf("--- #%d  id=%s", i, aws.ToString(m.MessageId))))
	keys := make([]string, 0, len(m.Attributes))
	for k := range m.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s: %s\n", output.Dim(k), m.Attributes[k])
	}
	for k, v := range m.MessageAttributes {
		fmt.Printf("  attr.%s: %s\n", k, aws.ToString(v.StringValue))
	}
	fmt.Println(output.PrettyJSON(aws.ToString(m.Body)))
}

func redriveQueue(ctx context.Context, client awsclient.SQSAPI, queueURL string) error {
	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return fmt.Errorf("get queue arn: %w", err)
	}
	arn := attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]

	fmt.Printf("\nRedrive ALL messages from %s back to their source queue? [y/N] ", arn)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.ToLower(strings.TrimSpace(line)) != "y" {
		fmt.Println("Aborted.")
		return nil
	}
	out, err := client.StartMessageMoveTask(ctx, &sqs.StartMessageMoveTaskInput{SourceArn: aws.String(arn)})
	if err != nil {
		return fmt.Errorf("start redrive: %w", err)
	}
	fmt.Printf("Redrive started (task handle: %s)\n", aws.ToString(out.TaskHandle))
	return nil
}
