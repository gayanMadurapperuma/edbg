package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

func collect() (func(tailEvent), func() []tailEvent) {
	var mu sync.Mutex
	var got []tailEvent
	return func(e tailEvent) { mu.Lock(); got = append(got, e); mu.Unlock() },
		func() []tailEvent { mu.Lock(); defer mu.Unlock(); return append([]tailEvent(nil), got...) }
}

func TestGlob(t *testing.T) {
	for _, tt := range []struct {
		pat, s string
		want   bool
	}{
		{"orders-*", "orders-api", true},
		{"orders-*", "payments-api", false},
		{"*", "anything/at/all", true},
		{"a?c", "abc", true},
		{"a?c", "abbc", false},
		{"exact", "exact", true},
		{"exact", "exact2", false},
		{"a.b", "axb", false}, // dots are literal
	} {
		if got := globMatch(tt.pat, tt.s); got != tt.want {
			t.Errorf("globMatch(%q,%q) = %v", tt.pat, tt.s, got)
		}
	}
	if longestLiteral("*ab*cdef?g*") != "cdef" || longestLiteral("*") != "" {
		t.Error("longestLiteral wrong")
	}
	if literalPrefix("orders-*-dlq") != "orders-" || literalPrefix("plain") != "plain" || literalPrefix("*x") != "" {
		t.Error("literalPrefix wrong")
	}
}

func TestClassifyLog(t *testing.T) {
	report := "REPORT RequestId: abc\tDuration: 123.45 ms\tBilled Duration: 124 ms\tMemory Size: 128 MB\tMax Memory Used: 64 MB\tInit Duration: 233.10 ms"
	for _, tt := range []struct {
		msg, level, contains string
	}{
		{"START RequestId: abc Version: $LATEST", "dim", "START"},
		{"END RequestId: abc", "dim", "END"},
		{report, "ok", "done 123.45 ms · 64 MB · cold start 233.10 ms"},
		{report + "\tStatus: timeout", "error", "timeout"},
		{"2024 ERROR something broke", "error", "broke"},
		{`{"level":"error","msg":"x"}`, "error", "x"},
		{"Task timed out after 3.00 seconds", "error", "timed out"},
		{"WARN slow query", "warn", "slow"},
		{"hello world", "", "hello"},
		{`{"errorCount":0}`, "", "errorCount"}, // must not false-positive on field names
	} {
		level, text := classifyLog(tt.msg)
		if level != tt.level || !strings.Contains(text, tt.contains) {
			t.Errorf("classifyLog(%q) = %q,%q; want level %q containing %q", tt.msg, level, text, tt.level, tt.contains)
		}
	}
}

// ---- logs

type fakeLogs struct {
	awsclient.LogsAPI
	mu       sync.Mutex
	groups   []string
	events   map[string][]cwtypes.FilteredLogEvent
	starts   []int64
	prefixes []*string
	patterns []*string
}

func (f *fakeLogs) DescribeLogGroups(_ context.Context, in *cloudwatchlogs.DescribeLogGroupsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefixes = append(f.prefixes, in.LogGroupNamePrefix)
	f.patterns = append(f.patterns, in.LogGroupNamePattern)
	out := &cloudwatchlogs.DescribeLogGroupsOutput{}
	for _, g := range f.groups {
		if (in.LogGroupNamePrefix == nil || strings.HasPrefix(g, *in.LogGroupNamePrefix)) &&
			(in.LogGroupNamePattern == nil || strings.Contains(g, *in.LogGroupNamePattern)) {
			out.LogGroups = append(out.LogGroups, cwtypes.LogGroup{LogGroupName: aws.String(g)})
		}
	}
	return out, nil
}

// FilterLogEvents honors StartTime like the real API, so overlapping polls return old events again.
func (f *fakeLogs) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, aws.ToInt64(in.StartTime))
	out := &cloudwatchlogs.FilterLogEventsOutput{}
	for _, e := range f.events[aws.ToString(in.LogGroupName)] {
		if aws.ToInt64(e.Timestamp) >= aws.ToInt64(in.StartTime) {
			out.Events = append(out.Events, e)
		}
	}
	return out, nil
}

