package cmd

import (
	"strings"
	"testing"
)

func TestParseTailEventRedactsCredentials(t *testing.T) {
	body := `{"source":"aws.sts","detail":{"responseElements":{"credentials":{"accessKeyId":"ASIAXXX","sessionToken":"IQo\"JbX","expiration":"2026"}},"requestParameters":{"roleArn":"arn:x"}}}`
	e := parseTailEvent(body)
	for _, s := range []string{string(e.Detail), e.Raw, e.jsonLine()} {
		if strings.Contains(s, "ASIAXXX") || strings.Contains(s, "IQo") {
			t.Errorf("secret leaked: %s", s)
		}
		if !strings.Contains(s, `"sessionToken":"****"`) || !strings.Contains(s, "arn:x") {
			t.Errorf("unexpected redaction result: %s", s)
		}
	}
}
