package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gayanMadurapperuma/edbg/internal/output"
	"golang.org/x/term"
)

const maxBuffered = 1000

// tailEvent is one line of the feed. Kind "event" is an EventBridge envelope; other kinds
// (lambda, logs, sfn, sqs) carry free text in Text and a Level used for coloring.
type tailEvent struct {
	Kind                                          string
	ID, Time, Source, DetailType, Account, Region string
	Resources                                     []string
	Detail                                        json.RawMessage
	Raw                                           string
	Text, Level                                   string
}

// reSecret matches JSON string fields that carry credentials (CloudTrail AssumeRole responses,
// Authorization headers...). Values are masked before anything is printed.
var reSecret = regexp.MustCompile(`(?i)("(?:sessionToken|secretAccessKey|accessKeyId|password|authorization|x-amz-security-token)"\s*:\s*)"(?:[^"\\]|\\.)*"`)

func redact(s string) string { return reSecret.ReplaceAllString(s, `$1"****"`) }

func parseTailEvent(body string) tailEvent {
	body = redact(body)
	var ev struct {
		ID         string          `json:"id"`
		Time       string          `json:"time"`
		Source     string          `json:"source"`
		DetailType string          `json:"detail-type"`
		Account    string          `json:"account"`
		Region     string          `json:"region"`
		Resources  []string        `json:"resources"`
		Detail     json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		return tailEvent{Kind: "event", Raw: body}
	}
	return tailEvent{Kind: "event", ID: ev.ID, Time: ev.Time, Source: ev.Source, DetailType: ev.DetailType,
		Account: ev.Account, Region: ev.Region, Resources: ev.Resources, Detail: ev.Detail, Raw: body}
}

func clock(t string) string {
	if ts, err := time.Parse(time.RFC3339, t); err == nil {
		return ts.Local().Format("15:04:05")
	}
	return t
}

func truncate(s string, w int) string {
	r := []rune(s)
	if w <= 0 || len(r) <= w {
		return s
	}
	return string(r[:max(w-1, 0)]) + "…"
}

// truncateMiddle keeps both ends, because resource names usually differ at the end (stack-Fn-ABC123).
func truncateMiddle(s string, w int) string {
	r := []rune(s)
	if w < 8 || len(r) <= w {
		return truncate(s, w)
	}
	tail := (w - 1) / 2
	return string(r[:w-1-tail]) + "…" + string(r[len(r)-tail:])
}

// renderEventBlock draws one event as a gutter-framed block with colorized JSON.
func renderEventBlock(n int, e tailEvent) string {
	var b strings.Builder
	gutter := output.Dim("│ ")
	if e.Source == "" && e.DetailType == "" { // not a recognizable event envelope
		fmt.Fprintf(&b, "%s%s  %s\n", output.Dim("┌─ #"+fmt.Sprint(n)), "", output.Yellow("raw message"))
		for _, l := range strings.Split(e.Raw, "\n") {
			b.WriteString(gutter + l + "\n")
		}
		b.WriteString(output.Dim("└") + "\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%s  %s  %s%s%s\n", output.Dim(fmt.Sprintf("┌─ #%d", n)), output.Dim(clock(e.Time)),
		output.Cyan(e.Source), output.Dim(" › "), output.Bold(e.DetailType))
	var meta []string
	for _, kv := range [][2]string{{"id", e.ID}, {"account", e.Account}, {"region", e.Region}} {
		if kv[1] != "" {
			meta = append(meta, output.Dim(kv[0]+" ")+kv[1])
		}
	}
	if len(meta) > 0 {
		b.WriteString(gutter + strings.Join(meta, "   ") + "\n")
	}
	for _, r := range e.Resources {
		b.WriteString(gutter + output.Dim("resource ") + r + "\n")
	}
	if len(e.Detail) > 0 {
		for _, l := range strings.Split(output.ColorJSON(string(e.Detail)), "\n") {
			b.WriteString(gutter + l + "\n")
		}
	}
	b.WriteString(output.Dim("└") + "\n")
	return b.String()
}

// renderEventLine is the one-line --compact form, cut to the terminal width.
func renderEventLine(e tailEvent, width int) string {
	if e.Source == "" && e.DetailType == "" {
		return truncate(strings.Join(strings.Fields(e.Raw), " "), width) + "\n"
	}
	head := fmt.Sprintf("%s  %s  %s  ", clock(e.Time), e.Source, e.DetailType)
	detail := ""
	if len(e.Detail) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, e.Detail); err == nil {
			detail = buf.String()
		}
	}
	room := width - len([]rune(head))
	return output.Dim(clock(e.Time)) + "  " + output.Cyan(e.Source) + "  " + output.Bold(e.DetailType) + "  " +
		output.Dim(truncate(detail, room)) + "\n"
}

// tailViewer prints events and supports pause/clear. It is safe for use from the key-reader goroutine.
type tailViewer struct {
	mu      sync.Mutex
	w       io.Writer
	compact bool
	tty     bool
	width   int
	start   time.Time
	n       int
	paused  bool
	buf     []tailEvent
	dropped int
}

func newTailViewer(w io.Writer, compact, tty bool, width int) *tailViewer {
	return &tailViewer{w: w, compact: compact, tty: tty, width: width, start: time.Now()}
}

