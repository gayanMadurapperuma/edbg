package cmd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/smithy-go"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
)

// feedSource is one thing `edbg watch` polls. Run blocks until ctx is cancelled or the source gives up.
type feedSource interface {
	Describe() string
	Run(ctx context.Context, emit func(tailEvent)) error
}

// runEvery polls immediately and then on each tick. Transient errors are tolerated;
// permission errors, or five failures in a row, end the source so the user sees why it went quiet.
func runEvery(ctx context.Context, interval time.Duration, poll func() error) error {
	fails := 0
	for {
		if err := poll(); err != nil && ctx.Err() == nil {
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && strings.HasPrefix(apiErr.ErrorCode(), "AccessDenied") {
				return err
			}
			if fails++; fails >= 5 {
				return err
			}
		} else {
			fails = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

const logOverlap = 5 * time.Second // logs can be ingested late, so each poll re-reads a little history and dedupes by event id

var (
	reLogError  = regexp.MustCompile(`(?i)\bERROR\b|"level"\s*:\s*"(error|fatal)"|Task timed out|Runtime\.|Unhandled|Traceback|Exception|"errorType"|panic:`)
	reLogWarn   = regexp.MustCompile(`(?i)\bWARN(ING)?\b|"level"\s*:\s*"warn`)
	reDuration  = regexp.MustCompile(`\tDuration: ([\d.]+) ms`)
	reMemory    = regexp.MustCompile(`Max Memory Used: (\d+) MB`)
	reInit      = regexp.MustCompile(`Init Duration: ([\d.]+) ms`)
	reReportBad = regexp.MustCompile(`Status: (timeout|error)`)
)

// classifyLog picks a display level and text for one log message.
// Levels: "dim" (hidden unless --verbose), "ok", "warn", "error", or "".
func classifyLog(msg string) (level, text string) {
	switch {
	case strings.HasPrefix(msg, "START RequestId"), strings.HasPrefix(msg, "END RequestId"):
		return "dim", msg
	case strings.HasPrefix(msg, "REPORT RequestId"):
		d := reDuration.FindStringSubmatch(msg)
		if d == nil {
			return "dim", msg
		}
		text = "done " + d[1] + " ms"
		if m := reMemory.FindStringSubmatch(msg); m != nil {
			text += " · " + m[1] + " MB"
		}
		if i := reInit.FindStringSubmatch(msg); i != nil {
			text += " · cold start " + i[1] + " ms"
		}
		if reReportBad.MatchString(msg) {
			return "error", text + " · " + reReportBad.FindString(msg)
		}
		return "ok", text
	case reLogError.MatchString(msg):
		return "error", msg
	case reLogWarn.MatchString(msg):
		return "warn", msg
	}
	return "", msg
}

type groupState struct {
	last int64            // newest timestamp seen (ms)
	seen map[string]int64 // event id -> timestamp, for de-duplication across overlapping polls
}

// logsSource follows CloudWatch log groups. It works for any trigger because it reads what the code wrote.
type logsSource struct {
	api      awsclient.LogsAPI
	kind     string // "lambda" or "logs"
	groups   []string
	trim     string // removed from the group name to get the display name
	since    time.Time
	verbose  bool
	interval time.Duration
	state    map[string]*groupState
}

func (s *logsSource) Describe() string {
	return fmt.Sprintf("%d %s log group(s)", len(s.groups), s.kind)
}

func (s *logsSource) Run(ctx context.Context, emit func(tailEvent)) error {
	return runEvery(ctx, s.interval, func() error { return s.pollOnce(ctx, emit) })
}

func (s *logsSource) pollOnce(ctx context.Context, emit func(tailEvent)) error {
	if s.state == nil {
		s.state = map[string]*groupState{}
	}
	type pending struct {
		group, msg string
		ts         int64
	}
	var batch []pending
	var firstErr error
	for _, g := range s.groups {
		st := s.state[g]
		if st == nil {
			st = &groupState{last: s.since.UnixMilli(), seen: map[string]int64{}}
			s.state[g] = st
		}
		from := max(st.last-logOverlap.Milliseconds(), s.since.UnixMilli())
		in := &cloudwatchlogs.FilterLogEventsInput{LogGroupName: aws.String(g), StartTime: aws.Int64(from)}

		for page := 0; page < 5; page++ { // cap pages per poll so one noisy group cannot starve the rest
			out, err := s.api.FilterLogEvents(ctx, in)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("filter log events %s: %w", g, err)
				}
				break
			}
			for _, ev := range out.Events {
				id, ts := aws.ToString(ev.EventId), aws.ToInt64(ev.Timestamp)
				if _, dup := st.seen[id]; dup {
					continue
				}
				st.seen[id] = ts
				st.last = max(st.last, ts)
				batch = append(batch, pending{g, aws.ToString(ev.Message), ts})
			}
			if out.NextToken == nil {
				break
			}
			in.NextToken = out.NextToken
		}
		for id, ts := range st.seen {
			if ts < st.last-30_000 {
				delete(st.seen, id)
			}
		}
	}
	// groups are polled one after another; sort so the feed reads chronologically across functions
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].ts < batch[j].ts })
	for _, p := range batch {
		s.emitLog(p.group, p.ts, p.msg, emit)
	}
	return firstErr
}

func (s *logsSource) emitLog(group string, ts int64, msg string, emit func(tailEvent)) {
	msg = strings.TrimRight(msg, "\r\n")
	if msg == "" {
		return
	}
	level, text := classifyLog(msg)
	if level == "dim" && !s.verbose {
		return
	}
	emit(tailEvent{
		Kind: s.kind, Source: strings.TrimPrefix(group, s.trim),
		Time: time.UnixMilli(ts).UTC().Format(time.RFC3339), Level: level, Text: text,
	})
}