func logEv(id string, ts time.Time, msg string) cwtypes.FilteredLogEvent {
	return cwtypes.FilteredLogEvent{EventId: aws.String(id), Timestamp: aws.Int64(ts.UnixMilli()), Message: aws.String(msg)}
}

func TestLogsSourceDedupesOverlapAndFiltersNoise(t *testing.T) {
	now := time.Now()
	g := "/aws/lambda/orders-api"
	f := &fakeLogs{events: map[string][]cwtypes.FilteredLogEvent{g: {
		logEv("1", now.Add(time.Second), "START RequestId: r1"),
		logEv("2", now.Add(2*time.Second), "ERROR boom"),
		logEv("3", now.Add(3*time.Second), "REPORT RequestId: r1\tDuration: 10.00 ms\tMax Memory Used: 50 MB"),
	}}}
	s := &logsSource{api: f, kind: "lambda", groups: []string{g}, trim: "/aws/lambda/", since: now.Add(-time.Minute)}
	emit, got := collect()

	_ = s.pollOnce(context.Background(), emit)
	_ = s.pollOnce(context.Background(), emit) // same events come back because of the overlap window
	if evs := got(); len(evs) != 2 {
		t.Fatalf("want 2 events (START hidden, no duplicates), got %d: %+v", len(evs), evs)
	}
	if e := got()[0]; e.Kind != "lambda" || e.Source != "orders-api" || e.Level != "error" || e.Text != "ERROR boom" {
		t.Errorf("first event = %+v", e)
	}
	if e := got()[1]; e.Level != "ok" || !strings.Contains(e.Text, "done 10.00 ms") {
		t.Errorf("report event = %+v", e)
	}

	// a late-arriving event inside the overlap window must still be picked up
	f.events[g] = append(f.events[g], logEv("4", now.Add(2500*time.Millisecond), "late line"))
	_ = s.pollOnce(context.Background(), emit)
	if evs := got(); len(evs) != 3 || evs[2].Text != "late line" {
		t.Errorf("late event not delivered: %+v", evs)
	}
	if want := now.Add(3*time.Second).UnixMilli() - logOverlap.Milliseconds(); f.starts[1] != want {
		t.Errorf("second poll StartTime = %d, want %d", f.starts[1], want)
	}
}

func TestLogsSourceVerboseShowsStartEnd(t *testing.T) {
	now := time.Now()
	f := &fakeLogs{events: map[string][]cwtypes.FilteredLogEvent{"g": {logEv("1", now.Add(time.Second), "START RequestId: r1")}}}
	s := &logsSource{api: f, kind: "lambda", groups: []string{"g"}, since: now, verbose: true}
	emit, got := collect()
	_ = s.pollOnce(context.Background(), emit)
	if len(got()) != 1 {
		t.Errorf("verbose should show START, got %+v", got())
	}
}

func TestLogsSourceIgnoresHistoryBeforeSince(t *testing.T) {
	now := time.Now()
	f := &fakeLogs{events: map[string][]cwtypes.FilteredLogEvent{"g": {logEv("old", now.Add(-time.Hour), "ancient")}}}
	s := &logsSource{api: f, kind: "logs", groups: []string{"g"}, since: now}
	emit, got := collect()
	_ = s.pollOnce(context.Background(), emit)
	if len(got()) != 0 {
		t.Errorf("old event leaked: %+v", got())
	}
}

