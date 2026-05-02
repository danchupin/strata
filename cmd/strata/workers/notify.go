package workers

import (
	"context"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/notify"
)

func init() {
	Register(Worker{
		Name:  "notify",
		Build: buildNotify,
	})
}

func buildNotify(deps Dependencies) (Runner, error) {
	router, err := notify.RouterFromEnv(notify.WithSQSClientFactory(sqsClientFactory))
	if err != nil {
		return nil, err
	}
	return notify.New(notify.Config{
		Meta:        deps.Meta,
		Router:      router,
		Logger:      deps.Logger,
		Metrics:     metrics.NotifyObserver{},
		Interval:    durationFromEnv("STRATA_NOTIFY_INTERVAL", 5*time.Second),
		MaxRetries:  intFromEnv("STRATA_NOTIFY_MAX_RETRIES", 6),
		BackoffBase: durationFromEnv("STRATA_NOTIFY_BACKOFF_BASE", 1*time.Second),
		PollLimit:   intFromEnv("STRATA_NOTIFY_POLL_LIMIT", 100),
	})
}

// sqsClientFactory builds an AWS SDK SQS client per RouterFromEnv target.
// Resolves credentials via the standard AWS chain (env, shared config, IRSA,
// EC2/ECS instance roles). An empty region lets the chain pick from
// AWS_REGION / EC2 metadata.
func sqsClientFactory(region string) (notify.SQSAPI, error) {
	ctx := context.Background()
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sqs.NewFromConfig(cfg), nil
}
