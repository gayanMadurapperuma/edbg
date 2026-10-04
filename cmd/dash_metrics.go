package cmd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

type dashKind int

const (
	kindLambda dashKind = iota
	kindSFN
	kindSQS
	kindRule
	numKinds
)

var kindTitles = [numKinds]string{"Lambda", "Step Functions", "Queues", "EventBridge"}

type metricSpec struct {
	Key, Namespace, Name, Stat string
	Gauge                      bool // the latest value matters (queue depth), not the sum
	MaxAcross                  bool // combine resources with max instead of sum (age, latency)
	Whole                      bool // one datapoint for the whole window (percentiles cannot be summed)
}

// kindSpec lists the metrics per resource kind. The Gate metrics are fetched for every resource; the
// rest only for resources with activity in a gate metric. An idle function cannot have errors, so a
// 900-function account costs about one metric per idle function per refresh.
type kindSpec struct {
	Gate    []string
	Metrics []metricSpec
}

func (k kindSpec) isGate(key string) bool {
	for _, g := range k.Gate {
		if g == key {
			return true
		}
	}
	return false
}

var kindSpecs = [numKinds]kindSpec{
	kindLambda: {Gate: []string{"inv"}, Metrics: []metricSpec{
		{Key: "inv", Namespace: "AWS/Lambda", Name: "Invocations", Stat: "Sum"},
		{Key: "err", Namespace: "AWS/Lambda", Name: "Errors", Stat: "Sum"},
		{Key: "thr", Namespace: "AWS/Lambda", Name: "Throttles", Stat: "Sum"},
		{Key: "p95", Namespace: "AWS/Lambda", Name: "Duration", Stat: "p95", MaxAcross: true, Whole: true},
	}},
	kindSFN: {Gate: []string{"start", "fail"}, Metrics: []metricSpec{
		{Key: "start", Namespace: "AWS/States", Name: "ExecutionsStarted", Stat: "Sum"},
		{Key: "ok", Namespace: "AWS/States", Name: "ExecutionsSucceeded", Stat: "Sum"},
		{Key: "fail", Namespace: "AWS/States", Name: "ExecutionsFailed", Stat: "Sum"},
		{Key: "tout", Namespace: "AWS/States", Name: "ExecutionsTimedOut", Stat: "Sum"},
	}},
	kindSQS: {Gate: []string{"vis", "sent"}, Metrics: []metricSpec{
		{Key: "vis", Namespace: "AWS/SQS", Name: "ApproximateNumberOfMessagesVisible", Stat: "Maximum", Gauge: true},
		{Key: "age", Namespace: "AWS/SQS", Name: "ApproximateAgeOfOldestMessage", Stat: "Maximum", Gauge: true, MaxAcross: true},
		{Key: "sent", Namespace: "AWS/SQS", Name: "NumberOfMessagesSent", Stat: "Sum"},
		{Key: "recv", Namespace: "AWS/SQS", Name: "NumberOfMessagesReceived", Stat: "Sum"},
	}},
	kindRule: {Gate: []string{"match"}, Metrics: []metricSpec{
		{Key: "match", Namespace: "AWS/Events", Name: "MatchedEvents", Stat: "Sum"},
		{Key: "inv", Namespace: "AWS/Events", Name: "Invocations", Stat: "Sum"},
		{Key: "fail", Namespace: "AWS/Events", Name: "FailedInvocations", Stat: "Sum"},
	}},
}

type dashResource struct {
	Kind dashKind
	Name string
	Arn  string // state machines: used for execution lookups
	Dims []cwtypes.Dimension
	DLQ  bool
}

type dashRow struct {
	dashResource
	Cur, Prev map[string]float64   // window totals (gauges: latest value)
	Series    map[string][]float64 // current-window buckets; NaN = no datapoint
	Idle      bool
}

func (r *dashRow) v(key string) float64 { return r.Cur[key] }

type dashSnapshot struct {
	At, Start, End time.Time // Start..End is the current window
	Window, Period time.Duration
	Rows           [numKinds][]*dashRow // sorted, most in need of attention first
	Idle           [numKinds]int
	Total          [numKinds]map[string][]float64
	Cur, Prev      [numKinds]map[string]float64
	Discovered     [numKinds]int
	Metrics        int // metrics requested by this refresh (what CloudWatch bills)
	Notes          []string
}

