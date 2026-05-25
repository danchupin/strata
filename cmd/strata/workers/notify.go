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
	cfg := workerCfg(deps)
	nCfg := cfg.Workers.Notify
	router, err := notify.RouterFromSpec(nCfg.Targets, notify.WithSQSClientFactory(sqsClientFactory))
	if err != nil {
		return nil, err
	}
	return notify.New(notify.Config{
		Meta:        deps.Meta,
		Router:      router,
		Logger:      deps.Logger,
		Metrics:     metrics.NotifyObserver{},
		Interval:    orDuration(nCfg.Interval, 5*time.Second),
		MaxRetries:  orInt(nCfg.MaxRetries, 6),
		BackoffBase: orDuration(nCfg.BackoffBase, 1*time.Second),
		PollLimit:   orInt(nCfg.PollLimit, 100),
		Tracer:      deps.Tracer.Tracer("strata.worker.notify"),
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