func TestResolveLogGroups(t *testing.T) {
	f := &fakeLogs{groups: []string{"/aws/lambda/orders-api", "/aws/lambda/orders-worker", "/aws/lambda/billing", "/aws/ecs/orders"}}
	got, err := resolveLogGroups(context.Background(), f, "/aws/lambda/", []string{"orders-*", "orders-api"})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v (duplicates must collapse)", got, err)
	}
	if aws.ToString(f.prefixes[0]) != "/aws/lambda/orders-" {
		t.Errorf("server-side prefix = %q", aws.ToString(f.prefixes[0]))
	}
	all, _ := resolveLogGroups(context.Background(), f, "", []string{"/aws/ecs/*"})
	if len(all) != 1 || all[0] != "/aws/ecs/orders" {
		t.Errorf("--logs resolution = %v", all)
	}
}

func TestResolveLogGroupsLeadingWildcardUsesServerSideFilter(t *testing.T) {
	f := &fakeLogs{groups: []string{"/aws/lambda/stack-cronUpdater-X", "/aws/lambda/stack-sender-Y", "/aws/ecs/cronUpdater"}}
	got, err := resolveLogGroups(context.Background(), f, "/aws/lambda/", []string{"*cronUpdater*"})
	if err != nil || len(got) != 1 || got[0] != "/aws/lambda/stack-cronUpdater-X" {
		t.Fatalf("got %v, %v (the ECS group must not match a Lambda pattern)", got, err)
	}
	if f.prefixes[0] != nil || aws.ToString(f.patterns[0]) != "cronUpdater" {
		t.Errorf("want substring filter without a prefix, got prefix=%v pattern=%v", f.prefixes[0], f.patterns[0])
	}
}

// ---- step functions

type fakeWatchSFN struct {
	awsclient.SFNAPI
	machines []sfntypes.StateMachineListItem
	execs    map[string][]sfntypes.ExecutionListItem // newest first, like the real API
	history  []sfntypes.HistoryEvent
}

func (f *fakeWatchSFN) ListStateMachines(context.Context, *sfn.ListStateMachinesInput, ...func(*sfn.Options)) (*sfn.ListStateMachinesOutput, error) {
	return &sfn.ListStateMachinesOutput{StateMachines: f.machines}, nil
}
func (f *fakeWatchSFN) ListExecutions(_ context.Context, in *sfn.ListExecutionsInput, _ ...func(*sfn.Options)) (*sfn.ListExecutionsOutput, error) {
	return &sfn.ListExecutionsOutput{Executions: f.execs[aws.ToString(in.StateMachineArn)]}, nil
}
func (f *fakeWatchSFN) GetExecutionHistory(context.Context, *sfn.GetExecutionHistoryInput, ...func(*sfn.Options)) (*sfn.GetExecutionHistoryOutput, error) {
	return &sfn.GetExecutionHistoryOutput{Events: f.history}, nil
}

const machineARN = "arn:aws:states:us-west-2:1:stateMachine:OrderFlow"

func exec(name string, status sfntypes.ExecutionStatus, start time.Time, stop *time.Time) sfntypes.ExecutionListItem {
	return sfntypes.ExecutionListItem{
		ExecutionArn: aws.String("arn:aws:states:us-west-2:1:execution:OrderFlow:" + name),
		Status:       status, StartDate: aws.Time(start), StopDate: stop,
	}
}

