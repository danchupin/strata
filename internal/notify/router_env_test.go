package notify

import (
	"errors"
	"testing"
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
		"missing-eq":    "topic:arn:aws|secret",
		"missing-pipe":  "topic:arn=https://x",
		"empty-url":     "topic:arn=|s",
		"empty-key":     "=https://x|s",
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
