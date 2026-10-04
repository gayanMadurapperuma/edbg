package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

type fakeEB struct {
	awsclient.EventBridgeAPI
	calls []string
	rules []string
}

func (f *fakeEB) PutRule(_ context.Context, in *eventbridge.PutRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error) {
	f.calls = append(f.calls, "PutRule:"+aws.ToString(in.EventPattern))
	return &eventbridge.PutRuleOutput{RuleArn: aws.String("arn:aws:events:us-east-1:1:rule/" + aws.ToString(in.Name))}, nil
}
func (f *fakeEB) PutTargets(context.Context, *eventbridge.PutTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error) {
	f.calls = append(f.calls, "PutTargets")
	return &eventbridge.PutTargetsOutput{}, nil
}
func (f *fakeEB) RemoveTargets(context.Context, *eventbridge.RemoveTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error) {
	f.calls = append(f.calls, "RemoveTargets")
	return &eventbridge.RemoveTargetsOutput{}, nil
}
func (f *fakeEB) DeleteRule(context.Context, *eventbridge.DeleteRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error) {
	f.calls = append(f.calls, "DeleteRule")
	return &eventbridge.DeleteRuleOutput{}, nil
}
func (f *fakeEB) ListRules(context.Context, *eventbridge.ListRulesInput, ...func(*eventbridge.Options)) (*eventbridge.ListRulesOutput, error) {
	out := &eventbridge.ListRulesOutput{}
	for _, n := range f.rules {
		out.Rules = append(out.Rules, ebRule(n))
	}
	return out, nil
}

