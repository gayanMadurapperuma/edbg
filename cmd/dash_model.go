package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var dashWindows = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour, 24 * time.Hour}

const (
	rediscoverEvery = 5 * time.Minute
	metricPrice     = 0.01 / 1000 // USD per metric requested via GetMetricData (us-east-1 list price)
	logErrorFilter  = `?ERROR ?Error ?error ?Exception ?Traceback ?"Task timed out" ?panic`
)

type dashOptions struct {
	name, bus, profile, region string
	window, refresh            time.Duration
	logLines                   int
}

type (
	snapMsg struct {
		snap  *dashSnapshot
		res   []dashResource
		notes []string
		found time.Time
		err   error
	}
	tickMsg struct{}
	logsMsg struct {
		gen   int
		lines []string
		err   error
	}
	liveMsg struct {
		gen  int
		line string
		ch   <-chan string
	}
	liveDoneMsg struct{ gen int }
)

type dashModel struct {
	ctx  context.Context
	d    dashDeps
	o    dashOptions
	st   dashStyles
	snap *dashSnapshot

	res     []dashResource
	found   time.Time
	notes   []string
	err     error
	loading bool

	tab       dashKind
	cursor    [numKinds]int
	offset    [numKinds]int
	filter    string
	filtering bool
	showIdle  bool
	help      bool
	w, h      int

	// bottom pane: last N log lines (or executions) of the selected resource
	logOpen  bool
	logAll   bool
	logTitle string
	logLines []string
	logErr   error
	logGen   int // drops replies for a pane that has since been closed or retargeted
	live     context.CancelFunc
}

func newDashModel(ctx context.Context, d dashDeps, o dashOptions) dashModel {
	return dashModel{ctx: ctx, d: d, o: o, st: newDashStyles(), loading: true}
}

func (m dashModel) Init() tea.Cmd { return m.refresh() }

// refresh rediscovers resources every few minutes (new functions appear without a restart) and
// fetches metrics on every call. It runs off the UI goroutine.
func (m dashModel) refresh() tea.Cmd {
	ctx, d, o, res, found := m.ctx, m.d, m.o, m.res, m.found
	return func() tea.Msg {
		var notes []string
		if res == nil || time.Since(found) > rediscoverEvery {
			res, notes = discoverDash(ctx, d, o.name, o.bus)
			found = time.Now()
		}
		snap, err := fetchSnapshot(ctx, d.cw, res, o.window, time.Now())
		return snapMsg{snap: snap, res: res, notes: notes, found: found, err: err}
	}
}

func (m dashModel) tick() tea.Cmd {
	return tea.Tick(m.o.refresh, func(time.Time) tea.Msg { return tickMsg{} })
}

// visible returns the rows of the current tab after the name filter; idle rows only on request.
func (m dashModel) visible() (rows []*dashRow, idle int) {
	if m.snap == nil {
		return nil, 0
	}
	for _, r := range m.snap.Rows[m.tab] {
		if m.filter != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(m.filter)) {
			continue
		}
		if r.Idle && !m.showIdle {
			idle++
			continue
		}
		rows = append(rows, r)
	}
	return rows, idle
}

func (m dashModel) selected() *dashRow {
	rows, _ := m.visible()
	if c := m.cursor[m.tab]; c >= 0 && c < len(rows) {
		return rows[c]
	}
	return nil
}

func (m *dashModel) clamp() {
	rows, _ := m.visible()
	m.cursor[m.tab] = max(min(m.cursor[m.tab], len(rows)-1), 0)
}

func (m *dashModel) stopLive() {
	if m.live != nil {
		m.live()
		m.live = nil
	}
}

func (m dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	if dm, ok := next.(dashModel); ok {
		dm.scroll()
		next = dm
	}
	return next, cmd
}

