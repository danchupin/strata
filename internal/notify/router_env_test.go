package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestRouterFromEnvEmpty(t *testing.T) {
	t.Setenv(EnvNotifyTargets, "")
	if _, err := RouterFromEnv(); !errors.Is(err, ErrNoTargets) {
		t.Fatalf("got %v want ErrNoTargets", err)
	}
}

func TestRouterFromEnvSingleTarget(t *testing.T) {
	t.Setenv(EnvNotifyTargets, "topic:arn:aws:sns:us-east-1:0:t=https://x.example/hook|topsecret")
	r, err := RouterFromEnv()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(r) != 1 {
		t.Fatalf("len=%d want 1", len(r))
	}
	wh, ok := r["topic:arn:aws:sns:us-east-1:0:t"].(*WebhookSink)
	if !ok {
		t.Fatalf("not webhook sink: %T", r["topic:arn:aws:sns:us-east-1:0:t"])
	}
	if wh.URL != "https://x.example/hook" {
		t.Fatalf("URL: %q", wh.URL)
	}
	if string(wh.Secret) != "topsecret" {
		t.Fatalf("secret: %q", wh.Secret)
	}
}

func TestRouterFromEnvMultiTarget(t *testing.T) {
	t.Setenv(EnvNotifyTargets,
		"topic:arn:aws:sns:0=https://a.example/h|sa,"+
			"queue:arn:aws:sqs:0=https://b.example/h|sb")
	r, err := RouterFromEnv()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(r) != 2 {
		t.Fatalf("len=%d want 2", len(r))
	}
}

func TestRouterFromEnvMalformed(t *testing.T) {
	cases := map[string]string{
		"missing-eq":   "topic:arn:aws|secret",
		"missing-pipe": "topic:arn=https://x",
		"empty-url":    "topic:arn=|s",
		"empty-key":    "=https://x|s",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvNotifyTargets, raw)
			if _, err := RouterFromEnv(); err == nil {
				t.Fatalf("expected error for %q", raw)
			}
		})
	}
}

func TestRouterFromEnvSQSTargetWithFactory(t *testing.T) {
	t.Setenv(EnvNotifyTargets,
		"queue:arn:aws:sqs:us-east-1:0:q=sqs:https://sqs.us-east-1.amazonaws.com/0/q|us-east-1")
	var receivedRegion string
	factory := func(region string) (SQSAPI, error) {
		receivedRegion = region
		return &fakeSQSClient{}, nil
	}
	r, err := RouterFromEnv(WithSQSClientFactory(factory))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sink, ok := r["queue:arn:aws:sqs:us-east-1:0:q"].(*SQSSink)
	if !ok {
		t.Fatalf("expected SQSSink, got %T", r["queue:arn:aws:sqs:us-east-1:0:q"])
	}
	if sink.QueueURL != "https://sqs.us-east-1.amazonaws.com/0/q" {
		t.Fatalf("queue url: %q", sink.QueueURL)
	}
	if sink.Region != "us-east-1" {
		t.Fatalf("region: %q", sink.Region)
	}
	if receivedRegion != "us-east-1" {
		t.Fatalf("factory region: %q", receivedRegion)
	}
}

func TestRouterFromEnvSQSWithoutFactoryRejected(t *testing.T) {
	t.Setenv(EnvNotifyTargets,
		"queue:arn=sqs:https://sqs.us-east-1.amazonaws.com/0/q|us-east-1")
	if _, err := RouterFromEnv(); err == nil {
		t.Fatal("SQS spec without factory should error")
	}
}

func TestRouterFromEnvSQSMalformed(t *testing.T) {
	cases := map[string]string{
		"missing-region": "queue:arn=sqs:https://sqs.us-east-1.amazonaws.com/0/q",
		"empty-url":      "queue:arn=sqs:|us-east-1",
	}
	factory := func(string) (SQSAPI, error) { return &fakeSQSClient{}, nil }
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvNotifyTargets, raw)
			if _, err := RouterFromEnv(WithSQSClientFactory(factory)); err == nil {
				t.Fatalf("expected error for %q", raw)
			}
		})
	}
}

func TestRouterFromEnvMixedTargets(t *testing.T) {
	t.Setenv(EnvNotifyTargets,
		"topic:arn:aws:sns:0=https://a.example/h|sa,"+
			"queue:arn:aws:sqs:0=sqs:https://sqs.us-east-1.amazonaws.com/0/q|us-east-1")
	factory := func(string) (SQSAPI, error) { return &fakeSQSClient{}, nil }
	r, err := RouterFromEnv(WithSQSClientFactory(factory))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := r["topic:arn:aws:sns:0"].(*WebhookSink); !ok {
		t.Fatalf("topic should be webhook: %T", r["topic:arn:aws:sns:0"])
	}
	if _, ok := r["queue:arn:aws:sqs:0"].(*SQSSink); !ok {
		t.Fatalf("queue should be sqs: %T", r["queue:arn:aws:sqs:0"])
	}
}

// Compile-time check that *sqs.Client satisfies SQSAPI. Without this, a
// future SDK upgrade that changes the SendMessage signature would silently
// drift the SQSSink contract.
var _ SQSAPI = (*sqs.Client)(nil)

// Compile-time check that fakeSQSClient implements SQSAPI.
var _ SQSAPI = (*fakeSQSClient)(nil)

// Stub to silence unused-import warning if downstream test files don't
// reference context.
var _ = context.Background