// ---- discovery ----

type dashDeps struct {
	lambda awsclient.LambdaAPI
	sfn    awsclient.SFNAPI
	q      awsclient.SQSAPI
	eb     awsclient.EventBridgeAPI
	cw     awsclient.MetricsAPI
	logs   awsclient.LogsAPI
}

// matchName is a case-insensitive glob; resource naming conventions differ per service
// (OrdersService-dev-fn vs ordersservice-dev-queue), and one --name should catch both.
func matchName(pattern, name string) bool {
	return pattern == "" || globMatch(strings.ToLower(pattern), strings.ToLower(name))
}

func dim(name, value string) cwtypes.Dimension {
	return cwtypes.Dimension{Name: aws.String(name), Value: aws.String(value)}
}

// discoverDash lists resources of every kind in parallel. A kind that fails (often a missing
// permission) becomes a note so the rest of the dashboard still works.
func discoverDash(ctx context.Context, d dashDeps, name, bus string) ([]dashResource, []string) {
	type result struct {
		res []dashResource
		err error
	}
	var out [numKinds]result
	var wg sync.WaitGroup
	run := func(k dashKind, f func() ([]dashResource, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f()
			out[k] = result{r, err}
		}()
	}
	run(kindLambda, func() ([]dashResource, error) {
		var rs []dashResource
		p := lambda.NewListFunctionsPaginator(d.lambda, &lambda.ListFunctionsInput{})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, f := range page.Functions {
				if n := aws.ToString(f.FunctionName); matchName(name, n) {
					rs = append(rs, dashResource{Kind: kindLambda, Name: n, Dims: []cwtypes.Dimension{dim("FunctionName", n)}})
				}
			}
		}
		return rs, nil
	})
	run(kindSFN, func() ([]dashResource, error) {
		var rs []dashResource
		p := sfn.NewListStateMachinesPaginator(d.sfn, &sfn.ListStateMachinesInput{})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, m := range page.StateMachines {
				if n, arn := aws.ToString(m.Name), aws.ToString(m.StateMachineArn); matchName(name, n) {
					rs = append(rs, dashResource{Kind: kindSFN, Name: n, Arn: arn, Dims: []cwtypes.Dimension{dim("StateMachineArn", arn)}})
				}
			}
		}
		return rs, nil
	})
	run(kindSQS, func() ([]dashResource, error) {
		var rs []dashResource
		p := sqs.NewListQueuesPaginator(d.q, &sqs.ListQueuesInput{MaxResults: aws.Int32(1000)})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, u := range page.QueueUrls {
				n := u[strings.LastIndex(u, "/")+1:]
				if !strings.HasPrefix(n, tailPrefix) && matchName(name, n) {
					rs = append(rs, dashResource{Kind: kindSQS, Name: n, Arn: u, Dims: []cwtypes.Dimension{dim("QueueName", n)}, DLQ: isDLQ(n)})
				}
			}
		}
		return rs, nil
	})
	run(kindRule, func() ([]dashResource, error) {
		var rs []dashResource
		in := &eventbridge.ListRulesInput{EventBusName: aws.String(bus)}
		for {
			page, err := d.eb.ListRules(ctx, in)
			if err != nil {
				return nil, err
			}
			for _, r := range page.Rules {
				n := aws.ToString(r.Name)
				if strings.HasPrefix(n, tailPrefix) || !matchName(name, n) {
					continue
				}
				dims := []cwtypes.Dimension{dim("RuleName", n)}
				if bus != "default" {
					dims = append([]cwtypes.Dimension{dim("EventBusName", bus)}, dims...)
				}
				rs = append(rs, dashResource{Kind: kindRule, Name: n, Dims: dims})
			}
			if page.NextToken == nil {
				break
			}
			in.NextToken = page.NextToken
		}
		return rs, nil
	})
	wg.Wait()

	var all []dashResource
	var notes []string
	for k, r := range out {
		if r.err != nil {
			if ctx.Err() == nil {
				notes = append(notes, fmt.Sprintf("%s unavailable: %s", kindTitles[k], friendlyError(r.err)))
			}
			continue
		}
		all = append(all, r.res...)
	}
	return all, notes
}

// ---- metrics ----

const (
	metricsPerCall  = 500 // GetMetricData limit
	metricsParallel = 4
)

