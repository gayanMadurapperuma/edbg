package cmd

import (
	"io"
	"os"
	"testing"

	"github.com/gayanMadurapperuma/edbg/internal/output"
)

func TestMain(m *testing.M) {
	output.ColorEnabled = false
	os.Exit(m.Run())
}

// captureStdout runs fn and returns what it printed to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	return <-done
}
