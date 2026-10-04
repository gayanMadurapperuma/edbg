package output

import (
	"bytes"
	"encoding/json"
	"strings"
)

// PrettyJSON indents s if it is valid JSON, otherwise returns it unchanged.
func PrettyJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return s
	}
	return buf.String()
}

// ColorJSON pretty-prints s and colors keys, strings, numbers and literals.
// Input that is not valid JSON is returned unchanged.
func ColorJSON(s string) string {
	if !json.Valid([]byte(s)) {
		return s
	}
	p := PrettyJSON(s)
	if !ColorEnabled {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		c := p[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(p) && p[j] != '"' {
				if p[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(p))
			k := j
			for k < len(p) && p[k] == ' ' {
				k++
			}
			if k < len(p) && p[k] == ':' {
				b.WriteString(Cyan(p[i:j]))
			} else {
				b.WriteString(Green(p[i:j]))
			}
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < len(p) && strings.IndexByte("+-.eE0123456789", p[j]) >= 0 {
				j++
			}
			b.WriteString(Yellow(p[i:j]))
			i = j
		case strings.HasPrefix(p[i:], "true"), strings.HasPrefix(p[i:], "false"), strings.HasPrefix(p[i:], "null"):
			j := i
			for j < len(p) && p[j] >= 'a' && p[j] <= 'z' {
				j++
			}
			b.WriteString(Magenta(p[i:j]))
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
