package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwmtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

// fakeMetrics answers every query with one datapoint in the current window, valued from data
// (keyed "dimension value/metric name"); missing keys get no datapoints, like an idle resource.
type fakeMetrics struct {
	mu    sync.Mutex
	calls []int
	data  map[string]float64
}

func (f *fakeMetrics) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.mu.Lock()
	f.calls = append(f.calls, len(in.MetricDataQueries))
	f.mu.Unlock()
	out := &cloudwatch.GetMetricDataOutput{}
	at := aws.ToTime(in.EndTime).Add(-time.Minute)
	for _, q := range in.MetricDataQueries {
		key := aws.ToString(q.MetricStat.Metric.Dimensions[len(q.MetricStat.Metric.Dimensions)-1].Value) + "/" + aws.ToString(q.MetricStat.Metric.MetricName)
		r := cwmtypes.MetricDataResult{Id: q.Id}
		if v, ok := f.data[key]; ok {
			ts := at
			if aws.ToInt32(q.MetricStat.Period) > 60 { // a Whole-window query: its bucket starts at the window start
				ts = aws.ToTime(in.EndTime).Add(-time.Duration(aws.ToInt32(q.MetricStat.Period)) * time.Second)
			}
			r.Timestamps, r.Values = []time.Time{ts}, []float64{v}
		}
		out.MetricDataResults = append(out.MetricDataResults, r)
	}
	return out, nil
}

func lambdaRes(names ...string) []dashResource {
	var rs []dashResource
	for _, n := range names {
		rs = append(rs, dashResource{Kind: kindLambda, Name: n, Dims: []cwmtypes.Dimension{dim("FunctionName", n)}})
	}
	return rs
}

