package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
)

var (
	dashOpts dashOptions
	dashOnce bool
	dashTop  int
)

var dashCmd = &cobra.Command{
	Use:   "dash",
	Short: "Live dashboard: Lambda, Step Functions, SQS and EventBridge health at a glance",
	Long: `A full-screen dashboard built from CloudWatch metrics, so it covers hundreds of resources
regardless of what triggers them. Problems sort to the top: errors, dead-letter messages,
throttling, then traffic. Select a row and press enter for its recent errors, or l to stream its
logs live.

Metrics arrive 1-3 minutes late; the dashboard is for trends, the log pane for detail.
Cost: CloudWatch bills about $0.01 per 1,000 metrics requested. Idle resources cost 1-2 metrics
per refresh, active ones up to 4. The header shows the estimate; narrow with --name, or slow
down with --refresh.`,
	Example: `  edbg dash
  edbg dash --name 'orders-*' --window 1h
  edbg dash --once                 # print a snapshot and exit
  edbg dash --once -o json | jq '.lambda[] | select(.errors > 0)'`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		o := dashOpts
		o.region = cfg.Region
		o.profile = flagProfile
		if o.profile == "" {
			o.profile = os.Getenv("AWS_PROFILE")
		}
		if o.profile == "" {
			o.profile = "default"
		}
		if o.refresh < 30*time.Second {
			return fmt.Errorf("--refresh must be at least 30s (metrics only change once a minute)")
		}
		d := dashDeps{
			lambda: awsclient.NewLambdaClient(cfg), sfn: awsclient.NewSFNClient(cfg), q: awsclient.NewSQSClient(cfg),
			eb: awsclient.NewEventBridgeClient(cfg), cw: awsclient.NewMetricsClient(cfg), logs: awsclient.NewLogsClient(cfg),
		}
		if flagNoColor || os.Getenv("NO_COLOR") != "" {
			lipgloss.SetColorProfile(termenv.Ascii)
		}
		if dashOnce || outputJSON() || !isTerminal(os.Stdout) {
			return runDashOnce(ctx, d, o)
		}
		p := tea.NewProgram(newDashModel(ctx, d, o), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
		_, err = p.Run()
		if err != nil && ctx.Err() != nil {
			return ctx.Err() // Ctrl+C from outside the program: exit quietly like the other commands
		}
		return err
	},
}

func init() {
	f := dashCmd.Flags()
	f.StringVar(&dashOpts.name, "name", "", "only resources whose name matches this pattern (* and ?, case-insensitive)")
	f.StringVar(&dashOpts.bus, "bus", "default", "event bus whose rules are shown")
	f.DurationVar(&dashOpts.window, "window", 15*time.Minute, "time window for the numbers and charts")
	f.DurationVar(&dashOpts.refresh, "refresh", time.Minute, "how often to re-query metrics")
	f.IntVar(&dashOpts.logLines, "log-lines", 15, "lines shown in the log pane")
	f.BoolVar(&dashOnce, "once", false, "print one snapshot and exit (implied by -o json or a non-terminal stdout)")
	f.IntVar(&dashTop, "top", 10, "with --once: rows per service")
	rootCmd.AddCommand(dashCmd)
}

func runDashOnce(ctx context.Context, d dashDeps, o dashOptions) error {
	if !outputJSON() {
		_, _ = fmt.Fprintln(os.Stderr, "discovering resources and fetching metrics…")
	}
	res, notes := discoverDash(ctx, d, o.name, o.bus)
	snap, err := fetchSnapshot(ctx, d.cw, res, o.window, time.Now())
	if err != nil {
		return err
	}
	snap.Notes = notes
	if outputJSON() {
		b, err := json.MarshalIndent(dashJSON(snap), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(renderDashText(newDashStyles(), snap, o, termWidthOr(120), dashTop))
	return nil
}

func termWidthOr(def int) int {
	if w := termWidth(); w > 0 {
		return max(w, 80)
	}
	return def
}

func renderDashText(st dashStyles, s *dashSnapshot, o dashOptions, width, top int) string {
	var b strings.Builder
	b.WriteString(st.title.Render("edbg dash") + st.dim.Render(fmt.Sprintf(" · %s · %s · last %s · data to %s · %d metrics",
		o.profile, o.region, shortDuration(s.Window), s.End.Local().Format("15:04"), s.Metrics)) + "\n")
	b.WriteString(renderCards(st, dashCards(s), width) + "\n")
	for k := range numKinds {
		rows := s.Rows[k]
		active := len(rows) - s.Idle[k]
		b.WriteString("\n" + st.title.Render(kindTitles[k]) + st.dim.Render(fmt.Sprintf("  %d active · %d idle", active, s.Idle[k])) + "\n")
		if active == 0 {
			continue
		}
		for _, l := range renderTable(st, k, rows, width, -1, 0, min(top, active)) {
			b.WriteString(l + "\n")
		}
		if active > top {
			b.WriteString(st.dim.Render(fmt.Sprintf("   … %d more active (use the interactive view or -o json)", active-top)) + "\n")
		}
	}
	for _, n := range s.Notes {
		b.WriteString(st.warn.Render("note: "+n) + "\n")
	}
	return b.String()
}

type dashJSONRow struct {
	Name    string             `json:"name"`
	Idle    bool               `json:"idle"`
	DLQ     bool               `json:"dlq,omitempty"`
	Metrics map[string]float64 `json:"metrics"`
	Prev    map[string]float64 `json:"previous_window"`
	Errors  float64            `json:"errors"`
}

func dashJSON(s *dashSnapshot) map[string]any {
	out := map[string]any{
		"window_seconds": s.Window.Seconds(), "start": s.Start, "end": s.End,
		"metrics_requested": s.Metrics, "notes": s.Notes,
	}
	keys := [numKinds]string{"lambda", "step_functions", "queues", "rules"}
	for k := range numKinds {
		rows := make([]dashJSONRow, 0, len(s.Rows[k]))
		for _, r := range s.Rows[k] {
			errs := 0.0
			switch k {
			case kindLambda:
				errs = r.v("err")
			case kindSFN:
				errs = r.v("fail") + r.v("tout")
			case kindSQS:
				if r.DLQ {
					errs = r.v("vis")
				}
			case kindRule:
				errs = r.v("fail")
			}
			rows = append(rows, dashJSONRow{Name: r.Name, Idle: r.Idle, DLQ: r.DLQ, Metrics: r.Cur, Prev: r.Prev, Errors: errs})
		}
		out[keys[k]] = rows
	}
	return out
}
