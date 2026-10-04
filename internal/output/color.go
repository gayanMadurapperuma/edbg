package output

import "fmt"

// ColorEnabled is set once at startup by the root command.
var ColorEnabled = true

func paint(code, s string) string {
	if !ColorEnabled {
		return s
	}
	return fmt.Sprintf("\x1b[%sm%s\x1b[0m", code, s)
}

func Red(s string) string    { return paint("31", s) }
func Green(s string) string  { return paint("32", s) }
func Yellow(s string) string { return paint("33", s) }
func Cyan(s string) string   { return paint("36", s) }
func Dim(s string) string    { return paint("2", s) }

func Bold(s string) string    { return paint("1", s) }
func Magenta(s string) string { return paint("35", s) }
