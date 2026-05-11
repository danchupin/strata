package s3

import (
	"context"

	"github.com/aws/smithy-go/middleware"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	strataotel "github.com/danchupin/strata/internal/otel"
)

const (
	// AttrStrataS3ClusterKey labels every otelaws-emitted span with the
	// originating Strata cluster id so operators can filter Jaeger by
	// the S3 cluster a request landed on.
	AttrStrataS3ClusterKey attribute.Key = "strata.s3_cluster"
)

// stampStrataAttrs returns an otelaws.AttributeBuilder that stamps
// strata.component=gateway + strata.s3_cluster=<id> on every span emitted
// by the SDK client bound to clusterID. otelaws.AppendMiddlewares
// concatenates the additional builders with its DefaultAttributeBuilder.
func stampStrataAttrs(clusterID string) otelaws.AttributeBuilder {
	cluster := AttrStrataS3ClusterKey.String(clusterID)
	return func(_ context.Context, _ middleware.InitializeInput, _ middleware.InitializeOutput) []attribute.KeyValue {
		return []attribute.KeyValue{strataotel.AttrComponentGateway, cluster}
	}
}

// installOTelMiddleware appends otelaws middleware to apiOpts when tp is
// non-nil. otelaws auto-emits one client-kind span per SDK call named
// `<service>.<operation>` (e.g. `S3.PutObject`) with `rpc.system=aws-api`,
// `rpc.method=S3/<op>`, `aws.region`, `http.status_code`, and request id
// attributes; failing calls flip status to Error so the tail-sampler
// always exports them. Skipping the install when tp is nil preserves the
// zero-config test fixture from `s3test.NewFixture`.
func installOTelMiddleware(apiOpts *[]func(*middleware.Stack) error, tp trace.TracerProvider, clusterID string) {
	if tp == nil {
		return
	}
	otelaws.AppendMiddlewares(apiOpts,
		otelaws.WithTracerProvider(tp),
		otelaws.WithAttributeBuilder(stampStrataAttrs(clusterID)),
	)
}
