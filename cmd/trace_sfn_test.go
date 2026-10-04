package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

type fakeSFN struct {
	awsclient.SFNAPI
	pages  [][]sfntypes.HistoryEvent
	status sfntypes.ExecutionStatus
}

func (f *fakeSFN) GetExecutionHistory(_ context.Context, in *sfn.GetExecutionHistoryInput, _ ...func(*sfn.Options)) (*sfn.GetExecutionHistoryOutput, error) {
	i := 0
	if in.NextToken != nil {
		i = int((*in.NextToken)[0] - '0')
	}
	out := &sfn.GetExecutionHistoryOutput{Events: f.pages[i]}
	if i+1 < len(f.pages) {
		out.NextToken = aws.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

func (f *fakeSFN) DescribeExecution(context.Context, *sfn.DescribeExecutionInput, ...func(*sfn.Options)) (*sfn.DescribeExecutionOutput, error) {
	return &sfn.DescribeExecutionOutput{Status: f.status}, nil
}

func ev(id int64, typ sfntypes.HistoryEventType, sec int) sfntypes.HistoryEvent {
	return sfntypes.HistoryEvent{Id: id, Type: typ, Timestamp: aws.Time(time.Unix(1700000000+int64(sec), 0))}
}

func historyFixture() *fakeSFN {
	entered := ev(2, sfntypes.HistoryEventTypeTaskStateEntered, 1)
	entered.StateEnteredEventDetails = &sfntypes.StateEnteredEventDetails{Name: aws.String("Charge")}
	failed := ev(3, sfntypes.HistoryEventTypeTaskFailed, 2)
	failed.TaskFailedEventDetails = &sfntypes.TaskFailedEventDetails{Error: aws.String("Card.Declined"), Cause: aws.String("insufficient funds")}
	return &fakeSFN{
		pages:  [][]sfntypes.HistoryEvent{{ev(1, sfntypes.HistoryEventTypeExecutionStarted, 0), entered}, {failed}},
		status: sfntypes.ExecutionStatusFailed,
	}
}

func TestTraceExecutionTimeline(t *testing.T) {
	out := captureStdout(t, func() {
		if err := traceExecution(context.Background(), historyFixture(), "arn"); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"ExecutionStarted", "state=Charge", "+2s", "error=Card.Declined cause=insufficient funds", "Status: FAILED"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTraceExecutionFailedOnly(t *testing.T) {
	traceFailedOnly = true
	defer func() { traceFailedOnly = false }()
	out := captureStdout(t, func() { _ = traceExecution(context.Background(), historyFixture(), "arn") })
	if strings.Contains(out, "ExecutionStarted") || !strings.Contains(out, "TaskFailed") {
		t.Errorf("failed-only filter wrong:\n%s", out)
	}
}

func TestIsFailureEvent(t *testing.T) {
	for typ, want := range map[sfntypes.HistoryEventType]bool{
		sfntypes.HistoryEventTypeTaskFailed:        true,
		sfntypes.HistoryEventTypeExecutionTimedOut: true,
		sfntypes.HistoryEventTypeExecutionAborted:  true,
		sfntypes.HistoryEventTypeTaskSucceeded:     false,
		sfntypes.HistoryEventTypePassStateEntered:  false,
	} {
		if got := isFailureEvent(typ); got != want {
			t.Errorf("isFailureEvent(%s) = %v, want %v", typ, got, want)
		}
	}
}
