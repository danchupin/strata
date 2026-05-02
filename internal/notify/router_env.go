package notify

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvNotifyTargets is the env var consulted by RouterFromEnv.
// Format:
//
//	STRATA_NOTIFY_TARGETS=<type>:<arn>=<spec>,<type>:<arn>=<spec>
//
// where <spec> is either:
//   - webhook (default, no prefix): "<url>|<secret>" — HTTPS POST with HMAC-SHA256
//   - sqs: "sqs:<queueURL>|<region>" — AWS SDK SendMessage (US-010)
//
// SQS targets require a SQSClientFactory option (see WithSQSClientFactory).
// Without it, a "sqs:" spec returns an error so misconfiguration surfaces at
// startup rather than per-event.
const EnvNotifyTargets = "STRATA_NOTIFY_TARGETS"

// EnvOption configures RouterFromEnv. Use WithSQSClientFactory to enable SQS
// targets in addition to webhooks.
type EnvOption func(*envCfg)

type envCfg struct {
	sqsFactory func(region string) (SQSAPI, error)
}

// WithSQSClientFactory wires the AWS SDK SQS client constructor used by
// RouterFromEnv when it encounters a "sqs:" spec. The factory receives the
// per-target region (or "" if absent) and returns a sqs.Client (or compatible
// mock). cmd/strata-notify wires this from config.LoadDefaultConfig.
func WithSQSClientFactory(factory func(region string) (SQSAPI, error)) EnvOption {
	return func(c *envCfg) { c.sqsFactory = factory }
}

// RouterFromEnv parses EnvNotifyTargets into a StaticRouter populated with
// per-target sinks. Returns ErrNoTargets if the env var is empty.
func RouterFromEnv(opts ...EnvOption) (StaticRouter, error) {
	cfg := envCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}
	raw := strings.TrimSpace(os.Getenv(EnvNotifyTargets))
	if raw == "" {
		return nil, ErrNoTargets
	}
	router := StaticRouter{}
	for _, entry := range splitTargets(raw) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, spec, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("notify: target %q missing '=' separator", entry)
		}
		key = strings.TrimSpace(key)
		spec = strings.TrimSpace(spec)
		if key == "" || spec == "" {
			return nil, fmt.Errorf("notify: target %q has empty key or spec", entry)
		}
		sink, err := buildSink(key, spec, &cfg)
		if err != nil {
			return nil, err
		}
		router[key] = sink
	}
	if len(router) == 0 {
		return nil, ErrNoTargets
	}
	return router, nil
}

func buildSink(key, spec string, cfg *envCfg) (Sink, error) {
	if rest, ok := strings.CutPrefix(spec, "sqs:"); ok {
		queueURL, region, ok := strings.Cut(rest, "|")
		if !ok {
			return nil, fmt.Errorf("notify: target %q sqs spec missing '|region'", key)
		}
		queueURL = strings.TrimSpace(queueURL)
		region = strings.TrimSpace(region)
		if queueURL == "" {
			return nil, fmt.Errorf("notify: target %q has empty SQS queue URL", key)
		}
		if cfg.sqsFactory == nil {
			return nil, fmt.Errorf("notify: target %q is SQS but no SQS client factory configured", key)
		}
		client, err := cfg.sqsFactory(region)
		if err != nil {
			return nil, fmt.Errorf("notify: target %q SQS client init: %w", key, err)
		}
		return &SQSSink{SinkName: key, QueueURL: queueURL, Region: region, Client: client}, nil
	}
	url, secret, ok := strings.Cut(spec, "|")
	if !ok {
		return nil, fmt.Errorf("notify: target %q spec missing '|secret'", key)
	}
	url = strings.TrimSpace(url)
	secret = strings.TrimSpace(secret)
	if url == "" {
		return nil, fmt.Errorf("notify: target %q has empty URL", key)
	}
	return &WebhookSink{SinkName: key, URL: url, Secret: []byte(secret)}, nil
}

// ErrNoTargets is returned by RouterFromEnv when STRATA_NOTIFY_TARGETS is
// unset or empty. The cmd binary fatals on this — running a notify worker
// with no sinks is a misconfiguration.
var ErrNoTargets = errors.New("notify: STRATA_NOTIFY_TARGETS not set")

// splitTargets splits the env value on top-level commas. ARN values do not
// embed commas in any AWS spec we support, so a plain split is safe.
func splitTargets(raw string) []string {
	return strings.Split(raw, ",")
}