// dashPeriod keeps a window at no more than about 60 points, in whole minutes as CloudWatch requires.
func dashPeriod(window time.Duration) time.Duration {
	p := (window/60 + time.Minute - 1).Truncate(time.Minute)
	return max(p, time.Minute)
}

type metricQuery struct {
	row  *dashRow
	spec metricSpec
}

// fetchSnapshot queries two windows (current and the one before, for the trend arrows) for every
// resource. CloudWatch bills per metric requested, not per datapoint, so the second window is free.
func fetchSnapshot(ctx context.Context, api awsclient.MetricsAPI, res []dashResource, window time.Duration, now time.Time) (*dashSnapshot, error) {
	period := dashPeriod(window)
	end := now.Truncate(time.Minute).Add(-time.Minute) // the newest minute is usually still incomplete
	s := &dashSnapshot{At: now, End: end, Start: end.Add(-window), Window: window, Period: period}

	rows := make([]*dashRow, len(res))
	var stage1, stage2 []metricQuery
	for i, r := range res {
		row := &dashRow{dashResource: r, Cur: map[string]float64{}, Prev: map[string]float64{}, Series: map[string][]float64{}}
		rows[i] = row
		s.Discovered[r.Kind]++
		spec := kindSpecs[r.Kind]
		for _, m := range spec.Metrics {
			if len(spec.Gate) == 0 || spec.isGate(m.Key) {
				stage1 = append(stage1, metricQuery{row, m})
			}
		}
	}
	if err := runMetricQueries(ctx, api, stage1, s); err != nil {
		return nil, err
	}
	for _, row := range rows {
		spec := kindSpecs[row.Kind]
		active := 0.0
		for _, g := range spec.Gate {
			active += row.Cur[g] + row.Prev[g]
		}
		if len(spec.Gate) == 0 || active == 0 {
			continue
		}
		for _, m := range spec.Metrics {
			if !spec.isGate(m.Key) {
				stage2 = append(stage2, metricQuery{row, m})
			}
		}
	}
	if err := runMetricQueries(ctx, api, stage2, s); err != nil {
		return nil, err
	}
	s.Metrics = len(stage1) + len(stage2)
	s.build(rows)
	return s, nil
}