func TestSFNSourceReportsStartsAndFinishesFromAnyTrigger(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	f := &fakeWatchSFN{execs: map[string][]sfntypes.ExecutionListItem{machineARN: {
		exec("running-before", sfntypes.ExecutionStatusRunning, old, nil),
		exec("done-before", sfntypes.ExecutionStatusSucceeded, old, aws.Time(old.Add(time.Second))),
	}}}
	s := &sfnSource{api: f, machines: []machine{{"OrderFlow", machineARN}}, since: now}
	emit, got := collect()

	_ = s.pollOnce(context.Background(), emit)
	if len(got()) != 0 {
		t.Fatalf("pre-existing executions must be silent, got %+v", got())
	}

	// the earlier run finishes and a brand-new one starts (triggered by SQS, EventBridge... we cannot tell and do not need to)
	f.execs[machineARN] = []sfntypes.ExecutionListItem{
		exec("new-1", sfntypes.ExecutionStatusRunning, now.Add(time.Second), nil),
		exec("running-before", sfntypes.ExecutionStatusSucceeded, old, aws.Time(now.Add(2*time.Second))),
		exec("done-before", sfntypes.ExecutionStatusSucceeded, old, aws.Time(old.Add(time.Second))),
	}
	_ = s.pollOnce(context.Background(), emit)
	evs := got()
	if len(evs) != 2 || !strings.Contains(evs[0].Text, "SUCCEEDED running-before") || !strings.Contains(evs[1].Text, "started new-1") {
		t.Fatalf("poll 2 events: %+v", evs)
	}
	if evs[0].Level != "ok" || evs[0].Kind != "sfn" || evs[0].Source != "OrderFlow" {
		t.Errorf("event fields: %+v", evs[0])
	}

	// the new run fails: the cause is pulled from its history
	f.history = []sfntypes.HistoryEvent{{TaskFailedEventDetails: &sfntypes.TaskFailedEventDetails{Error: aws.String("Card.Declined"), Cause: aws.String("insufficient funds")}}}
	f.execs[machineARN][0] = exec("new-1", sfntypes.ExecutionStatusFailed, now.Add(time.Second), aws.Time(now.Add(4*time.Second)))
	_ = s.pollOnce(context.Background(), emit)
	last := got()[2]
	if last.Level != "error" || !strings.Contains(last.Text, "FAILED new-1") || !strings.Contains(last.Text, "Card.Declined: insufficient funds") {
		t.Errorf("failure event: %+v", last)
	}

	_ = s.pollOnce(context.Background(), emit)
	if len(got()) != 3 {
		t.Errorf("unchanged state must not re-emit, got %d", len(got()))
	}
}

func TestSFNSourceSinceReplaysRecentExecutions(t *testing.T) {
	now := time.Now()
	f := &fakeWatchSFN{execs: map[string][]sfntypes.ExecutionListItem{machineARN: {
		exec("recent", sfntypes.ExecutionStatusSucceeded, now.Add(-time.Minute), aws.Time(now.Add(-50*time.Second))),
	}}}
	s := &sfnSource{api: f, machines: []machine{{"OrderFlow", machineARN}}, since: now.Add(-10 * time.Minute)}
	emit, got := collect()
	_ = s.pollOnce(context.Background(), emit)
	evs := got()
	if len(evs) != 2 || !strings.Contains(evs[0].Text, "started") || !strings.Contains(evs[1].Text, "SUCCEEDED") {
		t.Fatalf("--since should replay start+finish: %+v", evs)
	}
	wantStart := now.Add(-time.Minute).UTC().Format(time.RFC3339)
	if evs[0].Time != wantStart {
		t.Errorf("replayed start time = %s, want the execution's own %s", evs[0].Time, wantStart)
	}
}

func TestResolveMachinesSkipsExpress(t *testing.T) {
	f := &fakeWatchSFN{machines: []sfntypes.StateMachineListItem{
		{Name: aws.String("OrderFlow"), StateMachineArn: aws.String(machineARN), Type: sfntypes.StateMachineTypeStandard},
		{Name: aws.String("OrderFast"), StateMachineArn: aws.String("arn:x"), Type: sfntypes.StateMachineTypeExpress},
		{Name: aws.String("Other"), StateMachineArn: aws.String("arn:y"), Type: sfntypes.StateMachineTypeStandard},
	}}
	ms, notes, err := resolveMachines(context.Background(), f, []string{"Order*"})
	if err != nil || len(ms) != 1 || ms[0].Name != "OrderFlow" {
		t.Fatalf("machines %+v, err %v", ms, err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "Express") {
		t.Errorf("notes = %v", notes)
	}
}

// ---- sqs

type fakeDepthSQS struct {
	awsclient.SQSAPI
	attrs map[string]map[string]string
	urls  []string
}

func (f *fakeDepthSQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	return &sqs.GetQueueAttributesOutput{Attributes: f.attrs[aws.ToString(in.QueueUrl)]}, nil
}
func (f *fakeDepthSQS) ListQueues(context.Context, *sqs.ListQueuesInput, ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	return &sqs.ListQueuesOutput{QueueUrls: f.urls}, nil
}

func depthAttrs(v, inflight, delayed string) map[string]string {
	return map[string]string{
		string(sqstypes.QueueAttributeNameApproximateNumberOfMessages):           v,
		string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible): inflight,
		string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed):    delayed,
	}
}