func TestFetchSnapshotBatchesAndGatesIdleResources(t *testing.T) {
	names := make([]string, 1203)
	for i := range names {
		names[i] = fmt.Sprintf("fn-%04d", i)
	}
	f := &fakeMetrics{data: map[string]float64{
		"fn-0007/Invocations": 50, "fn-0007/Errors": 4, "fn-0007/Duration": 812.4,
		"fn-0100/Invocations": 900,
	}}
	s, err := fetchSnapshot(context.Background(), f, lambdaRes(names...), 15*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 1203 gate queries in 3 calls (500 max each), then 3 detail metrics for the 2 active functions.
	if len(f.calls) != 4 || s.Metrics != 1203+6 {
		t.Fatalf("calls=%v metrics=%d", f.calls, s.Metrics)
	}
	rows := s.Rows[kindLambda]
	if rows[0].Name != "fn-0007" || rows[1].Name != "fn-0100" {
		t.Errorf("errors should sort first, then traffic: %s, %s", rows[0].Name, rows[1].Name)
	}
	if s.Idle[kindLambda] != 1201 || rows[2].Idle != true {
		t.Errorf("idle=%d", s.Idle[kindLambda])
	}
	if s.Cur[kindLambda]["inv"] != 950 || s.Cur[kindLambda]["err"] != 4 || rows[0].Cur["p95"] != 812.4 {
		t.Errorf("totals %v, p95 %v", s.Cur[kindLambda], rows[0].Cur["p95"])
	}
	if got := s.Total[kindLambda]["inv"]; len(got) != 15 || got[len(got)-1] != 950 {
		t.Errorf("total series = %v", got)
	}
}

func TestStoreSplitsCurrentAndPreviousWindow(t *testing.T) {
	end := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := &dashSnapshot{Start: end.Add(-5 * time.Minute), End: end, Window: 5 * time.Minute, Period: time.Minute}
	row := &dashRow{Cur: map[string]float64{}, Prev: map[string]float64{}, Series: map[string][]float64{}}
	s.store(metricQuery{row, metricSpec{Key: "inv"}}, cwmtypes.MetricDataResult{
		Timestamps: []time.Time{end.Add(-9 * time.Minute), end.Add(-2 * time.Minute), end.Add(-time.Minute)},
		Values:     []float64{3, 5, 7},
	})
	if row.Prev["inv"] != 3 || row.Cur["inv"] != 12 || len(row.Series["inv"]) != 5 {
		t.Errorf("prev=%v cur=%v series=%v", row.Prev["inv"], row.Cur["inv"], row.Series["inv"])
	}
	s.store(metricQuery{row, metricSpec{Key: "vis", Gauge: true}}, cwmtypes.MetricDataResult{
		Timestamps: []time.Time{end.Add(-3 * time.Minute), end.Add(-time.Minute)}, Values: []float64{9, 4},
	})
	if row.Cur["vis"] != 4 {
		t.Errorf("gauge should keep the latest value, got %v", row.Cur["vis"])
	}
}

func TestDashPeriod(t *testing.T) {
	for w, want := range map[time.Duration]time.Duration{
		5 * time.Minute: time.Minute, time.Hour: time.Minute, 3 * time.Hour: 3 * time.Minute, 24 * time.Hour: 24 * time.Minute,
	} {
		if got := dashPeriod(w); got != want {
			t.Errorf("dashPeriod(%v) = %v, want %v", w, got, want)
		}
	}
}

func TestSparklineAndChart(t *testing.T) {
	if got := sparkline([]float64{0, 1, 2, 4, 8}, 10); got != "▁▂▃▅█" {
		t.Errorf("sparkline = %q", got)
	}
	if got := sparkline([]float64{0, 0, 0}, 10); got != "▁▁▁" {
		t.Errorf("flat sparkline = %q", got)
	}
	rows := lineChart([][]float64{{0, 5, 10, 5, 0}}, 30, 4, func(_ int, s string) string { return s })
	if len(rows) != 4 || !strings.HasPrefix(rows[0], "10 ┤") || !strings.HasPrefix(rows[3], " 0 ┤") {
		t.Errorf("chart rows:\n%s", strings.Join(rows, "\n"))
	}
	if humanNum(1234) != "1.2k" || humanNum(56000) != "56k" || humanNum(7) != "7" || shortDuration(time.Hour) != "1h" {
		t.Error("number formatting")
	}
}

func TestMatchNameIsCaseInsensitive(t *testing.T) {
	if !matchName("OrdersService*", "ordersservice-dev-queue") || matchName("orders-*", "payments-api") || !matchName("", "x") {
		t.Error("matchName")
	}
}

type fakeLambda struct {
	awsclient.LambdaAPI
	err error
}

func (f *fakeLambda) ListFunctions(context.Context, *lambda.ListFunctionsInput, ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &lambda.ListFunctionsOutput{Functions: []lambdatypes.FunctionConfiguration{{FunctionName: aws.String("orders-api")}, {FunctionName: aws.String("billing")}}}, nil
}

func TestDiscoverDashFiltersAndNotesFailures(t *testing.T) {
	d := dashDeps{
		lambda: &fakeLambda{},
		sfn:    &fakeWatchSFN{},
		q:      &fakeDepthSQS{urls: []string{"https://sqs/1/orders-dlq", "https://sqs/1/" + tailPrefix + "x"}},
		eb:     &fakeEB{rules: []string{"orders-rule", tailPrefix + "abc"}},
	}
	res, notes := discoverDash(context.Background(), d, "ORDERS*", "default")
	var names []string
	for _, r := range res {
		names = append(names, r.Name)
	}
	if got := strings.Join(names, ","); got != "orders-api,orders-dlq,orders-rule" || len(notes) != 0 {
		t.Errorf("resources=%s notes=%v", got, notes)
	}
	if !res[1].DLQ {
		t.Error("orders-dlq should be flagged as a DLQ")
	}
	d.lambda = &fakeLambda{err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "no lambda:ListFunctions"}}
	res, notes = discoverDash(context.Background(), d, "orders*", "default")
	if len(res) != 2 || len(notes) != 1 || !strings.Contains(notes[0], "Lambda unavailable") {
		t.Errorf("res=%d notes=%v", len(res), notes)
	}
}

type fakeDashLogs struct {
	awsclient.LogsAPI
	patterns []string
}

func (f *fakeDashLogs) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	f.patterns = append(f.patterns, aws.ToString(in.FilterPattern))
	var evs []logtypes.FilteredLogEvent
	for i := range 30 {
		evs = append(evs, logtypes.FilteredLogEvent{Timestamp: aws.Int64(int64(i) * 1000), Message: aws.String(fmt.Sprintf("ERROR boom %d", i))})
	}
	return &cloudwatchlogs.FilterLogEventsOutput{Events: evs}, nil
}

func TestRecentLogsKeepsNewestErrors(t *testing.T) {
	f := &fakeDashLogs{}
	lines, err := recentLogs(context.Background(), dashDeps{logs: f}, "/aws/lambda/x", time.Hour, 5, false)
	if err != nil || len(lines) != 5 || !strings.Contains(lines[4], "boom 29") || !strings.Contains(lines[0], "[E]") {
		t.Fatalf("lines=%v err=%v", lines, err)
	}
	if f.patterns[0] != logErrorFilter {
		t.Errorf("errors-only view should filter server-side, got %q", f.patterns[0])
	}
}

