package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

type fakeSQS struct {
	awsclient.SQSAPI // unimplemented methods panic if called
	receive          func(*sqs.ReceiveMessageInput) *sqs.ReceiveMessageOutput
	receiveInputs    []*sqs.ReceiveMessageInput
	queueURLByName   map[string]string
	deleted          []string
	created          []string
	deletedMessages  int
	setPolicy        string
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.receiveInputs = append(f.receiveInputs, in)
	return f.receive(in), nil
}

func (f *fakeSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String(f.queueURLByName[aws.ToString(in.QueueName)])}, nil
}

func (f *fakeSQS) CreateQueue(_ context.Context, in *sqs.CreateQueueInput, _ ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	f.created = append(f.created, aws.ToString(in.QueueName))
	return &sqs.CreateQueueOutput{QueueUrl: aws.String("https://sqs/q/" + aws.ToString(in.QueueName))}, nil
}

func (f *fakeSQS) GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{"QueueArn": "arn:aws:sqs:us-east-1:1:q"}}, nil
}

func (f *fakeSQS) SetQueueAttributes(_ context.Context, in *sqs.SetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.SetQueueAttributesOutput, error) {
	f.setPolicy = in.Attributes["Policy"]
	return &sqs.SetQueueAttributesOutput{}, nil
}

func (f *fakeSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deletedMessages++
	return &sqs.DeleteMessageOutput{}, nil
}

func (f *fakeSQS) DeleteQueue(_ context.Context, in *sqs.DeleteQueueInput, _ ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(in.QueueUrl))
	return &sqs.DeleteQueueOutput{}, nil
}

func (f *fakeSQS) ListQueues(context.Context, *sqs.ListQueuesInput, ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	return &sqs.ListQueuesOutput{QueueUrls: []string{"https://sqs/q/edbg-tail-1"}}, nil
}

func msg(id, body string) sqstypes.Message {
	return sqstypes.Message{MessageId: aws.String(id), Body: aws.String(body), Attributes: map[string]string{"ApproximateReceiveCount": "3"}}
}

func TestPeekQueueDedupesAndNeverConsumes(t *testing.T) {
	calls := 0
	f := &fakeSQS{receive: func(*sqs.ReceiveMessageInput) *sqs.ReceiveMessageOutput {
		calls++
		if calls == 1 {
			return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{msg("a", `{"x":1}`), msg("b", "plain")}}
		}
		// SQS may hand back the same messages again when visibility timeout is 0.
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{msg("a", `{"x":1}`)}}
	}}

	out := captureStdout(t, func() {
		if err := peekQueue(context.Background(), f, "https://q", 10); err != nil {
			t.Fatal(err)
		}
	})

	if strings.Count(out, "id=a") != 1 || strings.Count(out, "id=b") != 1 {
		t.Errorf("messages not deduped:\n%s", out)
	}
	if !strings.Contains(out, "2 message(s) shown") {
		t.Errorf("missing summary:\n%s", out)
	}
	for _, in := range f.receiveInputs {
		if in.VisibilityTimeout != 0 {
			t.Errorf("VisibilityTimeout = %d, want 0", in.VisibilityTimeout)
		}
	}
	if f.deletedMessages != 0 {
		t.Error("dlq-inspect must not delete messages")
	}
}

func TestPeekQueueEmpty(t *testing.T) {
	f := &fakeSQS{receive: func(*sqs.ReceiveMessageInput) *sqs.ReceiveMessageOutput { return &sqs.ReceiveMessageOutput{} }}
	out := captureStdout(t, func() { _ = peekQueue(context.Background(), f, "https://q", 5) })
	if !strings.Contains(out, "No messages found.") {
		t.Errorf("got %q", out)
	}
}

func TestPeekQueueRespectsMax(t *testing.T) {
	n := 0
	f := &fakeSQS{receive: func(in *sqs.ReceiveMessageInput) *sqs.ReceiveMessageOutput {
		var ms []sqstypes.Message
		for i := int32(0); i < in.MaxNumberOfMessages; i++ {
			n++
			ms = append(ms, msg(string(rune('a'+n)), "{}"))
		}
		return &sqs.ReceiveMessageOutput{Messages: ms}
	}}
	out := captureStdout(t, func() { _ = peekQueue(context.Background(), f, "https://q", 3) })
	if !strings.Contains(out, "3 message(s) shown") {
		t.Errorf("got %q", out)
	}
}

func TestResolveQueueURL(t *testing.T) {
	f := &fakeSQS{queueURLByName: map[string]string{"orders-dlq": "https://sqs/orders-dlq"}}
	for in, want := range map[string]string{
		"https://already/a/url": "https://already/a/url",
		"orders-dlq":            "https://sqs/orders-dlq",
	} {
		got, err := resolveQueueURL(context.Background(), f, in)
		if err != nil || got != want {
			t.Errorf("resolveQueueURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