func (m dashModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case snapMsg:
		m.loading = false
		m.err = msg.err
		if msg.res != nil {
			m.res, m.found = msg.res, msg.found
		}
		if msg.notes != nil {
			m.notes = msg.notes
		}
		if msg.snap != nil {
			m.snap = msg.snap
			m.clamp()
		}
		if msg.err != nil && isAccessDenied(msg.err) {
			return m, nil // retrying cannot help; the header shows why
		}
		return m, m.tick()

	case tickMsg:
		if m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.refresh()

	case logsMsg:
		if msg.gen == m.logGen {
			m.logLines, m.logErr = msg.lines, msg.err
		}
		return m, nil

	case liveMsg:
		if msg.gen != m.logGen {
			return m, nil
		}
		m.logLines = append(m.logLines, msg.line)
		if over := len(m.logLines) - 200; over > 0 {
			m.logLines = m.logLines[over:]
		}
		return m, waitLive(msg.gen, msg.ch)

	case liveDoneMsg:
		return m, nil

	case tea.MouseMsg:
		return m.mouse(msg)

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m dashModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch msg.Type {
		case tea.KeyEnter:
			m.filtering = false
		case tea.KeyEsc:
			m.filtering, m.filter = false, ""
		case tea.KeyBackspace:
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(msg.Runes)
		}
		m.cursor[m.tab], m.offset[m.tab] = 0, 0
		return m, nil
	}
	rows, _ := m.visible()
	switch msg.String() {
	case "q", "ctrl+c":
		m.stopLive()
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "esc":
		switch {
		case m.help:
			m.help = false
		case m.logOpen:
			m.stopLive()
			m.logOpen = false
			m.logGen++
		default:
			m.filter = ""
		}
	case "up", "k":
		m.cursor[m.tab] = max(m.cursor[m.tab]-1, 0)
	case "down", "j":
		m.cursor[m.tab] = min(m.cursor[m.tab]+1, max(len(rows)-1, 0))
	case "pgup":
		m.cursor[m.tab] = max(m.cursor[m.tab]-10, 0)
	case "pgdown":
		m.cursor[m.tab] = min(m.cursor[m.tab]+10, max(len(rows)-1, 0))
	case "home", "g":
		m.cursor[m.tab] = 0
	case "end", "G":
		m.cursor[m.tab] = max(len(rows)-1, 0)
	case "tab", "right", "L":
		m.tab = (m.tab + 1) % numKinds
	case "shift+tab", "left", "H":
		m.tab = (m.tab + numKinds - 1) % numKinds
	case "1", "2", "3", "4":
		m.tab = dashKind(msg.String()[0] - '1')
	case "/":
		m.filtering = true
	case "i":
		m.showIdle = !m.showIdle
		m.clamp()
	case "r":
		if !m.loading {
			m.loading = true
			return m, m.refresh()
		}
	case "w":
		i := 0
		for j, w := range dashWindows {
			if w == m.o.window {
				i = j
			}
		}
		m.o.window = dashWindows[(i+1)%len(dashWindows)]
		if !m.loading {
			m.loading = true
			return m, m.refresh()
		}
	case "enter":
		return m.openLogs(false)
	case "a":
		if m.logOpen {
			m.logAll = !m.logAll
			return m.openLogs(m.live != nil)
		}
	case "l":
		return m.openLogs(m.live == nil)
	}
	m.clamp()
	return m, nil
}

// mouse: wheel scrolls the table, a click selects a tab or a row.
func (m dashModel) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	rows, _ := m.visible()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.cursor[m.tab] = max(m.cursor[m.tab]-1, 0)
	case tea.MouseButtonWheelDown:
		m.cursor[m.tab] = min(m.cursor[m.tab]+1, max(len(rows)-1, 0))
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		lay := m.layout()
		switch {
		case msg.Y == lay.tabsY:
			x := 0
			for k := range numKinds {
				w := lipgloss.Width(m.tabLabel(k))
				if msg.X >= x && msg.X < x+w {
					m.tab = k
				}
				x += w + 1
			}
		case msg.Y > lay.tableY && msg.Y <= lay.tableY+lay.tableRows:
			if i := m.offset[m.tab] + msg.Y - lay.tableY - 1; i < len(rows) {
				m.cursor[m.tab] = i
			}
		}
	}
	return m, nil
}

