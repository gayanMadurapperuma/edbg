package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/gayanMadurapperuma/edbg/internal/output"
	"github.com/spf13/cobra"
)

var (
	traceFailedOnly bool
	traceFollow     bool
)

var traceSfnCmd = &cobra.Command{
	Use:   "trace-sfn <execution-arn>",
	Short: "Show a timeline of a Step Functions execution",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		client := awsclient.NewSFNClient(cfg)
		return traceExecution(ctx, client, args[0])
	},
}

func init() {
	traceSfnCmd.Flags().BoolVar(&traceFailedOnly, "failed-only", false, "show only failed/timed-out/aborted events")
	traceSfnCmd.Flags().BoolVar(&traceFollow, "follow", false, "keep polling until the execution finishes")
	rootCmd.AddCommand(traceSfnCmd)
}

func traceExecution(ctx context.Context, client awsclient.SFNAPI, arn string) error {
	var lastID int64
	var start time.Time
	for {
		events, err := fetchHistory(ctx, client, arn)
		if err != nil {
			return err
		}
		if len(events) > 0 && start.IsZero() {
			start = aws.ToTime(events[0].Timestamp)
		}
		for _, e := range events {
			if e.Id <= lastID {
				continue
			}
			lastID = e.Id
			if traceFailedOnly && !isFailureEvent(e.Type) {
				continue
			}
			printSFNEvent(e, start)
		}

		desc, err := client.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: aws.String(arn)})
		if err != nil {
			return fmt.Errorf("describe execution: %w", err)
		}
		if !traceFollow || desc.Status != sfntypes.ExecutionStatusRunning {
			if !outputJSON() {
				fmt.Printf("\nStatus: %s\n", colorStatus(desc.Status))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func fetchHistory(ctx context.Context, client awsclient.SFNAPI, arn string) ([]sfntypes.HistoryEvent, error) {
	var all []sfntypes.HistoryEvent
	p := sfn.NewGetExecutionHistoryPaginator(client, &sfn.GetExecutionHistoryInput{ExecutionArn: aws.String(arn)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("get execution history: %w", err)
		}
		all = append(all, page.Events...)
	}
	return all, nil
}

func isFailureEvent(t sfntypes.HistoryEventType) bool {
	s := string(t)
	return strings.HasSuffix(s, "Failed") || strings.HasSuffix(s, "TimedOut") || strings.HasSuffix(s, "Aborted")
}

func colorStatus(s sfntypes.ExecutionStatus) string {
	switch s {
	case sfntypes.ExecutionStatusSucceeded:
		return output.Green(string(s))
	case sfntypes.ExecutionStatusRunning:
		return output.Yellow(string(s))
	}
	return output.Red(string(s))
}

func printSFNEvent(e sfntypes.HistoryEvent, start time.Time) {
	if outputJSON() {
		b, _ := json.Marshal(e)
		fmt.Println(string(b))
		return
	}
	offset := aws.ToTime(e.Timestamp).Sub(start).Round(time.Millisecond)
	typ := string(e.Type)
	if isFailureEvent(e.Type) {
		typ = output.Red(typ)
	}
	line := fmt.Sprintf("%4d  +%-10s %s", e.Id, offset, typ)

	switch {
	case e.StateEnteredEventDetails != nil:
		line += "  state=" + aws.ToString(e.StateEnteredEventDetails.Name)
	case e.StateExitedEventDetails != nil:
		line += "  state=" + aws.ToString(e.StateExitedEventDetails.Name)
	}
	fmt.Println(line)

	if d := e.ExecutionFailedEventDetails; d != nil {
		fmt.Printf("        error=%s cause=%s\n", aws.ToString(d.Error), aws.ToString(d.Cause))
	}
	if d := e.TaskFailedEventDetails; d != nil {
		fmt.Printf("        error=%s cause=%s\n", aws.ToString(d.Error), aws.ToString(d.Cause))
	}
	if d := e.LambdaFunctionFailedEventDetails; d != nil {
		fmt.Printf("        error=%s cause=%s\n", aws.ToString(d.Error), aws.ToString(d.Cause))
	}
}
