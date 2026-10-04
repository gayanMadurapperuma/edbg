package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/spf13/cobra"
)

type watchOptions struct {
	lambda, logs, sfn, sqs []string
	events                 bool
	all                    bool
	name                   string        // with --all: narrow every kind to this name pattern
	rescan                 time.Duration // how often to look for newly created resources; 0 disables
	pattern                string
	since                  time.Duration
	verbose                bool
	maxSources             int
	interval               time.Duration // poll interval for logs/sfn; sqs polls at 2.5x
}

type watchDeps struct {
	eb   awsclient.EventBridgeAPI
	q    awsclient.SQSAPI
	sfn  awsclient.SFNAPI
	logs awsclient.LogsAPI
}

var (
	watchOpts   watchOptions
	watchEvents bool
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Live feed across Lambda, Step Functions, SQS and EventBridge",
	Long: `Follows serverless resources directly, so it works no matter what triggered them:

  --lambda  Lambda functions, via their CloudWatch logs (errors, durations, cold starts)
  --logs    any other log groups (ECS, API Gateway, Express workflows...)
  --sfn     Step Functions executions: started, succeeded, failed (with the cause)
  --sqs     SQS queue depth changes; growth in a *dlq* queue is flagged as an error
  --events  EventBridge events (temporary rule + queue, like "edbg tail")
  --all     all of the above for every Lambda, standard state machine and queue in the account
            (narrow with --name 'orders-*'); resources created while watching are picked up

Patterns are names with * and ? wildcards. Limits: a Lambda that does not log shows nothing,
Express workflows have no execution list (point --logs at their log group), and --sqs only
sees counts, not message bodies.`,
	Example: `  edbg watch --lambda 'orders-*' --sfn 'OrderFlow*' --sqs 'orders-*' --events --source my.app
  edbg watch --all --name 'orders-*'
  edbg watch --all --max-sources 100 --since 10m`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		o := watchOpts
		o.events = watchEvents || tailPattern != "" || tailSource != "" || tailDetailType != ""
		if o.all {
			pat := []string{"*"}
			if o.name != "" {
				pat = []string{o.name}
			}
			o.lambda, o.sfn, o.sqs = append(o.lambda, pat...), append(o.sfn, pat...), append(o.sqs, pat...)
			if !cmd.Flags().Changed("events") {
				o.events = true // --events=false opts out of the temporary rule and queue
			}
		} else if o.name != "" {
			return fmt.Errorf("--name only applies together with --all")
		}
		if len(o.lambda)+len(o.logs)+len(o.sfn)+len(o.sqs) == 0 && !o.events {
			return fmt.Errorf("choose at least one source: --lambda, --logs, --sfn, --sqs or --events")
		}
		pattern, err := buildPattern(tailPattern, tailSource, tailDetailType, tailCloudTrail)
		if err != nil {
			return err
		}
		o.pattern = pattern
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		d := watchDeps{
			eb: awsclient.NewEventBridgeClient(cfg), q: awsclient.NewSQSClient(cfg),
			sfn: awsclient.NewSFNClient(cfg), logs: awsclient.NewLogsClient(cfg),
		}
		return runWatch(ctx, d, o)
	},
}

