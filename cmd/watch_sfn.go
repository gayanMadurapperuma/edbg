package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

type machine struct{ Name, Arn string }

// sfnSource follows Step Functions by polling executions, so it sees runs started by any trigger
// (SQS, EventBridge, API, schedule, console). Express workflows have no execution list and are not covered.
type sfnSource struct {
	api      awsclient.SFNAPI
	machines []machine
	since    time.Time
	interval time.Duration
	known    map[string]sfntypes.ExecutionStatus
}

func (s *sfnSource) Describe() string {
	return fmt.Sprintf("%d state machine(s)", len(s.machines))
}

func (s *sfnSource) Run(ctx context.Context, emit func(tailEvent)) error {
	return runEvery(ctx, s.interval, func() error { return s.pollOnce(ctx, emit) })
}

func (s *sfnSource) pollOnce(ctx context.Context, emit func(tailEvent)) error {
	if s.known == nil {
		s.known = map[string]sfntypes.ExecutionStatus{}
	}
	var firstErr error
	for _, m := range s.machines {
		out, err := s.api.ListExecutions(ctx, &sfn.ListExecutionsInput{StateMachineArn: aws.String(m.Arn), MaxResults: 50})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list executions %s: %w", m.Name, err)
			}
			continue
		}
		present := map[string]bool{}
		for i := len(out.Executions) - 1; i >= 0; i-- { // oldest first, so the feed reads in order
			ex := out.Executions[i]
			arn := aws.ToString(ex.ExecutionArn)
			present[arn] = true
			prev, seen := s.known[arn]
			s.known[arn] = ex.Status
			switch {
			case !seen && aws.ToTime(ex.StartDate).Before(s.since):
				// started before we began watching: remember it so its completion is reported
			case !seen:
				emit(s.item(m, ex.StartDate, "", "▶ started "+execName(arn)))
				if ex.Status != sfntypes.ExecutionStatusRunning {
					s.finish(ctx, m, ex, emit)
				}
			case prev != ex.Status && ex.Status != sfntypes.ExecutionStatusRunning:
				s.finish(ctx, m, ex, emit)
			}
		}
		// forget executions that dropped out of the newest-50 window so the map stays bounded
		prefix := strings.Replace(m.Arn, ":stateMachine:", ":execution:", 1) + ":"
		for arn := range s.known {
			if strings.HasPrefix(arn, prefix) && !present[arn] {
				delete(s.known, arn)
			}
		}
	}
	return firstErr
}

func execName(arn string) string { return arn[strings.LastIndex(arn, ":")+1:] }

// item stamps the feed line with the execution's own time, so a --since replay shows when things happened.
func (s *sfnSource) item(m machine, at *time.Time, level, text string) tailEvent {
	t := time.Now()
	if at != nil {
		t = *at
	}
	return tailEvent{Kind: "sfn", Source: m.Name, Time: t.UTC().Format(time.RFC3339), Level: level, Text: text}
}

func (s *sfnSource) finish(ctx context.Context, m machine, ex sfntypes.ExecutionListItem, emit func(tailEvent)) {
	arn := aws.ToString(ex.ExecutionArn)
	took := ""
	if ex.StopDate != nil && ex.StartDate != nil {
		took = " (" + ex.StopDate.Sub(*ex.StartDate).Round(time.Millisecond).String() + ")"
	}
	if ex.Status == sfntypes.ExecutionStatusSucceeded {
		emit(s.item(m, ex.StopDate, "ok", "✔ SUCCEEDED "+execName(arn)+took))
		return
	}
	text := fmt.Sprintf("✖ %s %s%s", ex.Status, execName(arn), took)
	if d := failureDetail(ctx, s.api, arn); d != "" {
		text += "\n" + d
	}
	emit(s.item(m, ex.StopDate, "error", text))
}

// failureDetail returns "Error: cause" from the end of an execution's history, or "" if none is found.
func failureDetail(ctx context.Context, api awsclient.SFNAPI, arn string) string {
	out, err := api.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{
		ExecutionArn: aws.String(arn), ReverseOrder: true, MaxResults: 20,
	})
	if err != nil {
		return ""
	}
	for _, e := range out.Events {
		var errName, cause *string
		switch {
		case e.ExecutionFailedEventDetails != nil:
			errName, cause = e.ExecutionFailedEventDetails.Error, e.ExecutionFailedEventDetails.Cause
		case e.ExecutionTimedOutEventDetails != nil:
			errName, cause = e.ExecutionTimedOutEventDetails.Error, e.ExecutionTimedOutEventDetails.Cause
		case e.ExecutionAbortedEventDetails != nil:
			errName, cause = e.ExecutionAbortedEventDetails.Error, e.ExecutionAbortedEventDetails.Cause
		case e.TaskFailedEventDetails != nil:
			errName, cause = e.TaskFailedEventDetails.Error, e.TaskFailedEventDetails.Cause
		case e.LambdaFunctionFailedEventDetails != nil:
			errName, cause = e.LambdaFunctionFailedEventDetails.Error, e.LambdaFunctionFailedEventDetails.Cause
		default:
			continue
		}
		return aws.ToString(errName) + ": " + aws.ToString(cause)
	}
	return ""
}
