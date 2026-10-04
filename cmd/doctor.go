package cmd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/xray"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/gayanMadurapperuma/edbg/internal/output"
	"github.com/spf13/cobra"
)

var doctorTimeout time.Duration

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check credentials and connectivity to every AWS service edbg uses",
	Long: `Makes one cheap read-only call to each service. Useful from inside a VPC or behind a
proxy: a timeout usually means the service has no route or VPC endpoint
(interface endpoints needed: sts, sqs, events, states, xray, logs, monitoring, lambda).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Region: %s\n\n", cfg.Region)
		if !printChecks(runChecks(ctx, doctorChecks(cfg), doctorTimeout)) {
			return fmt.Errorf("one or more checks failed")
		}
		return nil
	},
}

func init() {
	doctorCmd.Flags().DurationVar(&doctorTimeout, "timeout", 5*time.Second, "per-service timeout")
	rootCmd.AddCommand(doctorCmd)
}

type check struct {
	Name string
	Run  func(ctx context.Context) (string, error)
}

type checkResult struct {
	Name, Detail string
	Err          error
}

func doctorChecks(cfg aws.Config) []check {
	return []check{
		{"sts (credentials)", func(ctx context.Context) (string, error) {
			out, err := awsclient.NewSTSClient(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return "", err
			}
			return aws.ToString(out.Arn), nil
		}},
		{"sqs (tail, dlq-inspect)", func(ctx context.Context) (string, error) {
			_, err := awsclient.NewSQSClient(cfg).ListQueues(ctx, &sqs.ListQueuesInput{MaxResults: aws.Int32(1)})
			return "", err
		}},
		{"events (tail)", func(ctx context.Context) (string, error) {
			_, err := awsclient.NewEventBridgeClient(cfg).ListEventBuses(ctx, &eventbridge.ListEventBusesInput{Limit: aws.Int32(1)})
			return "", err
		}},
		{"states (trace-sfn)", func(ctx context.Context) (string, error) {
			_, err := awsclient.NewSFNClient(cfg).ListStateMachines(ctx, &sfn.ListStateMachinesInput{MaxResults: 1})
			return "", err
		}},
		{"logs (watch --lambda)", func(ctx context.Context) (string, error) {
			_, err := awsclient.NewLogsClient(cfg).DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{Limit: aws.Int32(1)})
			return "", err
		}},
		{"monitoring (dash metrics)", func(ctx context.Context) (string, error) {
			// One account-wide series: exercises cloudwatch:GetMetricData at a cost of a single metric.
			now := time.Now()
			_, err := awsclient.NewMetricsClient(cfg).GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
				StartTime: aws.Time(now.Add(-5 * time.Minute)), EndTime: aws.Time(now),
				MetricDataQueries: []cwtypes.MetricDataQuery{{Id: aws.String("m0"), MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{Namespace: aws.String("AWS/Lambda"), MetricName: aws.String("Invocations")},
					Period: aws.Int32(300), Stat: aws.String("Sum"),
				}}},
			})
			return "", err
		}},
		{"lambda (dash)", func(ctx context.Context) (string, error) {
			_, err := awsclient.NewLambdaClient(cfg).ListFunctions(ctx, &lambda.ListFunctionsInput{MaxItems: aws.Int32(1)})
			return "", err
		}},
		{"xray (trace, traces)", func(ctx context.Context) (string, error) {
			now := time.Now()
			_, err := awsclient.NewXRayClient(cfg).GetTraceSummaries(ctx, &xray.GetTraceSummariesInput{
				StartTime: aws.Time(now.Add(-time.Minute)), EndTime: aws.Time(now),
			})
			return "", err
		}},
	}
}

// runChecks runs all checks concurrently, each with its own timeout, and returns results in input order.
func runChecks(ctx context.Context, checks []check, timeout time.Duration) []checkResult {
	results := make([]checkResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			detail, err := c.Run(cctx)
			results[i] = checkResult{Name: c.Name, Detail: detail, Err: err}
		}()
	}
	wg.Wait()
	return results
}

func printChecks(results []checkResult) (allOK bool) {
	allOK = true
	for _, r := range results {
		if r.Err != nil {
			allOK = false
			fmt.Printf("%s  %-26s %s\n", output.Red("FAIL"), r.Name, friendlyError(r.Err))
			continue
		}
		fmt.Printf("%s    %-26s %s\n", output.Green("OK"), r.Name, r.Detail)
	}
	return allOK
}