// openLogs fills the bottom pane for the selected resource. live=true streams a Lambda's log group.
func (m dashModel) openLogs(live bool) (tea.Model, tea.Cmd) {
	r := m.selected()
	if r == nil {
		return m, nil
	}
	m.stopLive()
	m.logOpen, m.logErr, m.logLines = true, nil, nil
	m.logGen++
	gen, ctx, d, n, all, window := m.logGen, m.ctx, m.d, m.o.logLines, m.logAll, m.o.window
	switch r.Kind {
	case kindLambda:
		group := "/aws/lambda/" + r.Name
		if live {
			m.logTitle = "live · " + r.Name
			lctx, cancel := context.WithCancel(ctx)
			m.live = cancel
			return m, startLive(lctx, gen, d, group, all)
		}
		m.logTitle = map[bool]string{false: "recent errors · ", true: "recent logs · "}[all] + r.Name
		return m, func() tea.Msg {
			lines, err := recentLogs(ctx, d, group, window, n, all)
			return logsMsg{gen, lines, err}
		}
	case kindSFN:
		m.logTitle = "recent executions · " + r.Name
		arn := r.Arn
		return m, func() tea.Msg {
			lines, err := recentExecutions(ctx, d, arn, n)
			return logsMsg{gen, lines, err}
		}
	case kindSQS:
		m.logTitle = "queue · " + r.Name
		m.logLines = []string{
			fmt.Sprintf("visible %s · oldest %s · sent %s · received %s in the last %s",
				humanNum(r.v("vis")), humanAge(r.v("age")), humanNum(r.v("sent")), humanNum(r.v("recv")), shortDuration(window)),
			"peek at messages without consuming them:  edbg dlq-inspect " + r.Name,
		}
	case kindRule:
		m.logTitle = "rule · " + r.Name
		m.logLines = []string{
			fmt.Sprintf("matched %s · invoked targets %s · failed %s in the last %s",
				humanNum(r.v("match")), humanNum(r.v("inv")), humanNum(r.v("fail")), shortDuration(window)),
			"see the events themselves:  edbg tail --bus " + m.o.bus,
		}
	}
	return m, nil
}

// recentLogs returns the newest n lines of a log group within the window, errors only unless all.
// FilterLogEvents reads oldest first, so when a busy group has more than a few pages in the window,
// it re-reads just the last five minutes to reach the newest lines.
func recentLogs(ctx context.Context, d dashDeps, group string, window time.Duration, n int, all bool) ([]string, error) {
	read := func(since time.Duration) ([]string, bool, error) {
		in := &cloudwatchlogs.FilterLogEventsInput{LogGroupName: aws.String(group), StartTime: aws.Int64(time.Now().Add(-since).UnixMilli())}
		if !all {
			in.FilterPattern = aws.String(logErrorFilter)
		}
		var lines []string
		for page := 0; page < 10; page++ {
			out, err := d.logs.FilterLogEvents(ctx, in)
			if err != nil {
				return nil, false, err
			}
			for _, e := range out.Events {
				level, text := classifyLog(aws.ToString(e.Message))
				if level == "dim" && !all {
					continue
				}
				ts := time.UnixMilli(aws.ToInt64(e.Timestamp)).Local().Format("15:04:05")
				lines = append(lines, ts+"  "+levelTag(level)+" "+firstLines(text, 2))
			}
			if out.NextToken == nil {
				return lines, true, nil
			}
			in.NextToken = out.NextToken
		}
		return lines, false, nil
	}
	lines, complete, err := read(window)
	if err == nil && !complete && window > 5*time.Minute {
		lines, _, err = read(5 * time.Minute)
	}
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return []string{map[bool]string{false: "no errors in this window", true: "no log lines in this window"}[all]}, nil
	}
	return lines[max(len(lines)-n, 0):], nil
}