func TestSQSSourceReportsChangesAndFlagsDLQGrowth(t *testing.T) {
	f := &fakeDepthSQS{attrs: map[string]map[string]string{
		"https://q/orders":     depthAttrs("5", "0", "0"),
		"https://q/orders-dlq": depthAttrs("0", "0", "0"),
	}}
	s := &sqsSource{api: f, queues: []queueRef{{"orders", "https://q/orders"}, {"orders-dlq", "https://q/orders-dlq"}}}
	emit, got := collect()

	_ = s.pollOnce(context.Background(), emit)
	if len(got()) != 2 || !strings.Contains(got()[0].Text, "watching · visible 5") {
		t.Fatalf("baseline: %+v", got())
	}
	_ = s.pollOnce(context.Background(), emit)
	if len(got()) != 2 {
		t.Fatal("no change must be silent")
	}

	f.attrs["https://q/orders"] = depthAttrs("9", "1", "0")
	f.attrs["https://q/orders-dlq"] = depthAttrs("2", "0", "0")
	_ = s.pollOnce(context.Background(), emit)
	evs := got()[2:]
	if len(evs) != 2 {
		t.Fatalf("want 2 change events, got %+v", evs)
	}
	if !strings.Contains(evs[0].Text, "visible 9 (+4)") || evs[0].Level != "" {
		t.Errorf("normal queue: %+v", evs[0])
	}
	if evs[1].Level != "error" || !strings.HasPrefix(evs[1].Text, "DLQ received messages") {
		t.Errorf("dlq growth must be an error: %+v", evs[1])
	}
}

func TestResolveQueues(t *testing.T) {
	f := &fakeDepthSQS{urls: []string{"https://sqs/1/orders-a", "https://sqs/1/orders-b-dlq", "https://sqs/1/billing"}}
	qs, err := resolveQueues(context.Background(), f, []string{"orders-*"})
	if err != nil || len(qs) != 2 || qs[1].Name != "orders-b-dlq" {
		t.Errorf("got %+v, %v", qs, err)
	}
}

// ---- plumbing

func TestRunEveryStopsOnAccessDenied(t *testing.T) {
	calls := 0
	err := runEvery(context.Background(), time.Millisecond, func() error {
		calls++
		return &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "no"}
	})
	if err == nil || calls != 1 {
		t.Errorf("err=%v calls=%d; permission errors must stop the source at once", err, calls)
	}
}