func init() {
	f := watchCmd.Flags()
	f.StringSliceVar(&watchOpts.lambda, "lambda", nil, "Lambda function name pattern (repeatable)")
	f.StringSliceVar(&watchOpts.logs, "logs", nil, "log group name pattern (repeatable)")
	f.StringSliceVar(&watchOpts.sfn, "sfn", nil, "Step Functions state machine name pattern (repeatable)")
	f.StringSliceVar(&watchOpts.sqs, "sqs", nil, "SQS queue name pattern (repeatable)")
	f.BoolVar(&watchOpts.all, "all", false, "watch every Lambda, state machine and queue (plus EventBridge)")
	f.StringVar(&watchOpts.name, "name", "", "with --all: only resources whose name matches this pattern")
	f.DurationVar(&watchOpts.rescan, "rescan", 30*time.Second, "look for newly created resources this often (0 to disable)")
	f.BoolVar(&watchEvents, "events", false, "include EventBridge events (implied by --source/--detail-type/--pattern)")
	f.StringVar(&tailBus, "bus", "default", "event bus name (with --events)")
	f.StringVar(&tailPattern, "pattern", "", "raw EventBridge event pattern (JSON)")
	f.StringVar(&tailSource, "source", "", "match events with this source")
	f.StringVar(&tailDetailType, "detail-type", "", "match events with this detail-type")
	f.BoolVar(&tailCompact, "compact", false, "one line per EventBridge event")
	f.BoolVar(&tailCloudTrail, "include-cloudtrail", false, "with no event filter, also show \"AWS API Call via CloudTrail\" events")
	f.DurationVar(&watchOpts.since, "since", 0, "also show activity from this far back (logs, executions)")
	f.BoolVar(&watchOpts.verbose, "verbose", false, "include Lambda START/END lines")
	f.IntVar(&watchOpts.maxSources, "max-sources", 20, "refuse to watch more than this many resources of one kind")
	f.DurationVar(&watchOpts.interval, "interval", 2*time.Second, "poll interval for logs and executions")
	rootCmd.AddCommand(watchCmd)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

func checkLimit(kind string, n, limit int) error {
	if n > limit {
		return fmt.Errorf("patterns matched %d %s (limit %d); narrow the pattern or raise --max-sources", n, kind, limit)
	}
	return nil
}

func resolveLogGroups(ctx context.Context, api awsclient.LogsAPI, prefix string, patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var groups []string
	for _, pat := range patterns {
		in := &cloudwatchlogs.DescribeLogGroupsInput{}
		switch lp := literalPrefix(pat); {
		case lp != "":
			in.LogGroupNamePrefix = aws.String(prefix + lp)
		case longestLiteral(pat) != "":
			// A leading wildcard cannot use a prefix; let CloudWatch pre-filter by a substring instead of
			// listing every log group in the account. The glob below stays the authority.
			in.LogGroupNamePattern = aws.String(longestLiteral(pat))
		case prefix != "":
			in.LogGroupNamePrefix = aws.String(prefix)
		}
		p := cloudwatchlogs.NewDescribeLogGroupsPaginator(api, in)
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("describe log groups: %w", err)
			}
			for _, g := range page.LogGroups {
				name := aws.ToString(g.LogGroupName)
				if seen[name] || !strings.HasPrefix(name, prefix) {
					continue
				}
				if globMatch(pat, strings.TrimPrefix(name, prefix)) {
					seen[name] = true
					groups = append(groups, name)
				}
			}
		}
	}
	return groups, nil
}

func resolveMachines(ctx context.Context, api awsclient.SFNAPI, patterns []string) (ms []machine, notes []string, err error) {
	p := sfn.NewListStateMachinesPaginator(api, &sfn.ListStateMachinesInput{})
	for p.HasMorePages() {
		page, perr := p.NextPage(ctx)
		if perr != nil {
			return nil, nil, fmt.Errorf("list state machines: %w", perr)
		}
		for _, m := range page.StateMachines {
			name := aws.ToString(m.Name)
			if !matchAny(patterns, name) {
				continue
			}
			if m.Type == sfntypes.StateMachineTypeExpress {
				notes = append(notes, fmt.Sprintf("skipped Express workflow %s (no execution list; use --logs on its log group)", name))
				continue
			}
			ms = append(ms, machine{Name: name, Arn: aws.ToString(m.StateMachineArn)})
		}
	}
	return ms, notes, nil
}

func resolveQueues(ctx context.Context, api awsclient.SQSAPI, patterns []string) ([]queueRef, error) {
	seen := map[string]bool{}
	var qs []queueRef
	for _, pat := range patterns {
		in := &sqs.ListQueuesInput{}
		if lp := literalPrefix(pat); lp != "" {
			in.QueueNamePrefix = aws.String(lp)
		}
		p := sqs.NewListQueuesPaginator(api, in)
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("list queues: %w", err)
			}
			for _, u := range page.QueueUrls {
				name := u[strings.LastIndex(u, "/")+1:]
				if !seen[u] && !strings.HasPrefix(name, tailPrefix) && globMatch(pat, name) {
					seen[u] = true
					qs = append(qs, queueRef{Name: name, URL: u})
				}
			}
		}
	}
	return qs, nil
}