func testDashModel(t *testing.T) dashModel {
	t.Helper()
	f := &fakeMetrics{data: map[string]float64{
		"orders-api/Invocations": 1200, "orders-api/Errors": 38, "orders-api/Duration": 2210,
		"billing/Invocations": 830, "orders-dlq/ApproximateNumberOfMessagesVisible": 9,
	}}
	res := append(lambdaRes("orders-api", "billing", "idle-fn"),
		dashResource{Kind: kindSQS, Name: "orders-dlq", DLQ: true, Dims: []cwmtypes.Dimension{dim("QueueName", "orders-dlq")}})
	s, err := fetchSnapshot(context.Background(), f, res, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := newDashModel(context.Background(), dashDeps{cw: f}, dashOptions{profile: "dev", region: "us-west-2", window: time.Hour, refresh: time.Minute, logLines: 10})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 45})
	next, _ = next.Update(snapMsg{snap: s, res: res, found: time.Now()})
	return next.(dashModel)
}

func TestDashModelViewAndKeys(t *testing.T) {
	m := testDashModel(t)
	view := stripANSI(m.View())
	for _, want := range []string{"edbg dash", "Lambda errors", "Dead-letter queues", "orders-api", "billing", "1 idle", "1 Lambda 2 ✖"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q", want)
		}
	}
	if strings.Index(view, "orders-api") > strings.Index(view, "billing") {
		t.Error("the erroring function should be listed first")
	}
	if n := strings.Count(view, "\n") + 1; n != 45 {
		t.Errorf("view is %d lines, want exactly the terminal height 45", n)
	}
	for _, l := range strings.Split(view, "\n") {
		if w := len([]rune(l)); w > 140 {
			t.Errorf("line wider than terminal (%d): %q", w, l)
		}
	}
	t.Log("\n" + view)

	press := func(m dashModel, keys ...string) (dashModel, tea.Cmd) {
		var cmd tea.Cmd
		var next tea.Model = m
		for _, k := range keys {
			msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
			switch k {
			case "down":
				msg = tea.KeyMsg{Type: tea.KeyDown}
			case "tab":
				msg = tea.KeyMsg{Type: tea.KeyTab}
			case "enter":
				msg = tea.KeyMsg{Type: tea.KeyEnter}
			}
			next, cmd = next.Update(msg)
		}
		return next.(dashModel), cmd
	}
	m2, _ := press(m, "down")
	if m2.selected().Name != "billing" {
		t.Errorf("down should select billing, got %s", m2.selected().Name)
	}
	m2, _ = press(m2, "i")
	if rows, idle := m2.visible(); len(rows) != 3 || idle != 0 {
		t.Errorf("i should reveal idle rows: %d/%d", len(rows), idle)
	}
	m3, _ := press(m, "tab", "tab")
	if m3.tab != kindSQS || m3.selected().Name != "orders-dlq" {
		t.Errorf("tab twice should land on queues, got %v", m3.tab)
	}
	m3, _ = press(m3, "enter")
	if !m3.logOpen || !strings.Contains(strings.Join(m3.logLines, " "), "dlq-inspect orders-dlq") {
		t.Errorf("enter on a queue should show the peek hint: %v", m3.logLines)
	}
	m4, cmd := press(m, "w")
	if m4.o.window != 3*time.Hour || cmd == nil || !m4.loading {
		t.Errorf("w should move to the next window and refresh: %v", m4.o.window)
	}
	m5, _ := press(m, "/", "b", "i", "l")
	if rows, _ := m5.visible(); len(rows) != 1 || rows[0].Name != "billing" || !m5.filtering {
		t.Errorf("filter: %d rows", len(rows))
	}
	if _, cmd := press(m, "q"); cmd == nil {
		t.Error("q should quit")
	}
}

func TestDashModelSmallTerminal(t *testing.T) {
	m := testDashModel(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	if !strings.Contains(next.(dashModel).View(), "Terminal too small") {
		t.Error("expected a size hint")
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24}) // no chart, but cards + table
	v := stripANSI(next.(dashModel).View())
	if strings.Contains(v, "┤") || !strings.Contains(v, "orders-api") {
		t.Error("medium terminal should drop the chart but keep the table")
	}
}
