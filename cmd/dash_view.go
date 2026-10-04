package cmd

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Basic ANSI colors, so the terminal's own theme decides the exact shades in light and dark mode.
type dashStyles struct {
	box, boxBad, title, dim, bold, accent, bad, warn, good, cursor, tabOn, tabOff lipgloss.Style
}

func newDashStyles() dashStyles {
	border := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8")).Padding(0, 1)
	return dashStyles{
		box:    border,
		boxBad: border.BorderForeground(lipgloss.Color("1")),
		title:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		dim:    lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		bold:   lipgloss.NewStyle().Bold(true),
		accent: lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
		bad:    lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
		warn:   lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		good:   lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		cursor: lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true),
		tabOn:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6")).Padding(0, 1),
		tabOff: lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Padding(0, 1),
	}
}

// boxed draws a titled panel of exactly width columns.
func boxed(st lipgloss.Style, title string, lines []string, width int) string {
	inner := max(width-4, 1) // border + padding
	body := make([]string, len(lines))
	for i, l := range lines {
		body[i] = fit(l, inner)
	}
	out := st.Width(width - 2).Render(strings.Join(body, "\n"))
	if title == "" {
		return out
	}
	// Splice the title into the top border: "╭─ title ───╮".
	rows := strings.SplitN(out, "\n", 2)
	top := []rune(stripANSI(rows[0]))
	t := []rune("─ " + title + " ")
	if len(t)+2 < len(top) {
		copy(top[1:], t)
	}
	rows[0] = st.UnsetPadding().UnsetBorderStyle().Foreground(st.GetBorderTopForeground()).Render(string(top))
	return strings.Join(rows, "\n")
}

