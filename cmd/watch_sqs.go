package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

type queueRef struct{ Name, URL string }

type depth struct{ visible, inflight, delayed int }

// sqsSource follows queue depth. It never receives messages, so it cannot disturb consumers.
type sqsSource struct {
	api      awsclient.SQSAPI
	queues   []queueRef
	interval time.Duration
	last     map[string]depth
}

func (s *sqsSource) Describe() string { return fmt.Sprintf("%d queue(s)", len(s.queues)) }

func (s *sqsSource) Run(ctx context.Context, emit func(tailEvent)) error {
	return runEvery(ctx, s.interval, func() error { return s.pollOnce(ctx, emit) })
}

func isDLQ(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "dlq") || strings.Contains(n, "dead")
}

func (s *sqsSource) pollOnce(ctx context.Context, emit func(tailEvent)) error {
	if s.last == nil {
		s.last = map[string]depth{}
	}
	var firstErr error
	for _, q := range s.queues {
		out, err := s.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(q.URL),
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
			},
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("get queue attributes %s: %w", q.Name, err)
			}
			continue
		}
		atoi := func(n sqstypes.QueueAttributeName) int { v, _ := strconv.Atoi(out.Attributes[string(n)]); return v }
		cur := depth{
			atoi(sqstypes.QueueAttributeNameApproximateNumberOfMessages),
			atoi(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible),
			atoi(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed),
		}
		prev, seen := s.last[q.URL]
		s.last[q.URL] = cur
		ev := tailEvent{Kind: "sqs", Source: q.Name, Time: time.Now().UTC().Format(time.RFC3339)}
		switch {
		case !seen:
			ev.Text = fmt.Sprintf("watching · visible %d · in-flight %d · delayed %d", cur.visible, cur.inflight, cur.delayed)
			if isDLQ(q.Name) && cur.visible > 0 {
				ev.Level = "warn"
			}
		case cur != prev:
			ev.Text = fmt.Sprintf("visible %d (%+d) · in-flight %d · delayed %d", cur.visible, cur.visible-prev.visible, cur.inflight, cur.delayed)
			if isDLQ(q.Name) && cur.visible > prev.visible {
				ev.Level = "error"
				ev.Text = "DLQ received messages · " + ev.Text
			}
		default:
			continue
		}
		emit(ev)
	}
	return firstErr
}
