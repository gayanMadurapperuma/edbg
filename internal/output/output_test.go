package output

import (
	"strings"
	"testing"
)

func TestPrettyJSON(t *testing.T) {
	if got, want := PrettyJSON(`{"a":1}`), "{\n  \"a\": 1\n}"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := PrettyJSON("plain text"); got != "plain text" {
		t.Errorf("non-JSON should pass through, got %q", got)
	}
}

func TestColorToggle(t *testing.T) {
	defer func(v bool) { ColorEnabled = v }(ColorEnabled)
	ColorEnabled = false
	if Red("x") != "x" {
		t.Error("color should be off")
	}
	ColorEnabled = true
	if Red("x") != "\x1b[31mx\x1b[0m" {
		t.Errorf("got %q", Red("x"))
	}
}

func TestColorJSON(t *testing.T) {
	defer func(v bool) { ColorEnabled = v }(ColorEnabled)
	in := `{"k":"v \"q\"","n":-1.5e3,"b":true,"z":null}`

	ColorEnabled = false
	if got := ColorJSON(in); got != PrettyJSON(in) {
		t.Errorf("without color should equal PrettyJSON, got %q", got)
	}
	ColorEnabled = true
	got := ColorJSON(in)
	for _, want := range []string{Cyan(`"k"`), Green(`"v \"q\""`), Yellow("-1.5e3"), Magenta("true"), Magenta("null")} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if ColorJSON("not json") != "not json" {
		t.Error("non-JSON must pass through")
	}
}