func TestRunEveryToleratesTransientErrors(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	err := runEvery(ctx, time.Millisecond, func() error {
		if calls++; calls == 4 {
			cancel()
		}
		return errors.New("throttled")
	})
	if err != nil || calls != 4 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestRunEveryGivesUpAfterRepeatedFailures(t *testing.T) {
	calls := 0
	err := runEvery(context.Background(), time.Millisecond, func() error { calls++; return errors.New("down") })
	if err == nil || calls != 5 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestBuildSourcesLimitAndNoMatch(t *testing.T) {
	f := &fakeLogs{groups: []string{"/aws/lambda/a1", "/aws/lambda/a2", "/aws/lambda/a3"}}
	_, _, err := buildSources(context.Background(), watchDeps{logs: f}, watchOptions{lambda: []string{"a*"}, maxSources: 2}, time.Now(), map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "matched 3") {
		t.Errorf("limit error = %v", err)
	}
	srcs, notes, err := buildSources(context.Background(), watchDeps{logs: f}, watchOptions{lambda: []string{"zzz*"}, maxSources: 5}, time.Now(), map[string]bool{})
	if err != nil || len(srcs) != 0 || len(notes) != 1 {
		t.Errorf("srcs=%v notes=%v err=%v", srcs, notes, err)
	}
}

func TestRenderFeedLine(t *testing.T) {
	e := tailEvent{Kind: "lambda", Source: "orders-api", Time: "2024-01-01T00:00:00Z", Level: "error", Text: "line one\nline two"}
	out := renderFeedLine(e, 100)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "lambda") || !strings.Contains(lines[0], "orders-api") || !strings.HasSuffix(lines[0], "line one") {
		t.Fatalf("got %q", out)
	}
	if !strings.HasPrefix(lines[1], strings.Repeat(" ", feedPrefixWidth)+"line two") {
		t.Errorf("continuation not aligned: %q", lines[1])
	}
	long := renderFeedLine(tailEvent{Kind: "sfn", Text: strings.Repeat("x", 500)}, 80)
	if n := len([]rune(strings.TrimSuffix(long, "\n"))); n > 80 {
		t.Errorf("line is %d wide, want <= 80", n)
	}
	many := renderFeedLine(tailEvent{Kind: "logs", Text: strings.Repeat("l\n", 40)}, 80)
	if !strings.Contains(many, "more lines") {
		t.Errorf("long traces must be capped: %q", many)
	}
}

func TestJSONLine(t *testing.T) {
	if got := parseTailEvent(`{"source":"s"}`).jsonLine(); got != `{"source":"s"}` {
		t.Errorf("events keep the raw envelope, got %s", got)
	}
	got := tailEvent{Kind: "sfn", Source: "M", Level: "error", Text: "x"}.jsonLine()
	for _, want := range []string{`"kind":"sfn"`, `"name":"M"`, `"level":"error"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestRunWatchCombinesSourcesAndCleansUp(t *testing.T) {
	now := time.Now()
	g := "/aws/lambda/orders-api"
	logs := &fakeLogs{groups: []string{g}, events: map[string][]cwtypes.FilteredLogEvent{g: {logEv("1", now.Add(time.Hour), "ERROR kaboom")}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventBody := `{"time":"2024-01-01T00:00:00Z","source":"my.app","detail-type":"Order","detail":{"id":7}}`
	q := &fakeSQS{receive: func(*sqs.ReceiveMessageInput) *sqs.ReceiveMessageOutput {
		time.Sleep(150 * time.Millisecond) // give the log poller a few ticks
		cancel()
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{{Body: aws.String(eventBody), ReceiptHandle: aws.String("rh")}}}
	}}
	eb := &fakeEB{}
	tailBus = "default"

	out := captureStdout(t, func() {
		o := watchOptions{lambda: []string{"orders-*"}, events: true, pattern: `{"source":["my.app"]}`, maxSources: 5, interval: 20 * time.Millisecond}
		if err := runWatch(ctx, watchDeps{eb: eb, q: q, logs: logs}, o); err != nil {
			t.Fatal(err)
		}
	})

	for _, want := range []string{"edbg watch", "watching 1 lambda log group(s)", "orders-api", "ERROR kaboom", "my.app", "Cleaning up"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(q.created) != 1 || len(q.deleted) != 1 {
		t.Errorf("temporary queue not cleaned up: created %v deleted %v", q.created, q.deleted)
	}
	if got := strings.Join(eb.calls, ","); !strings.HasSuffix(got, "PutTargets,RemoveTargets,DeleteRule") {
		t.Errorf("eventbridge calls = %s", got)
	}
}

func TestRunWatchWithoutEventsCreatesNothing(t *testing.T) {
	g := "/aws/lambda/orders-api"
	logs := &fakeLogs{groups: []string{g}, events: map[string][]cwtypes.FilteredLogEvent{}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	q := &fakeSQS{}
	eb := &fakeEB{}
	captureStdout(t, func() {
		if err := runWatch(ctx, watchDeps{eb: eb, q: q, logs: logs}, watchOptions{lambda: []string{"orders-*"}, maxSources: 5, interval: 20 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
	})
	if len(q.created) != 0 || len(eb.calls) != 0 {
		t.Errorf("read-only watch must not create resources: queues %v, eb %v", q.created, eb.calls)
	}
}

func TestRunWatchNothingMatched(t *testing.T) {
	err := runWatch(context.Background(), watchDeps{logs: &fakeLogs{}}, watchOptions{lambda: []string{"zzz*"}, maxSources: 5})
	if err == nil || !strings.Contains(err.Error(), "nothing to watch") {
		t.Errorf("err = %v", err)
	}
}

func TestLogsSourceOrdersAcrossGroupsChronologically(t *testing.T) {
	now := time.Now()
	f := &fakeLogs{events: map[string][]cwtypes.FilteredLogEvent{
		"/aws/lambda/a": {logEv("a1", now.Add(3*time.Second), "a late"), logEv("a2", now.Add(time.Second), "a early")},
		"/aws/lambda/b": {logEv("b1", now.Add(2*time.Second), "b middle")},
	}}
	s := &logsSource{api: f, kind: "lambda", groups: []string{"/aws/lambda/a", "/aws/lambda/b"}, since: now}
	emit, got := collect()
	_ = s.pollOnce(context.Background(), emit)
	var order []string
	for _, e := range got() {
		order = append(order, e.Text)
	}
	if strings.Join(order, ",") != "a early,b middle,a late" {
		t.Errorf("order = %v", order)
	}
}

func TestTruncateMiddleKeepsTheDistinguishingSuffix(t *testing.T) {
	name := "OrdersStack-stateless-s-OrdersApiProcessorCronU-JMyqTxVhdwkH"
	got := truncateMiddle(name, 30)
	if len([]rune(got)) != 30 || !strings.HasSuffix(got, "JMyqTxVhdwkH") || !strings.HasPrefix(got, "OrdersStack") || !strings.Contains(got, "…") {
		t.Errorf("got %q (%d)", got, len([]rune(got)))
	}
	if truncateMiddle("short", 30) != "short" {
		t.Error("short names must pass through")
	}
}

func TestBuildSourcesRescanOnlyReturnsNewResources(t *testing.T) {
	f := &fakeLogs{groups: []string{"/aws/lambda/a1"}}
	known := map[string]bool{}
	o := watchOptions{lambda: []string{"*"}, maxSources: 5}
	srcs, _, err := buildSources(context.Background(), watchDeps{logs: f}, o, time.Now(), known)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("first scan: srcs=%d err=%v", len(srcs), err)
	}
	srcs, _, _ = buildSources(context.Background(), watchDeps{logs: f}, o, time.Now(), known)
	if len(srcs) != 0 {
		t.Errorf("unchanged account should add nothing, got %d", len(srcs))
	}
	f.groups = append(f.groups, "/aws/lambda/a2")
	srcs, _, _ = buildSources(context.Background(), watchDeps{logs: f}, o, time.Now(), known)
	if len(srcs) != 1 || len(srcs[0].(*logsSource).groups) != 1 || srcs[0].(*logsSource).groups[0] != "/aws/lambda/a2" {
		t.Errorf("rescan should return only the new group, got %+v", srcs)
	}
}

func TestResolveQueuesSkipsEdbgTempQueues(t *testing.T) {
	f := &fakeDepthSQS{urls: []string{"https://sqs/1/orders", "https://sqs/1/" + tailPrefix + "abc"}}
	qs, err := resolveQueues(context.Background(), f, []string{"*"})
	if err != nil || len(qs) != 1 {
		t.Fatalf("qs=%v err=%v", qs, err)
	}
	for _, q := range qs {
		if strings.HasPrefix(q.Name, tailPrefix) {
			t.Errorf("temp queue %s should be skipped", q.Name)
		}
	}
}
