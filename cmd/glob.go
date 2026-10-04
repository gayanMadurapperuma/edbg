package cmd

import (
	"regexp"
	"strings"
)

// globMatch matches s against a pattern where * is any run of characters and ? is one character.
func globMatch(pattern, s string) bool {
	re := "^" + strings.NewReplacer(`\*`, `.*`, `\?`, `.`).Replace(regexp.QuoteMeta(pattern)) + "$"
	ok, err := regexp.MatchString(re, s)
	return err == nil && ok
}

// literalPrefix is the part of a glob before its first wildcard, usable as an AWS name-prefix filter.
func literalPrefix(pattern string) string {
	if i := strings.IndexAny(pattern, "*?"); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// longestLiteral is the longest wildcard-free run in a glob, usable as a server-side "contains" filter.
func longestLiteral(pattern string) string {
	best := ""
	for _, part := range strings.FieldsFunc(pattern, func(r rune) bool { return r == '*' || r == '?' }) {
		if len(part) > len(best) {
			best = part
		}
	}
	return best
}