func TestBuildPattern(t *testing.T) {
	tests := []struct {
		name, raw, source, dt, want string
		cloudTrail, wantErr         bool
	}{
		{name: "raw passthrough", raw: `{"source":["a"]}`, want: `{"source":["a"]}`},
		{name: "raw invalid", raw: `{bad`, wantErr: true},
		{name: "source only", source: "my.app", want: `{"source":["my.app"]}`},
		{name: "both", source: "my.app", dt: "Order", want: `{"detail-type":["Order"],"source":["my.app"]}`},
		{name: "default skips cloudtrail", want: `{"detail-type":[{"anything-but":"AWS API Call via CloudTrail"}],"source":[{"prefix":""}]}`},
		{name: "default with cloudtrail", cloudTrail: true, want: `{"source":[{"prefix":""}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildPattern(tt.raw, tt.source, tt.dt, tt.cloudTrail)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// The tail must tear down everything it created, even when it stops via context cancellation.
func TestRunTailCreatesAndCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	eventBody := `{"time":"2024-01-01T00:00:00Z","source":"my.app","detail-type":"Order","detail":{"id":7}}`
	q := &fakeSQS{receive: func(*sqs.ReceiveMessageInput) *sqs.ReceiveMessageOutput {
		cancel() // simulate Ctrl+C after the first poll
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{{Body: aws.String(eventBody), ReceiptHandle: aws.String("rh")}}}
	}}
	eb := &fakeEB{}
	tailBus = "default"

	out := captureStdout(t, func() {
		if err := runTail(ctx, eb, q, `{"source":["my.app"]}`); err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(out, "my.app") || !strings.Contains(out, `"id": 7`) {
		t.Errorf("event not printed:\n%s", out)
	}
	if got := strings.Join(eb.calls, ","); !strings.HasSuffix(got, "PutTargets,RemoveTargets,DeleteRule") {
		t.Errorf("eventbridge calls = %s", got)
	}
	if len(q.created) != 1 || len(q.deleted) != 1 || !strings.HasSuffix(q.deleted[0], q.created[0]) {
		t.Errorf("queue created %v, deleted %v", q.created, q.deleted)
	}
	var pol struct {
		Statement []struct{ Condition map[string]map[string]string }
	}
	if err := json.Unmarshal([]byte(q.setPolicy), &pol); err != nil {
		t.Fatal(err)
	}
	if arn := pol.Statement[0].Condition["ArnEquals"]["aws:SourceArn"]; !strings.Contains(arn, ":rule/edbg-tail-") {
		t.Errorf("queue policy not scoped to the rule: %s", q.setPolicy)
	}
	if q.deletedMessages != 1 {
		t.Errorf("deleted messages = %d, want 1", q.deletedMessages)
	}
}

func TestRenderEventBlock(t *testing.T) {
	e := parseTailEvent(`{"id":"i-1","time":"2024-01-01T00:00:00Z","source":"my.app","detail-type":"Order","account":"123","region":"us-west-2","resources":["arn:x"],"detail":{"id":7}}`)
	out := renderEventBlock(3, e)
	for _, want := range []string{"#3", "my.app", "Order", "id i-1", "account 123", "region us-west-2", "resource arn:x", `"id": 7`, "└"} {
		if !strings.Contains(out, want) {
			t.Errorf("block missing %q:\n%s", want, out)
		}
	}
}

func TestRenderNonEnvelopeShowsRaw(t *testing.T) {
	out := renderEventBlock(1, parseTailEvent("not json"))
	if !strings.Contains(out, "raw message") || !strings.Contains(out, "not json") {
		t.Errorf("got %q", out)
	}
}

func TestRenderEventLineTruncates(t *testing.T) {
	e := parseTailEvent(`{"time":"2024-01-01T00:00:00Z","source":"s","detail-type":"d","detail":{"k":"` + strings.Repeat("x", 200) + `"}}`)
	line := strings.TrimSuffix(renderEventLine(e, 60), "\n")
	if n := len([]rune(line)); n > 60 || !strings.HasSuffix(line, "…") {
		t.Errorf("len %d, line %q", n, line)
	}
}

func TestViewerPauseBuffersAndResumes(t *testing.T) {
	var buf bytes.Buffer
	v := newTailViewer(&buf, true, false, 100)
	ev := func(dt string) tailEvent {
		return parseTailEvent(`{"source":"s","detail-type":"` + dt + `","detail":{}}`)
	}

	v.Event(ev("one"))
	v.TogglePause()
	v.Event(ev("two"))
	if strings.Contains(buf.String(), "two") {
		t.Fatal("event printed while paused")
	}
	v.TogglePause()
	out := buf.String()
	if !strings.Contains(out, "1 buffered") || !strings.Contains(out, "two") {
		t.Errorf("resume did not flush:\n%s", out)
	}
	if !strings.Contains(v.Summary(), "2 event(s)") {
		t.Errorf("summary: %s", v.Summary())
	}
}

func TestViewerPauseBufferIsBounded(t *testing.T) {
	v := newTailViewer(io.Discard, true, false, 100)
	v.TogglePause()
	for range maxBuffered + 5 {
		v.Event(parseTailEvent(`{"source":"s","detail-type":"d"}`))
	}
	if len(v.buf) != maxBuffered || v.dropped != 5 {
		t.Errorf("buf=%d dropped=%d", len(v.buf), v.dropped)
	}
}

func TestViewerClearOnlyOnTTY(t *testing.T) {
	var plain, tty bytes.Buffer
	newTailViewer(&plain, false, false, 80).Clear()
	newTailViewer(&tty, false, true, 80).Clear()
	if plain.Len() != 0 || !strings.Contains(tty.String(), "\x1b[2J") {
		t.Errorf("plain=%q tty=%q", plain.String(), tty.String())
	}
}

func TestCRLFWriter(t *testing.T) {
	var buf bytes.Buffer
	_, _ = crlfWriter{&buf}.Write([]byte("a\nb\n"))
	if buf.String() != "a\r\nb\r\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestRunCleanup(t *testing.T) {
	eb := &fakeEB{rules: []string{"edbg-tail-1"}}
	q := &fakeSQS{}
	out := captureStdout(t, func() {
		if err := runCleanup(context.Background(), eb, q, "default"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "deleted rule edbg-tail-1") || !strings.Contains(out, "deleted queue") {
		t.Errorf("got:\n%s", out)
	}
	if len(q.deleted) != 1 {
		t.Errorf("queues deleted = %v", q.deleted)
	}
}

func ebRule(name string) ebtypes.Rule { return ebtypes.Rule{Name: aws.String(name)} }