func recentExecutions(ctx context.Context, d dashDeps, arn string, n int) ([]string, error) {
	out, err := d.sfn.ListExecutions(ctx, &sfn.ListExecutionsInput{StateMachineArn: aws.String(arn), MaxResults: int32(min(n, 100))})
	if err != nil {
		return nil, err
	}
	if len(out.Executions) == 0 {
		return []string{"no executions"}, nil
	}
	var lines []string
	details := 0
	for _, ex := range out.Executions {
		started := aws.ToTime(ex.StartDate)
		line := fmt.Sprintf("%s  %-9s %s", started.Local().Format("01-02 15:04:05"), ex.Status, aws.ToString(ex.Name))
		if ex.StopDate != nil {
			line += fmt.Sprintf("  (%s)", ex.StopDate.Sub(started).Round(time.Millisecond))
		}
		switch ex.Status {
		case "FAILED", "TIMED_OUT", "ABORTED":
			line = "[E] " + line
			if details < 3 { // each cause costs a history call
				details++
				if cause := failureDetail(ctx, d.sfn, aws.ToString(ex.ExecutionArn)); cause != "" {
					line += "  " + firstLines(cause, 1)
				}
			}
		case "RUNNING":
			line = "[R] " + line
		default:
			line = "[ ] " + line
		}
		lines = append(lines, line)
	}
	sort.SliceStable(lines, func(i, j int) bool { return strings.HasPrefix(lines[i], "[E]") && !strings.HasPrefix(lines[j], "[E]") })
	return lines, nil
}

func levelTag(level string) string {
	switch level {
	case "error":
		return "[E]"
	case "warn":
		return "[W]"
	case "ok":
		return "[✓]"
	default:
		return "   "
	}
}

func firstLines(s string, n int) string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	if len(parts) > n {
		return strings.Join(parts[:n], " ⏎ ") + " …"
	}
	return strings.Join(parts, " ⏎ ")
}

// startLive streams one log group through the same poller `watch` uses.
func startLive(ctx context.Context, gen int, d dashDeps, group string, all bool) tea.Cmd {
	ch := make(chan string, 256)
	src := &logsSource{api: d.logs, kind: "lambda", groups: []string{group}, trim: "/aws/lambda/",
		since: time.Now().Add(-time.Minute), verbose: all, interval: 2 * time.Second}
	go func() {
		defer close(ch)
		_ = src.Run(ctx, func(e tailEvent) {
			if e.Level == "dim" && !all {
				return
			}
			select {
			case ch <- clock(e.Time) + "  " + levelTag(e.Level) + " " + firstLines(e.Text, 2):
			case <-ctx.Done():
			}
		})
	}()
	return waitLive(gen, ch)
}

func waitLive(gen int, ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return liveDoneMsg{gen}
		}
		return liveMsg{gen, line, ch}
	}
}

// ---- layout & view ----

type dashLayout struct {
	chart, cards             bool
	chartH                   int
	tabsY, tableY, tableRows int
	logH                     int
}

// layout decides which panels fit. Screen rows, top to bottom: header, cards (4), chart (chartH+3),
// tabs, table header, table rows, log pane (logH+2), footer.
func (m dashModel) layout() dashLayout {
	l := dashLayout{cards: m.h >= 20, chart: m.h >= 34, chartH: 6}
	y := 1
	if l.cards {
		y += 4
	}
	if l.chart {
		y += l.chartH + 3
	}
	l.tabsY = y
	l.tableY = y + 1 // table header; rows start below it
	rest := m.h - l.tableY - 1 - 1
	if m.logOpen {
		l.logH = min(m.o.logLines, max(rest/2-2, 3))
		rest -= l.logH + 2
	}
	l.tableRows = max(rest, 1)
	return l
}

