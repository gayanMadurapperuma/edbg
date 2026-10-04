package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/xray"
	xtypes "github.com/aws/aws-sdk-go-v2/service/xray/types"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/gayanMadurapperuma/edbg/internal/output"
	"github.com/spf13/cobra"
)

const noXRayHint = `No X-Ray data found. X-Ray may not be enabled for these services (it is opt-in).
Enable "Active tracing" on Lambda / API Gateway / Step Functions, or use:
  edbg tail         see the events themselves
  edbg trace-sfn    follow a Step Functions execution
  edbg dlq-inspect  look at failed messages`

var (
	tracesSince   time.Duration
	tracesErrors  bool
	tracesService string
	tracesLimit   int
)

var traceCmd = &cobra.Command{
	Use:   "trace <xray-trace-id>",
	Short: "Show every service hop of one X-Ray trace as a timeline",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		return traceXRay(ctx, awsclient.NewXRayClient(cfg), args[0])
	},
}

var tracesCmd = &cobra.Command{
	Use:   "traces",
	Short: "Find recent X-Ray traces (use --errors to find failures)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		return listTraces(ctx, awsclient.NewXRayClient(cfg), time.Now(), tracesSince, tracesErrors, tracesService, tracesLimit)
	},
}

func init() {
	tracesCmd.Flags().DurationVar(&tracesSince, "since", 15*time.Minute, "look back this far")
	tracesCmd.Flags().BoolVar(&tracesErrors, "errors", false, "only traces with errors, faults or throttling")
	tracesCmd.Flags().StringVar(&tracesService, "service", "", "only traces that touched this service name")
	tracesCmd.Flags().IntVar(&tracesLimit, "limit", 20, "maximum traces to list")
	rootCmd.AddCommand(traceCmd, tracesCmd)
}

// span is one X-Ray segment or subsegment, flattened so cross-service segments can be linked by parent_id.
type span struct {
	ID, ParentID, Name, Origin string
	Start, End                 float64
	Error, Fault, Throttle     bool
	Cause                      string
	children                   []*span
}

type rawSegment struct {
	ID        string       `json:"id"`
	ParentID  string       `json:"parent_id"`
	Name      string       `json:"name"`
	Origin    string       `json:"origin"`
	StartTime float64      `json:"start_time"`
	EndTime   float64      `json:"end_time"`
	Error     bool         `json:"error"`
	Fault     bool         `json:"fault"`
	Throttle  bool         `json:"throttle"`
	Cause     any          `json:"cause"`
	Sub       []rawSegment `json:"subsegments"`
}

func flatten(r rawSegment, parent string, out *[]*span) {
	if r.ParentID != "" {
		parent = r.ParentID // a segment's parent_id points into the calling service's span
	}
	*out = append(*out, &span{
		ID: r.ID, ParentID: parent, Name: r.Name, Origin: r.Origin,
		Start: r.StartTime, End: r.EndTime,
		Error: r.Error, Fault: r.Fault, Throttle: r.Throttle, Cause: causeText(r.Cause),
	})
	for _, s := range r.Sub {
		flatten(s, r.ID, out)
	}
}

// causeText pulls the first exception message out of an X-Ray cause (which may also be a bare exception-id string).
func causeText(c any) string {
	m, ok := c.(map[string]any)
	if !ok {
		return ""
	}
	exs, _ := m["exceptions"].([]any)
	if len(exs) == 0 {
		return ""
	}
	ex, _ := exs[0].(map[string]any)
	msg, _ := ex["message"].(string)
	typ, _ := ex["type"].(string)
	switch {
	case typ != "" && msg != "":
		return typ + ": " + msg
	case msg != "":
		return msg
	}
	return typ
}

func buildTree(spans []*span) (roots []*span, start float64) {
	byID := map[string]*span{}
	for _, s := range spans {
		byID[s.ID] = s
	}
	for _, s := range spans {
		if p, ok := byID[s.ParentID]; ok && s.ParentID != "" {
			p.children = append(p.children, s)
		} else {
			roots = append(roots, s)
		}
		if start == 0 || s.Start < start {
			start = s.Start
		}
	}
	var sortRec func([]*span)
	sortRec = func(ss []*span) {
		sort.Slice(ss, func(i, j int) bool { return ss[i].Start < ss[j].Start })
		for _, s := range ss {
			sortRec(s.children)
		}
	}
	sortRec(roots)
	return roots, start
}

