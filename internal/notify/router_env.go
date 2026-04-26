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
//	STRATA_NOTIFY_TARGETS=<type>:<arn>=<url>|<secret>,<type>:<arn>=<url>|<secret>
//
// Example:
//
//	topic:arn:aws:sns:us-east-1:0:t=https://hook.example/webhook|topsecret
//
// The type prefix is matched against meta.NotificationEvent.TargetType (one of
// topic/queue/lambda/eventbridge). Only "webhook" delivery is supported in
// US-009; an SQS sink is added in US-010.
const EnvNotifyTargets = "STRATA_NOTIFY_TARGETS"

// RouterFromEnv parses EnvNotifyTargets into a StaticRouter populated with
// WebhookSink targets. Returns ErrNoTargets if the env var is empty.
func RouterFromEnv() (StaticRouter, error) {
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
		url, secret, ok := strings.Cut(spec, "|")
		if !ok {
			return nil, fmt.Errorf("notify: target %q spec missing '|secret'", entry)
		}
		url = strings.TrimSpace(url)
		secret = strings.TrimSpace(secret)
		if url == "" {
			return nil, fmt.Errorf("notify: target %q has empty URL", entry)
		}
		router[key] = &WebhookSink{
			SinkName: key,
			URL:      url,
			Secret:   []byte(secret),
		}
	}
	if len(router) == 0 {
		return nil, ErrNoTargets
	}
	return router, nil
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
