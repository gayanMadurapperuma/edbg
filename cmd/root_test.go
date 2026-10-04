package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

func TestFriendlyError(t *testing.T) {
	denied := fmt.Errorf("operation error SQS: CreateQueue: %w", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform sqs:CreateQueue"})
	if got := friendlyError(denied); !strings.Contains(got, "hint:") || !strings.Contains(got, "sqs:CreateQueue") {
		t.Errorf("access denied not decorated: %s", got)
	}
	other := errors.New("boom")
	if got := friendlyError(other); got != "boom" {
		t.Errorf("got %q", got)
	}
}