func traceXRay(ctx context.Context, client awsclient.XRayAPI, id string) error {
	var spans []*span
	var token *string
	for {
		out, err := client.BatchGetTraces(ctx, &xray.BatchGetTracesInput{TraceIds: []string{id}, NextToken: token})
		if err != nil {
			return fmt.Errorf("batch get traces: %w", err)
		}
		for _, t := range out.Traces {
			for _, seg := range t.Segments {
				var r rawSegment
				if err := json.Unmarshal([]byte(aws.ToString(seg.Document)), &r); err != nil {
					continue // skip a malformed segment rather than lose the whole trace
				}
				flatten(r, "", &spans)
			}
		}
		if token = out.NextToken; token == nil {
			break
		}
	}
	if len(spans) == 0 {
		fmt.Printf("Trace %s not found. Check the ID, that it is within X-Ray's 30-day retention,\nand that X-Ray is enabled for the services involved (it is opt-in).\nTry: edbg traces --since 24h\n", id)
		return nil
	}

	roots, start := buildTree(spans)
	if outputJSON() {
		return printSpansJSON(roots, start)
	}
	fmt.Printf("Trace %s  (%d spans)\n\n", id, len(spans))
	for _, r := range roots {
		printSpan(r, start, 0)
	}
	return nil
}

func spanStatus(s *span) string {
	switch {
	case s.Fault:
		return output.Red("FAULT")
	case s.Error:
		return output.Red("ERROR")
	case s.Throttle:
		return output.Yellow("THROTTLE")
	}
	return ""
}

func printSpan(s *span, start float64, depth int) {
	line := fmt.Sprintf("%*s%s", depth*2, "", s.Name)
	if s.Origin != "" {
		line += output.Dim(" [" + s.Origin + "]")
	}
	dur := time.Duration((s.End - s.Start) * float64(time.Second)).Round(time.Millisecond)
	off := time.Duration((s.Start - start) * float64(time.Second)).Round(time.Millisecond)
	line += fmt.Sprintf("  +%s  %s", off, dur)
	if st := spanStatus(s); st != "" {
		line += "  " + st
	}
	fmt.Println(line)
	if s.Cause != "" {
		fmt.Printf("%*s  cause: %s\n", depth*2, "", s.Cause)
	}
	for _, c := range s.children {
		printSpan(c, start, depth+1)
	}
}

func printSpansJSON(roots []*span, start float64) error {
	var walk func(s *span)
	walk = func(s *span) {
		b, _ := json.Marshal(map[string]any{
			"id": s.ID, "parent_id": s.ParentID, "name": s.Name, "origin": s.Origin,
			"offset_ms": int64((s.Start - start) * 1000), "duration_ms": int64((s.End - s.Start) * 1000),
			"error": s.Error, "fault": s.Fault, "throttle": s.Throttle, "cause": s.Cause,
		})
		fmt.Println(string(b))
		for _, c := range s.children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return nil
}

func listTraces(ctx context.Context, client awsclient.XRayAPI, now time.Time, since time.Duration, errorsOnly bool, service string, limit int) error {
	in := &xray.GetTraceSummariesInput{StartTime: aws.Time(now.Add(-since)), EndTime: aws.Time(now)}
	switch {
	case errorsOnly && service != "":
		in.FilterExpression = aws.String(fmt.Sprintf(`service("%s") { error OR fault OR throttle }`, service))
	case errorsOnly:
		in.FilterExpression = aws.String("error OR fault OR throttle")
	case service != "":
		in.FilterExpression = aws.String(fmt.Sprintf(`service("%s")`, service))
	}

	n := 0
	for n < limit {
		out, err := client.GetTraceSummaries(ctx, in)
		if err != nil {
			return fmt.Errorf("get trace summaries: %w", err)
		}
		for _, t := range out.TraceSummaries {
			if n >= limit {
				break
			}
			n++
			printTraceSummary(t)
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	if n == 0 {
		if errorsOnly || service != "" {
			// X-Ray answered; there is simply nothing matching, which is not a sign it is disabled.
			fmt.Printf("No matching traces in the last %s.\n", since)
		} else {
			fmt.Println(noXRayHint)
		}
	}
	return nil
}

func printTraceSummary(t xtypes.TraceSummary) {
	if outputJSON() {
		b, _ := json.Marshal(t)
		fmt.Println(string(b))
		return
	}
	status := "ok"
	switch {
	case aws.ToBool(t.HasFault):
		status = output.Red("FAULT")
	case aws.ToBool(t.HasError):
		status = output.Red("ERROR")
	case aws.ToBool(t.HasThrottle):
		status = output.Yellow("THROTTLE")
	}
	entry := ""
	if t.EntryPoint != nil {
		entry = aws.ToString(t.EntryPoint.Name)
	}
	where := ""
	if t.Http != nil && t.Http.HttpURL != nil {
		where = fmt.Sprintf("%s %s", aws.ToString(t.Http.HttpMethod), aws.ToString(t.Http.HttpURL))
	}
	when := ""
	if t.StartTime != nil {
		when = t.StartTime.Local().Format("15:04:05")
	}
	fmt.Printf("%s  %s  %-8s %6.0fms  %s  %s\n", output.Cyan(aws.ToString(t.Id)), when, status, aws.ToFloat64(t.Duration)*1000, entry, where)
}