// buildSources turns name patterns into concrete sources and returns notes about anything skipped.
// known holds the resources already being watched (updated in place), so a rescan only returns new ones.
func buildSources(ctx context.Context, d watchDeps, o watchOptions, since time.Time, known map[string]bool) (srcs []feedSource, notes []string, err error) {
	interval := o.interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	first := len(known) == 0
	note := func(format string, args ...any) {
		if first {
			notes = append(notes, fmt.Sprintf(format, args...))
		}
	}
	fresh := func(prefix string, names []string) (out []string) {
		for _, n := range names {
			if !known[prefix+n] {
				known[prefix+n] = true
				out = append(out, n)
			}
		}
		return out
	}
	if len(o.lambda) > 0 {
		gs, err := resolveLogGroups(ctx, d.logs, "/aws/lambda/", o.lambda)
		if err != nil {
			return nil, nil, err
		}
		if err := checkLimit("Lambda log groups", len(gs), o.maxSources); err != nil {
			return nil, nil, err
		}
		if len(gs) == 0 {
			note("no Lambda log groups matched (functions that never ran have none)")
		} else if gs = fresh("log:", gs); len(gs) > 0 {
			srcs = append(srcs, &logsSource{api: d.logs, kind: "lambda", groups: gs, trim: "/aws/lambda/", since: since, verbose: o.verbose, interval: interval})
		}
	}
	if len(o.logs) > 0 {
		gs, err := resolveLogGroups(ctx, d.logs, "", o.logs)
		if err != nil {
			return nil, nil, err
		}
		if err := checkLimit("log groups", len(gs), o.maxSources); err != nil {
			return nil, nil, err
		}
		if len(gs) == 0 {
			note("no log groups matched --logs")
		} else if gs = fresh("log:", gs); len(gs) > 0 {
			srcs = append(srcs, &logsSource{api: d.logs, kind: "logs", groups: gs, since: since, verbose: true, interval: interval})
		}
	}
	if len(o.sfn) > 0 {
		ms, n, err := resolveMachines(ctx, d.sfn, o.sfn)
		if err != nil {
			return nil, nil, err
		}
		if first {
			notes = append(notes, n...)
		}
		if err := checkLimit("state machines", len(ms), o.maxSources); err != nil {
			return nil, nil, err
		}
		if len(ms) == 0 {
			note("no standard state machines matched --sfn")
		} else {
			var fm []machine
			for _, m := range ms {
				if !known["sfn:"+m.Arn] {
					known["sfn:"+m.Arn] = true
					fm = append(fm, m)
				}
			}
			if len(fm) > 0 {
				srcs = append(srcs, &sfnSource{api: d.sfn, machines: fm, since: since, interval: interval})
			}
		}
	}
	if len(o.sqs) > 0 {
		qs, err := resolveQueues(ctx, d.q, o.sqs)
		if err != nil {
			return nil, nil, err
		}
		if err := checkLimit("queues", len(qs), o.maxSources); err != nil {
			return nil, nil, err
		}
		if len(qs) == 0 {
			note("no queues matched --sqs")
		} else {
			var fq []queueRef
			for _, q := range qs {
				if !known["sqs:"+q.URL] {
					known["sqs:"+q.URL] = true
					fq = append(fq, q)
				}
			}
			if len(fq) > 0 {
				srcs = append(srcs, &sqsSource{api: d.q, queues: fq, interval: interval * 5 / 2})
			}
		}
	}
	return srcs, notes, nil
}

func runWatch(ctx context.Context, d watchDeps, o watchOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	_, _ = fmt.Fprintln(os.Stderr, "resolving resources…")
	start := time.Now()
	known := map[string]bool{}
	srcs, notes, err := buildSources(ctx, d, o, start.Add(-o.since), known)
	if err != nil {
		return err
	}
	if len(srcs) == 0 && !o.events {
		return fmt.Errorf("nothing to watch: %s", strings.Join(notes, "; "))
	}

	var sess *tailSession
	if o.events {
		if sess, err = startTail(ctx, d.eb, d.q, tailBus, o.pattern); err != nil {
			return err
		}
		defer sess.Close() // runs after the terminal is restored below
	}

	var emit func(tailEvent)
	var v *tailViewer
	restore := func() {}
	if outputJSON() {
		var mu sync.Mutex
		emit = func(e tailEvent) { mu.Lock(); fmt.Println(e.jsonLine()); mu.Unlock() }
	} else {
		var keys bool
		v, keys, restore = openView(cancel)
		defer restore()
		emit = v.Event
		info := make([]string, 0, len(srcs)+len(notes)+1)
		for _, s := range srcs {
			info = append(info, "watching "+s.Describe())
		}
		if o.events {
			info = append(info, fmt.Sprintf("watching EventBridge bus %q  %s", tailBus, o.pattern))
		}
		for _, n := range notes {
			info = append(info, "note: "+n)
		}
		v.Banner("edbg watch", info, keys)
	}

	var wg sync.WaitGroup
	say := func(level, source, text string) {
		emit(tailEvent{Kind: "watch", Source: source, Time: time.Now().UTC().Format(time.RFC3339), Level: level, Text: text})
	}
	launch := func(s feedSource) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Run(ctx, emit); err != nil {
				say("error", s.Describe(), "stopped: "+friendlyError(err))
			}
		}()
	}
	for _, s := range srcs {
		launch(s)
	}
	if o.rescan > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// New resources are read from the previous scan onward, so their first invocation is not missed.
			last, lastWarn := start, ""
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(o.rescan):
				}
				scanAt := time.Now()
				fresh, _, err := buildSources(ctx, d, o, last, known)
				if err != nil {
					if msg := friendlyError(err); ctx.Err() == nil && msg != lastWarn {
						lastWarn = msg
						say("warn", "rescan", msg)
					}
					continue // keep `last` so nothing is skipped once a rescan succeeds
				}
				last = scanAt
				for _, s := range fresh {
					say("", "new", "now watching "+s.Describe())
					launch(s)
				}
			}
		}()
	}
	var pollErr error
	if sess != nil {
		pollErr = sess.Poll(ctx, emit)
	} else {
		<-ctx.Done()
	}
	cancel()
	wg.Wait()

	restore()
	if v != nil {
		fmt.Println("\n" + v.Summary())
	}
	return pollErr
}
