package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gayanMadurapperuma/edbg/internal/awsclient"
	"github.com/spf13/cobra"
)

var cleanupBus string

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove edbg-tail-* rules and queues left behind by a crashed tail",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, err := loadConfig(ctx)
		if err != nil {
			return err
		}
		return runCleanup(ctx, awsclient.NewEventBridgeClient(cfg), awsclient.NewSQSClient(cfg), cleanupBus)
	},
}

func init() {
	cleanupCmd.Flags().StringVar(&cleanupBus, "bus", "default", "event bus name")
	rootCmd.AddCommand(cleanupCmd)
}

func runCleanup(ctx context.Context, eb awsclient.EventBridgeAPI, q awsclient.SQSAPI, bus string) error {
	rules, err := eb.ListRules(ctx, &eventbridge.ListRulesInput{
		EventBusName: aws.String(bus), NamePrefix: aws.String(tailPrefix),
	})
	if err != nil {
		return fmt.Errorf("list rules: %w", err)
	}
	for _, r := range rules.Rules {
		if _, err := eb.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{
			Rule: r.Name, EventBusName: aws.String(bus), Ids: []string{"edbg"},
		}); err != nil {
			fmt.Printf("warning: remove targets for %s: %v\n", aws.ToString(r.Name), err)
		}
		if _, err := eb.DeleteRule(ctx, &eventbridge.DeleteRuleInput{
			Name: r.Name, EventBusName: aws.String(bus),
		}); err != nil {
			fmt.Printf("warning: delete rule %s: %v\n", aws.ToString(r.Name), err)
			continue
		}
		fmt.Println("deleted rule", aws.ToString(r.Name))
	}

	queues, err := q.ListQueues(ctx, &sqs.ListQueuesInput{QueueNamePrefix: aws.String(tailPrefix)})
	if err != nil {
		return fmt.Errorf("list queues: %w", err)
	}
	for _, u := range queues.QueueUrls {
		if _, err := q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(u)}); err != nil {
			fmt.Printf("warning: delete queue %s: %v\n", u, err)
			continue
		}
		fmt.Println("deleted queue", u)
	}
	return nil
}
