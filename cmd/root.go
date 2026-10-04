package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/gayanMadurapperuma/edbg/internal/output"
	"github.com/spf13/cobra"
)

// version is overridden at build time: go build -ldflags "-X github.com/gayanMadurapperuma/edbg/cmd.version=1.0.0"
var version = "dev"

var (
	flagProfile string
	flagRegion  string
	flagOutput  string
	flagNoColor bool
)

var rootCmd = &cobra.Command{
	Use:           "edbg",
	Short:         "Event-Driven Debugging CLI for AWS Serverless",
	Long:          `A CLI tool to trace, tail, and debug AWS EventBridge, SQS, and Step Functions in real-time.`,
	Version:       version,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if flagOutput != "text" && flagOutput != "json" {
			return fmt.Errorf("--output must be \"text\" or \"json\", got %q", flagOutput)
		}
		output.ColorEnabled = !flagNoColor && os.Getenv("NO_COLOR") == "" && flagOutput == "text" && isTerminal(os.Stdout)
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagProfile, "profile", "", "AWS shared config profile")
	rootCmd.PersistentFlags().StringVar(&flagRegion, "region", "", "AWS region (overrides profile/env)")
	rootCmd.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format: text|json")
	rootCmd.PersistentFlags().BoolVar(&flagNoColor, "no-color", false, "disable colored output")
}

func Execute() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if ctx.Err() != nil { // Ctrl+C / SIGTERM: say nothing alarming
			return 130
		}
		fmt.Fprintln(os.Stderr, output.Red("Error:")+" "+friendlyError(err))
		return 1
	}
	return 0
}

func loadConfig(ctx context.Context) (aws.Config, error) {
	return awsclient.LoadConfig(ctx, awsclient.Options{Profile: flagProfile, Region: flagRegion})
}

func outputJSON() bool { return flagOutput == "json" }

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// friendlyError adds a hint for IAM permission errors; AWS usually names the missing action in the message.
func friendlyError(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if strings.HasPrefix(code, "AccessDenied") || code == "UnauthorizedOperation" {
			return fmt.Sprintf("%v\nhint: your credentials lack a required IAM permission (see the action named above)", err)
		}
	}
	return err.Error()
}
