package awsclient

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/xray"
)

// Options overrides the default credential/region resolution when set.
type Options struct {
	Profile string
	Region  string
}

func LoadConfig(ctx context.Context, o Options) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if o.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(o.Profile))
	}
	if o.Region != "" {
		opts = append(opts, config.WithRegion(o.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("unable to load AWS config: %w", err)
	}
	return cfg, nil
}

func NewSQSClient(cfg aws.Config) *sqs.Client {
	return sqs.NewFromConfig(cfg)
}

func NewEventBridgeClient(cfg aws.Config) *eventbridge.Client {
	return eventbridge.NewFromConfig(cfg)
}

func NewSFNClient(cfg aws.Config) *sfn.Client {
	return sfn.NewFromConfig(cfg)
}

func NewXRayClient(cfg aws.Config) *xray.Client {
	return xray.NewFromConfig(cfg)
}

func NewSTSClient(cfg aws.Config) *sts.Client {
	return sts.NewFromConfig(cfg)
}

func NewLogsClient(cfg aws.Config) *cloudwatchlogs.Client {
	return cloudwatchlogs.NewFromConfig(cfg)
}

func NewMetricsClient(cfg aws.Config) *cloudwatch.Client {
	return cloudwatch.NewFromConfig(cfg)
}

func NewLambdaClient(cfg aws.Config) *lambda.Client {
	return lambda.NewFromConfig(cfg)
}