func runMetricQueries(ctx context.Context, api awsclient.MetricsAPI, qs []metricQuery, s *dashSnapshot) error {
	if len(qs) == 0 {
		return nil
	}
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, metricsParallel)
	var wg sync.WaitGroup
	for off := 0; off < len(qs); off += metricsPerCall {
		chunk := qs[off:min(off+metricsPerCall, len(qs))]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			res, err := getMetricChunk(ctx, api, chunk, s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for i, q := range chunk {
				s.store(q, res[i])
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// getMetricChunk runs one GetMetricData call (following NextToken, which can split a series across
// pages) and returns the datapoints for each query in chunk order.
func getMetricChunk(ctx context.Context, api awsclient.MetricsAPI, chunk []metricQuery, s *dashSnapshot) ([]cwtypes.MetricDataResult, error) {
	in := &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(s.Start.Add(-s.Window)), EndTime: aws.Time(s.End),
		ScanBy: cwtypes.ScanByTimestampAscending,
	}
	for i, q := range chunk {
		period := s.Period
		if q.spec.Whole {
			period = s.Window
		}
		in.MetricDataQueries = append(in.MetricDataQueries, cwtypes.MetricDataQuery{
			Id: aws.String(fmt.Sprintf("q%d", i)),
			MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{Namespace: aws.String(q.spec.Namespace), MetricName: aws.String(q.spec.Name), Dimensions: q.row.Dims},
				Period: aws.Int32(int32(period / time.Second)), Stat: aws.String(q.spec.Stat),
			},
		})
	}
	out := make([]cwtypes.MetricDataResult, len(chunk))
	for {
		page, err := api.GetMetricData(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("get metric data: %w", err)
		}
		for _, r := range page.MetricDataResults {
			var i int
			if _, err := fmt.Sscanf(aws.ToString(r.Id), "q%d", &i); err != nil || i < 0 || i >= len(out) {
				continue
			}
			out[i].Timestamps = append(out[i].Timestamps, r.Timestamps...)
			out[i].Values = append(out[i].Values, r.Values...)
		}
		if page.NextToken == nil {
			return out, nil
		}
		in.NextToken = page.NextToken
	}
}

// store turns raw datapoints into a fixed-length bucket series covering both windows.
func (s *dashSnapshot) store(q metricQuery, r cwtypes.MetricDataResult) {
	period := s.Period
	if q.spec.Whole {
		period = s.Window
	}
	n := int(s.Window / period)
	from := s.Start.Add(-s.Window)
	all := make([]float64, 2*n)
	for i := range all {
		all[i] = math.NaN()
	}
	for i, ts := range r.Timestamps {
		if i >= len(r.Values) {
			break
		}
		if idx := int(ts.Sub(from) / period); idx >= 0 && idx < len(all) {
			all[idx] = r.Values[i]
		}
	}
	prev, cur := all[:n], all[n:]
	q.row.Series[q.spec.Key] = cur
	q.row.Cur[q.spec.Key] = windowValue(q.spec, cur)
	q.row.Prev[q.spec.Key] = windowValue(q.spec, prev)
}

func windowValue(m metricSpec, xs []float64) float64 {
	if m.Gauge || m.Whole {
		for i := len(xs) - 1; i >= 0; i-- {
			if !math.IsNaN(xs[i]) {
				return xs[i]
			}
		}
		return 0
	}
	t := 0.0
	for _, x := range xs {
		if !math.IsNaN(x) {
			t += x
		}
	}
	return t
}

func isIdle(r *dashRow) bool {
	switch r.Kind {
	case kindLambda:
		return r.v("inv") == 0
	case kindSFN:
		return r.v("start")+r.v("ok")+r.v("fail")+r.v("tout") == 0
	case kindSQS:
		return r.v("vis")+r.v("sent")+r.v("recv") == 0
	case kindRule:
		return r.v("match")+r.v("fail") == 0
	}
	return false
}

// attention ranks rows: failures first, then backlog and throttling, then traffic.
func attention(r *dashRow) []float64 {
	switch r.Kind {
	case kindLambda:
		return []float64{r.v("err"), r.v("thr"), r.v("inv")}
	case kindSFN:
		return []float64{r.v("fail") + r.v("tout"), r.v("start")}
	case kindSQS:
		dlq := 0.0
		if r.DLQ {
			dlq = r.v("vis")
		}
		return []float64{dlq, r.v("age"), r.v("vis"), r.v("sent")}
	case kindRule:
		return []float64{r.v("fail"), r.v("match")}
	}
	return nil
}

func (s *dashSnapshot) build(rows []*dashRow) {
	for k := range numKinds {
		s.Total[k], s.Cur[k], s.Prev[k] = map[string][]float64{}, map[string]float64{}, map[string]float64{}
	}
	for _, r := range rows {
		r.Idle = isIdle(r)
		k := r.Kind
		s.Rows[k] = append(s.Rows[k], r)
		if r.Idle {
			s.Idle[k]++
		}
		for _, m := range kindSpecs[k].Metrics {
			combine := func(a, b float64) float64 { return a + b }
			if m.MaxAcross {
				combine = math.Max
			}
			s.Cur[k][m.Key] = combine(s.Cur[k][m.Key], r.Cur[m.Key])
			s.Prev[k][m.Key] = combine(s.Prev[k][m.Key], r.Prev[m.Key])
			if m.Whole {
				continue
			}
			src := r.Series[m.Key]
			if src == nil {
				continue
			}
			dst := s.Total[k][m.Key]
			if dst == nil {
				dst = make([]float64, len(src))
				s.Total[k][m.Key] = dst
			}
			for i, x := range src {
				if !math.IsNaN(x) {
					dst[i] = combine(dst[i], x)
				}
			}
		}
	}
	for k := range numKinds {
		rs := s.Rows[k]
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].Idle != rs[j].Idle {
				return !rs[i].Idle
			}
			a, b := attention(rs[i]), attention(rs[j])
			for x := range a {
				if a[x] != b[x] {
					return a[x] > b[x]
				}
			}
			return rs[i].Name < rs[j].Name
		})
	}
}

// isAccessDenied is used to stop retrying refreshes that can never succeed.
func isAccessDenied(err error) bool {
	var ae interface{ ErrorCode() string }
	return errors.As(err, &ae) && strings.HasPrefix(ae.ErrorCode(), "AccessDenied")
}
