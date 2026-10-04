package cmd

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/xray"
	xtypes "github.com/aws/aws-sdk-go-v2/service/xray/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

type fakeXRay struct {
	awsclient.XRayAPI
	segments  []string
	summaries []xtypes.TraceSummary
	gotFilter string
}

func (f *fakeXRay) BatchGetTraces(context.Context, *xray.BatchGetTracesInput, ...func(*xray.Options)) (*xray.BatchGetTracesOutput, error) {
	out := &xray.BatchGetTracesOutput{}
	if len(f.segments) > 0 {
		t := xtypes.Trace{}
		for _, d := range f.segments {
			t.Segments = append(t.Segments, xtypes.Segment{Document: aws.String(d)})
		}
		out.Traces = []xtypes.Trace{t}
	}
	return out, nil
}

func (f *fakeXRay) GetTraceSummaries(_ context.Context, in *xray.GetTraceSummariesInput, _ ...func(*xray.Options)) (*xray.GetTraceSummariesOutput, error) {
	f.gotFilter = aws.ToString(in.FilterExpression)
	return &xray.GetTraceSummariesOutput{TraceSummaries: f.summaries}, nil
}

// An API Gateway segment calls a Lambda segment (linked by parent_id), which has a failing DynamoDB subsegment.
var xrayFixture = []string{
	`{"id":"a1","name":"orders-api","origin":"AWS::ApiGateway::Stage","start_time":100.0,"end_time":100.9,
	  "subsegments":[{"id":"a2","name":"Lambda","start_time":100.1,"end_time":100.8}]}`,
	`{"id":"b1","parent_id":"a2","name":"create-order","origin":"AWS::Lambda::Function","start_time":100.2,"end_time":100.7,
	  "subsegments":[{"id":"b2","name":"DynamoDB","start_time":100.3,"end_time":100.6,"fault":true,
	    "cause":{"exceptions":[{"type":"ProvisionedThroughputExceeded","message":"slow down"}]}}]}`,
}

func TestTraceXRayBuildsCrossServiceTree(t *testing.T) {
	out := captureStdout(t, func() {
		if err := traceXRay(context.Background(), &fakeXRay{segments: xrayFixture}, "1-abc"); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(out, "\n")
	idx := func(sub string) int {
		for i, l := range lines {
			if strings.Contains(l, sub) {
				return i
			}
		}
		t.Fatalf("%q not found in:\n%s", sub, out)
		return -1
	}
	if order := []int{idx("orders-api"), idx("Lambda  +"), idx("create-order"), idx("DynamoDB")}; !sort.IntsAreSorted(order) {
		t.Errorf("hops out of order:\n%s", out)
	}
	if !strings.Contains(lines[idx("create-order")], "  create-order") { // nested under the API Gateway's Lambda subsegment
		t.Errorf("create-order not indented:\n%s", out)
	}
	if !strings.Contains(out, "FAULT") || !strings.Contains(out, "ProvisionedThroughputExceeded: slow down") {
		t.Errorf("fault and cause missing:\n%s", out)
	}
	if !strings.Contains(out, "(4 spans)") {
		t.Errorf("span count wrong:\n%s", out)
	}
}

func TestTraceXRayNoDataShowsHint(t *testing.T) {
	out := captureStdout(t, func() { _ = traceXRay(context.Background(), &fakeXRay{}, "1-none") })
	if !strings.Contains(out, "Trace 1-none not found") || !strings.Contains(out, "opt-in") {
		t.Errorf("got %q", out)
	}
}

func TestTraceXRaySkipsMalformedSegment(t *testing.T) {
	out := captureStdout(t, func() {
		_ = traceXRay(context.Background(), &fakeXRay{segments: []string{`{not json`, xrayFixture[0]}}, "1-abc")
	})
	if !strings.Contains(out, "orders-api") {
		t.Errorf("good segment lost:\n%s", out)
	}
}

func TestListTracesFilter(t *testing.T) {
	tests := []struct {
		errs    bool
		service string
		want    string
	}{
		{false, "", ""},
		{true, "", "error OR fault OR throttle"},
		{false, "orders", `service("orders")`},
		{true, "orders", `service("orders") { error OR fault OR throttle }`},
	}
	for _, tt := range tests {
		f := &fakeXRay{summaries: []xtypes.TraceSummary{{Id: aws.String("1-x"), HasFault: aws.Bool(true), Duration: aws.Float64(0.5)}}}
		out := captureStdout(t, func() {
			_ = listTraces(context.Background(), f, time.Now(), time.Hour, tt.errs, tt.service, 5)
		})
		if f.gotFilter != tt.want {
			t.Errorf("filter = %q, want %q", f.gotFilter, tt.want)
		}
		if !strings.Contains(out, "1-x") || !strings.Contains(out, "FAULT") {
			t.Errorf("summary not printed: %q", out)
		}
	}
}

func TestListTracesFilteredEmptyIsNotAnXRayWarning(t *testing.T) {
	out := captureStdout(t, func() { _ = listTraces(context.Background(), &fakeXRay{}, time.Now(), time.Hour, true, "", 5) })
	if !strings.Contains(out, "No matching traces in the last 1h0m0s") || strings.Contains(out, "may not be enabled") {
		t.Errorf("got %q", out)
	}
}

func TestListTracesEmptyShowsHint(t *testing.T) {
	out := captureStdout(t, func() { _ = listTraces(context.Background(), &fakeXRay{}, time.Now(), time.Hour, false, "", 5) })
	if !strings.Contains(out, "X-Ray may not be enabled") {
		t.Errorf("got %q", out)
	}
}

func TestRunChecksReportsFailureAndTimeout(t *testing.T) {
	checks := []check{
		{"ok", func(context.Context) (string, error) { return "arn:x", nil }},
		{"denied", func(context.Context) (string, error) { return "", errors.New("nope") }},
		{"blackhole", func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }}, // no VPC endpoint
	}
	res := runChecks(context.Background(), checks, 50*time.Millisecond)
	if res[0].Err != nil || res[1].Err == nil || !errors.Is(res[2].Err, context.DeadlineExceeded) {
		t.Fatalf("unexpected results: %+v", res)
	}
	var ok bool
	out := captureStdout(t, func() { ok = printChecks(res) })
	if ok || !strings.Contains(out, "OK") || strings.Count(out, "FAIL") != 2 {
		t.Errorf("ok=%v out:\n%s", ok, out)
	}
}