// scroll keeps the cursor inside the visible table rows.
func (m *dashModel) scroll() {
	rows := m.layout().tableRows
	off, cur := m.offset[m.tab], m.cursor[m.tab]
	if cur < off {
		off = cur
	}
	if cur >= off+rows {
		off = cur - rows + 1
	}
	m.offset[m.tab] = max(off, 0)
}

func (m dashModel) tabLabel(k dashKind) string {
	n := 0
	if m.snap != nil {
		n = len(m.snap.Rows[k]) - m.snap.Idle[k]
	}
	label := fmt.Sprintf("%d %s %d", k+1, kindTitles[k], n)
	if m.snap != nil {
		for _, r := range m.snap.Rows[k] {
			if bad, _ := rowStatus(r); bad {
				label += " ✖"
				break
			}
		}
	}
	if k == m.tab {
		return m.st.tabOn.Render(label)
	}
	return m.st.tabOff.Render(label)
}

func (m dashModel) View() string {
	if m.w == 0 {
		return ""
	}
	if m.w < 80 || m.h < 12 {
		return fmt.Sprintf("Terminal too small (%d×%d); need at least 80×12.\nTry `edbg dash --once`, or press q.", m.w, m.h)
	}
	st := m.st
	var out []string
	out = append(out, m.header())
	if m.snap == nil {
		msg := "discovering resources and fetching metrics…"
		if m.err != nil {
			msg = st.bad.Render("Error: " + friendlyError(m.err))
		}
		out = append(out, "", "  "+msg)
		for _, n := range m.notes {
			out = append(out, "  "+st.warn.Render("note: "+n))
		}
		return strings.Join(out, "\n")
	}
	if m.help {
		return strings.Join(append(out, m.helpView()...), "\n")
	}
	lay := m.layout()
	if lay.cards {
		out = append(out, renderCards(st, dashCards(m.snap), m.w))
	}
	rows, idle := m.visible()
	sel := m.selected()
	if lay.chart {
		title := fmt.Sprintf("%s · all %s", shortDuration(m.snap.Window), kindTitles[m.tab])
		series := m.snap.Total[m.tab]
		if sel != nil {
			title, series = fmt.Sprintf("%s · %s", shortDuration(m.snap.Window), sel.Name), sel.Series
		}
		out = append(out, renderChart(st, title, m.tab, series, m.w, lay.chartH))
	}
	tabs := make([]string, numKinds)
	for k := range numKinds {
		tabs[k] = m.tabLabel(k)
	}
	out = append(out, strings.Join(tabs, " "))

	table := renderTable(st, m.tab, rows, m.w, m.cursor[m.tab], m.offset[m.tab], lay.tableRows)
	switch {
	case len(rows) == 0 && idle > 0:
		table = append(table, st.dim.Render(fmt.Sprintf("   all %d are idle in this window · i to show them", idle)))
	case len(rows) == 0:
		table = append(table, st.dim.Render("   nothing matched"))
	case idle > 0 && len(table)-1 < lay.tableRows:
		table = append(table, st.dim.Render(fmt.Sprintf("   … %d idle · i to show", idle)))
	}
	for len(table) < lay.tableRows+1 {
		table = append(table, "")
	}
	out = append(out, table[:lay.tableRows+1]...)

	if m.logOpen {
		out = append(out, m.logPane(lay.logH))
	}
	out = append(out, m.footer())
	return strings.Join(out, "\n")
}

