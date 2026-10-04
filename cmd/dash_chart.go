package cmd

import (
	"fmt"
	"math"
	"strings"
	"time"
)

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// resample fits xs into width points, keeping each bucket's max so spikes survive shrinking.
// NaN (no datapoint) counts as zero.
func resample(xs []float64, width int) []float64 {
	if width <= 0 || len(xs) == 0 {
		return nil
	}
	out := make([]float64, width)
	for i := range out {
		lo := i * len(xs) / width
		hi := max((i+1)*len(xs)/width, lo+1)
		m := 0.0
		for _, x := range xs[lo:min(hi, len(xs))] {
			if !math.IsNaN(x) && x > m {
				m = x
			}
		}
		out[i] = m
	}
	return out
}

// sparkline draws xs in at most width cells; a flat zero series stays at the baseline.
func sparkline(xs []float64, width int) string {
	if len(xs) == 0 || width <= 0 {
		return ""
	}
	pts := resample(xs, min(width, len(xs)))
	top := 0.0
	for _, x := range pts {
		top = math.Max(top, x)
	}
	var b strings.Builder
	for _, x := range pts {
		i := 0
		if top > 0 && x > 0 {
			i = max(1, int(math.Round(x/top*float64(len(sparkRunes)-1))))
		}
		b.WriteRune(sparkRunes[i])
	}
	return b.String()
}

// brailleCanvas packs a 2×4 dot grid into each terminal cell.
type brailleCanvas struct {
	w, h  int // in cells
	dots  [][]uint8
	owner [][]int // which series last drew in a cell, for coloring; -1 = empty
}

func newBrailleCanvas(w, h int) *brailleCanvas {
	c := &brailleCanvas{w: w, h: h, dots: make([][]uint8, h), owner: make([][]int, h)}
	for y := range h {
		c.dots[y] = make([]uint8, w)
		c.owner[y] = make([]int, w)
		for x := range c.owner[y] {
			c.owner[y][x] = -1
		}
	}
	return c
}

var brailleBits = [4][2]uint8{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}

func (c *brailleCanvas) set(px, py, series int) {
	if px < 0 || py < 0 || px >= c.w*2 || py >= c.h*4 {
		return
	}
	cx, cy := px/2, py/4
	c.dots[cy][cx] |= brailleBits[py%4][px%2]
	c.owner[cy][cx] = series
}

// plot draws a series as a connected line scaled to top.
func (c *brailleCanvas) plot(xs []float64, top float64, series int) {
	pw, ph := c.w*2, c.h*4
	pts := resample(xs, pw)
	if top <= 0 {
		top = 1
	}
	y := func(v float64) int { return ph - 1 - int(math.Round(v/top*float64(ph-1))) }
	for x := range pts {
		y1 := y(pts[x])
		y0 := y1
		if x > 0 {
			y0 = y(pts[x-1])
		}
		lo, hi := min(y0, y1), max(y0, y1)
		for yy := lo; yy <= hi; yy++ { // vertical fill keeps steep changes connected
			c.set(x, yy, series)
		}
	}
}

func (c *brailleCanvas) rows(paint func(series int, s string) string) []string {
	out := make([]string, c.h)
	for y := range c.h {
		var b strings.Builder
		for x := range c.w {
			if c.dots[y][x] == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteString(paint(c.owner[y][x], string(rune(0x2800+int(c.dots[y][x])))))
		}
		out[y] = b.String()
	}
	return out
}

// lineChart renders series on a shared y axis with a labelled left gutter. The last series is
// drawn on top, so pass the one that matters most (errors) last.
func lineChart(series [][]float64, width, height int, paint func(series int, s string) string) []string {
	top := 0.0
	for _, xs := range series {
		for _, x := range xs {
			if !math.IsNaN(x) {
				top = math.Max(top, x)
			}
		}
	}
	label := humanNum(top)
	gutter := max(len(label), 1) + 1
	c := newBrailleCanvas(max(width-gutter-1, 4), max(height, 2))
	for i, xs := range series {
		c.plot(xs, top, i)
	}
	rows := c.rows(paint)
	for i := range rows {
		l := ""
		switch i {
		case 0:
			l = label
		case len(rows) - 1:
			l = "0"
		}
		rows[i] = fmt.Sprintf("%*s ┤", gutter-1, l) + rows[i]
	}
	return rows
}

// humanNum prints 1234 as 1.2k and keeps small values exact.
func humanNum(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return fmt.Sprintf("%.1fG", v/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case a >= 1e4:
		return fmt.Sprintf("%.0fk", v/1e3)
	case a >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case a == math.Trunc(a):
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.1f", v)
	}
}

func humanAge(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	return shortDuration(time.Duration(seconds) * time.Second)
}

func shortDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour && int(d.Minutes())%60 == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// trend compares a window with the previous one: "▲12%", "▼3%", "new" or "".
func trend(cur, prev float64) string {
	switch {
	case cur == prev:
		return ""
	case prev == 0:
		return "new"
	}
	pct := (cur - prev) / prev * 100
	if math.Abs(pct) < 1 {
		return ""
	}
	if pct > 0 {
		return fmt.Sprintf("▲%.0f%%", pct)
	}
	return fmt.Sprintf("▼%.0f%%", -pct)
}

func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole * 100
}