func (v *tailViewer) Header(bus, pattern, rule string, keys bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.say("%s  %s\n", output.Bold("edbg tail"), output.Dim("bus "+bus+" · rule "+rule))
	v.say("%s %s\n", output.Dim("pattern"), pattern)
	if keys {
		v.say("%s\n", output.Dim("p pause · c clear · q quit"))
	} else {
		v.say("%s\n", output.Dim("Ctrl+C to stop"))
	}
	v.say("%s\n\n", output.Dim(strings.Repeat("─", min(max(v.width, 20), 80))))
}

func (v *tailViewer) printLocked(e tailEvent) {
	v.n++
	if e.Kind != "" && e.Kind != "event" {
		_, _ = io.WriteString(v.w, renderFeedLine(e, v.width))
		return
	}
	if v.compact {
		_, _ = io.WriteString(v.w, renderEventLine(e, v.width))
		return
	}
	_, _ = io.WriteString(v.w, renderEventBlock(v.n, e))
}

func (v *tailViewer) Event(e tailEvent) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.paused {
		v.printLocked(e)
		return
	}
	if len(v.buf) >= maxBuffered {
		v.buf = v.buf[1:]
		v.dropped++
	}
	v.buf = append(v.buf, e)
}

func (v *tailViewer) TogglePause() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.paused {
		v.paused = true
		v.say("%s\n", output.Yellow("⏸  paused — events are buffered (press p to resume)"))
		return
	}
	v.paused = false
	v.say("%s\n", output.Green(fmt.Sprintf("▶  resumed — %d buffered", len(v.buf))))
	if v.dropped > 0 {
		v.say("%s\n", output.Yellow(fmt.Sprintf("   (%d older events dropped, buffer limit %d)", v.dropped, maxBuffered)))
	}
	for _, e := range v.buf {
		v.printLocked(e)
	}
	v.buf, v.dropped = nil, 0
}

func (v *tailViewer) Clear() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tty {
		_, _ = io.WriteString(v.w, "\x1b[2J\x1b[H")
	}
}

func (v *tailViewer) Summary() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return fmt.Sprintf("Stopped. %d event(s) shown in %s.", v.n, time.Since(v.start).Round(time.Second))
}

// crlfWriter is needed because raw terminal mode turns off newline translation.
type crlfWriter struct{ w io.Writer }

func (c crlfWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(c.w, strings.ReplaceAll(string(p), "\n", "\r\n"))
	return len(p), err
}

func termWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 120
}

// enableKeys puts the terminal in raw mode and reads single-key commands. It returns a restore func.
func enableKeys(cancel func(), v *tailViewer) (restore func(), ok bool) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}, false
	}
	go func() {
		b := make([]byte, 1)
		for {
			if n, err := os.Stdin.Read(b); err != nil || n == 0 {
				return
			}
			switch b[0] {
			case 'q', 'Q', 3: // 3 = Ctrl+C, which raw mode no longer turns into a signal
				cancel()
				return
			case 'p', 'P', ' ':
				v.TogglePause()
			case 'c', 'C':
				v.Clear()
			}
		}
	}()
	return func() { _ = term.Restore(fd, old) }, true
}

// say writes formatted text to the terminal; a failed terminal write is not actionable.
func (v *tailViewer) say(format string, args ...any) {
	_, _ = fmt.Fprintf(v.w, format, args...)
}

const feedPrefixWidth = 50 // clock(8) + kind(6) + name(30) + separators

func colorLevel(level, s string) string {
	switch level {
	case "error":
		return output.Red(s)
	case "warn":
		return output.Yellow(s)
	case "ok":
		return output.Green(s)
	case "dim":
		return output.Dim(s)
	}
	return s
}

// renderFeedLine prints a lambda/logs/sfn/sqs item: one prefixed line, with continuation lines indented.
func renderFeedLine(e tailEvent, width int) string {
	kind := fmt.Sprintf("%-6s", e.Kind)
	switch e.Kind {
	case "lambda", "logs":
		kind = output.Yellow(kind)
	case "sfn":
		kind = output.Cyan(kind)
	case "sqs":
		kind = output.Magenta(kind)
	}
	prefix := fmt.Sprintf("%s  %s  %-30s  ", output.Dim(clock(e.Time)), kind, truncateMiddle(e.Source, 30))
	room := max(width-feedPrefixWidth, 20)

	lines := strings.Split(strings.TrimRight(e.Text, "\r\n"), "\n")
	const maxLines = 20
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], fmt.Sprintf("… (%d more lines)", len(lines)-maxLines))
	}
	var b strings.Builder
	for i, l := range lines {
		l = colorLevel(e.Level, truncate(strings.TrimRight(l, "\r"), room))
		if i == 0 {
			b.WriteString(prefix)
		} else {
			b.WriteString(strings.Repeat(" ", feedPrefixWidth))
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// jsonLine is the -o json form: the raw envelope for EventBridge events, a small object for other items.
func (e tailEvent) jsonLine() string {
	if e.Kind == "" || e.Kind == "event" {
		return e.Raw
	}
	b, _ := json.Marshal(map[string]string{"kind": e.Kind, "time": e.Time, "name": e.Source, "level": e.Level, "text": e.Text})
	return string(b)
}

// Banner prints the start-up header for commands that watch several sources.
func (v *tailViewer) Banner(title string, info []string, keys bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.say("%s\n", output.Bold(title))
	for _, l := range info {
		v.say("%s\n", output.Dim(l))
	}
	if keys {
		v.say("%s\n", output.Dim("p pause · c clear · q quit"))
	} else {
		v.say("%s\n", output.Dim("Ctrl+C to stop"))
	}
	v.say("%s\n\n", output.Dim(strings.Repeat("─", min(max(v.width, 20), 80))))
}