// fit pads or truncates a possibly styled string to exactly w visible columns.
func fit(s string, w int) string {
	vw := lipgloss.Width(s)
	if vw == w {
		return s
	}
	if vw < w {
		return s + strings.Repeat(" ", w-vw)
	}
	plain := []rune(stripANSI(s))
	return truncate(string(plain), w) + strings.Repeat(" ", max(w-len([]rune(truncate(string(plain), w))), 0))
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			in = true
		case in && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func padL(s string, w int) string {
	if n := lipgloss.Width(s); n < w {
		return strings.Repeat(" ", w-n) + s
	}
	return s
}

// ---- KPI cards ----

type dashCard struct {
	title, value, sub string
	spark             []float64
	bad               bool
}

func dashCards(s *dashSnapshot) []dashCard {
	l, sf, q := s.Cur[kindLambda], s.Cur[kindSFN], s.Cur[kindSQS]
	lp, sp := s.Prev[kindLambda], s.Prev[kindSFN]
	dlq, dlqPrev := 0.0, 0.0
	for _, r := range s.Rows[kindSQS] {
		if r.DLQ {
			dlq += r.Cur["vis"]
			dlqPrev += r.Prev["vis"]
		}
	}
	queues := dashCard{title: "Queue backlog", value: humanNum(q["vis"]), sub: "oldest " + humanAge(q["age"]), spark: s.Total[kindSQS]["vis"]}
	if dlq > 0 {
		queues = dashCard{title: "Dead-letter queues", value: humanNum(dlq) + " msgs", sub: trend(dlq, dlqPrev), spark: s.Total[kindSQS]["vis"], bad: true}
	}
	failed := sf["fail"] + sf["tout"]
	return []dashCard{
		{title: "Lambda invocations", value: humanNum(l["inv"]), sub: trend(l["inv"], lp["inv"]), spark: s.Total[kindLambda]["inv"]},
		{title: "Lambda errors", value: humanNum(l["err"]), sub: fmt.Sprintf("%.1f%%  %s", pct(l["err"], l["inv"]), trend(l["err"], lp["err"])),
			spark: s.Total[kindLambda]["err"], bad: l["err"] > 0},
		{title: "Step Functions", value: fmt.Sprintf("%s failed", humanNum(failed)), sub: fmt.Sprintf("%s started  %s", humanNum(sf["start"]), trend(failed, sp["fail"]+sp["tout"])),
			spark: s.Total[kindSFN]["fail"], bad: failed > 0},
		queues,
	}
}

func renderCards(st dashStyles, cards []dashCard, width int) string {
	n := len(cards)
	w := width / n
	parts := make([]string, n)
	for i, c := range cards {
		cw := w
		if i == n-1 {
			cw = width - w*(n-1)
		}
		box, val := st.box, st.bold
		if c.bad {
			box, val = st.boxBad, st.bad
		}
		spark := st.accent.Render(sparkline(c.spark, cw-4))
		if c.bad {
			spark = st.bad.Render(sparkline(c.spark, cw-4))
		}
		lines := []string{val.Render(c.value) + "  " + st.dim.Render(c.sub), spark}
		parts[i] = boxed(box, c.title, lines, cw)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// ---- tables ----

type dashCol struct {
	title string
	w     int
	cell  func(r *dashRow) string
}

func rowStatus(r *dashRow) (bad, warn bool) {
	switch r.Kind {
	case kindLambda:
		return r.v("err") > 0, r.v("thr") > 0
	case kindSFN:
		return r.v("fail")+r.v("tout") > 0, false
	case kindSQS:
		return r.DLQ && r.v("vis") > 0, r.v("age") > 300
	case kindRule:
		return r.v("fail") > 0, false
	}
	return false, false
}

func dashColumns(st dashStyles, k dashKind) []dashCol {
	num := func(key string) func(*dashRow) string {
		return func(r *dashRow) string { return humanNum(r.v(key)) }
	}
	red := func(key string) func(*dashRow) string {
		return func(r *dashRow) string {
			if r.v(key) > 0 {
				return st.bad.Render(humanNum(r.v(key)))
			}
			return st.dim.Render("0")
		}
	}
	spark := func(key, badKey string) func(*dashRow) string {
		return func(r *dashRow) string {
			if badKey != "" && r.v(badKey) > 0 {
				return st.bad.Render(sparkline(r.Series[badKey], 16))
			}
			return st.accent.Render(sparkline(r.Series[key], 16))
		}
	}
	switch k {
	case kindLambda:
		return []dashCol{
			{"INVOKES", 8, num("inv")}, {"ERRORS", 7, red("err")},
			{"ERR%", 6, func(r *dashRow) string {
				p := pct(r.v("err"), r.v("inv"))
				if p == 0 {
					return st.dim.Render("-")
				}
				return st.bad.Render(fmt.Sprintf("%.1f", p))
			}},
			{"THROTTL", 8, func(r *dashRow) string {
				if r.v("thr") > 0 {
					return st.warn.Render(humanNum(r.v("thr")))
				}
				return st.dim.Render("0")
			}},
			{"P95 MS", 7, func(r *dashRow) string {
				if r.v("p95") == 0 {
					return st.dim.Render("-")
				}
				return fmt.Sprintf("%.0f", math.Round(r.v("p95")))
			}},
			{"TREND", 17, spark("inv", "err")},
		}
	case kindSFN:
		return []dashCol{
			{"STARTED", 8, num("start")}, {"OK", 7, num("ok")}, {"FAILED", 7, red("fail")}, {"TIMEOUT", 8, red("tout")},
			{"TREND", 17, spark("start", "fail")},
		}
	case kindSQS:
		return []dashCol{
			{"VISIBLE", 8, func(r *dashRow) string {
				if r.DLQ && r.v("vis") > 0 {
					return st.bad.Render(humanNum(r.v("vis")))
				}
				return humanNum(r.v("vis"))
			}},
			{"OLDEST", 7, func(r *dashRow) string {
				if r.v("age") > 300 {
					return st.warn.Render(humanAge(r.v("age")))
				}
				return humanAge(r.v("age"))
			}},
			{"SENT", 7, num("sent")}, {"RECV", 7, num("recv")},
			{"TREND", 17, spark("vis", "")},
		}
	default:
		return []dashCol{
			{"MATCHED", 8, num("match")}, {"INVOKED", 8, num("inv")}, {"FAILED", 7, red("fail")},
			{"TREND", 17, spark("match", "fail")},
		}
	}
}

// renderTable draws header plus rows[offset:offset+height]; cursor < 0 hides the selection marker.
func renderTable(st dashStyles, k dashKind, rows []*dashRow, width, cursor, offset, height int) []string {
	cols := dashColumns(st, k)
	fixed := 0
	for _, c := range cols {
		fixed += c.w + 1
	}
	nameW := max(width-fixed-3, 12)
	var hdr strings.Builder
	hdr.WriteString("   " + fit("NAME", nameW))
	for _, c := range cols {
		if c.title == "TREND" {
			hdr.WriteString(" " + fit(c.title, c.w))
		} else {
			hdr.WriteString(" " + padL(c.title, c.w))
		}
	}
	out := []string{st.dim.Render(fit(hdr.String(), width))}
	for i := offset; i < len(rows) && i < offset+height; i++ {
		r := rows[i]
		mark := "  "
		if bad, warn := rowStatus(r); bad {
			mark = st.bad.Render("✖ ")
		} else if warn {
			mark = st.warn.Render("⚠ ")
		}
		name := fit(truncateMiddle(r.Name, nameW), nameW)
		pointer := " "
		if i == cursor {
			pointer = st.cursor.Render("▸")
			name = st.cursor.Render(name)
		} else if r.Idle {
			name = st.dim.Render(name)
		}
		var b strings.Builder
		b.WriteString(pointer + mark + name)
		for _, c := range cols {
			if c.title == "TREND" {
				b.WriteString(" " + fit(c.cell(r), c.w))
			} else {
				b.WriteString(" " + padL(c.cell(r), c.w))
			}
		}
		out = append(out, fit(b.String(), width))
	}
	return out
}

// chartSeries picks what the main chart shows for a row: traffic, then failures drawn on top.
func chartSeries(k dashKind, series map[string][]float64) (data [][]float64, legend []string) {
	switch k {
	case kindLambda:
		return [][]float64{series["inv"], series["err"]}, []string{"invocations", "errors"}
	case kindSFN:
		return [][]float64{series["start"], addSeries(series["fail"], series["tout"])}, []string{"started", "failed"}
	case kindSQS:
		return [][]float64{series["recv"], series["vis"]}, []string{"received", "visible"}
	default:
		return [][]float64{series["match"], series["fail"]}, []string{"matched", "failed"}
	}
}

func addSeries(a, b []float64) []float64 {
	if a == nil {
		return b
	}
	out := make([]float64, len(a))
	for i := range a {
		out[i] = nz(a[i])
		if i < len(b) {
			out[i] += nz(b[i])
		}
	}
	return out
}

func nz(x float64) float64 {
	if x != x { // NaN
		return 0
	}
	return x
}

func renderChart(st dashStyles, title string, k dashKind, series map[string][]float64, width, height int) string {
	data, legend := chartSeries(k, series)
	paints := []lipgloss.Style{st.accent, st.bad}
	if k == kindSQS {
		paints = []lipgloss.Style{st.accent, st.warn}
	}
	rows := lineChart(data, width-4, height, func(i int, s string) string { return paints[i].Render(s) })
	key := paints[0].Render("── "+legend[0]) + "  " + paints[1].Render("── "+legend[1])
	rows = append(rows, padL(key, width-4))
	return boxed(st.box, title, rows, width)
}