func (m dashModel) header() string {
	st := m.st
	left := st.title.Render("edbg dash") + st.dim.Render(fmt.Sprintf(" · %s · %s · last %s", m.o.profile, m.o.region, shortDuration(m.o.window)))
	if m.o.name != "" {
		left += st.dim.Render(" · name ") + m.o.name
	}
	right := ""
	switch {
	case m.loading:
		right = st.accent.Render("↻ refreshing…")
	case m.err != nil && m.snap != nil:
		right = st.bad.Render("refresh failed: " + truncate(friendlyError(m.err), 50))
	case m.snap != nil:
		perHour := float64(m.snap.Metrics) * metricPrice * float64(time.Hour/m.o.refresh)
		right = st.dim.Render(fmt.Sprintf("data to %s · %s metrics/refresh ≈ $%.2f/h",
			m.snap.End.Local().Format("15:04"), humanNum(float64(m.snap.Metrics)), perHour))
	}
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return fit(left, m.w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m dashModel) logPane(h int) string {
	st := m.st
	var lines []string
	switch {
	case m.logErr != nil:
		lines = []string{st.bad.Render(friendlyError(m.logErr))}
	case m.logLines == nil && m.live != nil:
		lines = []string{st.dim.Render("waiting for new log lines…")}
	case m.logLines == nil:
		lines = []string{st.dim.Render("loading…")}
	default:
		for _, l := range m.logLines[max(len(m.logLines)-h, 0):] {
			lines = append(lines, paintLogLine(st, l))
		}
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return boxed(st.box, m.logTitle, lines, m.w)
}

func paintLogLine(st dashStyles, l string) string {
	switch {
	case strings.Contains(l, "[E]"):
		return st.bad.Render(strings.Replace(l, "[E] ", "✖ ", 1))
	case strings.Contains(l, "[W]"):
		return st.warn.Render(strings.Replace(l, "[W] ", "⚠ ", 1))
	case strings.Contains(l, "[✓]"):
		return st.good.Render(strings.Replace(l, "[✓] ", "✔ ", 1))
	case strings.HasPrefix(l, "[R] "):
		return st.accent.Render("▶ " + l[4:])
	case strings.HasPrefix(l, "[ ] "):
		return "  " + l[4:]
	}
	return l
}

func (m dashModel) footer() string {
	st := m.st
	if m.filtering {
		return st.accent.Render("filter: ") + m.filter + st.cursor.Render("█") + st.dim.Render("   enter keep · esc clear")
	}
	keys := "↑↓ select · enter errors · l live · tab switch · / filter · i idle · w window · r refresh · ? help · q quit"
	if m.logOpen {
		keys = "a all lines/errors only · l live on/off · esc close · " + keys
	}
	if m.filter != "" {
		keys = "filter \"" + m.filter + "\" · " + keys
	}
	for _, n := range m.notes {
		keys = "note: " + n + " · " + keys
	}
	return st.dim.Render(fit(keys, m.w))
}

func (m dashModel) helpView() []string {
	st := m.st
	lines := []string{"",
		st.bold.Render("  What you are looking at"),
		"  CloudWatch metrics for every matching Lambda, state machine, queue and EventBridge rule,",
		"  for the selected window plus the window before it (the ▲▼ trends). Rows needing attention come",
		"  first: errors, then dead-letter messages and throttling, then traffic. Idle resources are hidden.",
		"  Metrics arrive 1-3 minutes late, so this shows trends; use enter or l for second-by-second logs.",
		"",
		st.bold.Render("  Keys"),
		"  ↑/↓ j/k  select row         enter  recent errors (Lambda) or executions (Step Functions)",
		"  tab 1-4  switch service     a      in the log pane: all lines vs errors only",
		"  /        filter by name     l      live-stream the selected Lambda's logs",
		"  i        show idle rows     w      cycle window 5m · 15m · 1h · 3h · 24h",
		"  r        refresh now        esc    close pane / clear filter      q  quit",
		"",
		st.bold.Render("  Cost"),
		"  CloudWatch bills GetMetricData per metric requested (about $0.01 per 1,000). Idle resources",
		"  cost one or two metrics each per refresh; narrow with --name or slow down with --refresh.",
		"", st.dim.Render("  press ? or esc to go back"),
	}
	return lines
}
