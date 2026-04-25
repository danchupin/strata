package policy_test

import (
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth/policy"
)

func parse(t *testing.T, doc string) *policy.Document {
	t.Helper()
	d, err := policy.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

func TestEvaluate_ExplicitAllow(t *testing.T) {
	d := parse(t, `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": "arn:aws:iam::1:user/alice"},
			"Action": "s3:GetObject",
			"Resource": "arn:aws:s3:::bkt/*"
		}]
	}`)
	got, err := policy.Evaluate(d, "arn:aws:iam::1:user/alice", "s3:GetObject", "arn:aws:s3:::bkt/k", nil)
	if err != nil || got != policy.Allow {
		t.Fatalf("want Allow, got %v err=%v", got, err)
	}
}

func TestEvaluate_DenyOverridesAllow(t *testing.T) {
	d := parse(t, `{
		"Statement": [
			{"Effect": "Allow", "Principal": "*", "Action": "s3:*", "Resource": "*"},
			{"Effect": "Deny",  "Principal": "*", "Action": "s3:DeleteObject", "Resource": "arn:aws:s3:::bkt/*"}
		]
	}`)
	if got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::bkt/k", nil); got != policy.Allow {
		t.Fatalf("Get: want Allow, got %v", got)
	}
	if got, _ := policy.Evaluate(d, "*", "s3:DeleteObject", "arn:aws:s3:::bkt/k", nil); got != policy.Deny {
		t.Fatalf("Delete: want Deny (explicit), got %v", got)
	}
}

func TestEvaluate_WildcardAction(t *testing.T) {
	d := parse(t, `{
		"Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:Get*", "Resource": "*"}]
	}`)
	if got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/k", nil); got != policy.Allow {
		t.Fatalf("GetObject: want Allow, got %v", got)
	}
	if got, _ := policy.Evaluate(d, "*", "s3:PutObject", "arn:aws:s3:::b/k", nil); got != policy.Deny {
		t.Fatalf("PutObject: want Deny (no match), got %v", got)
	}
}

func TestEvaluate_WildcardResource(t *testing.T) {
	d := parse(t, `{
		"Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::b/prefix/*"}]
	}`)
	if got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/prefix/key", nil); got != policy.Allow {
		t.Fatalf("matching prefix: want Allow, got %v", got)
	}
	if got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/other/key", nil); got != policy.Deny {
		t.Fatalf("non-matching prefix: want Deny, got %v", got)
	}
}

func TestEvaluate_StringEqualsCondition(t *testing.T) {
	d := parse(t, `{
		"Statement": [{
			"Effect": "Allow",
			"Principal": "*",
			"Action": "s3:GetObject",
			"Resource": "arn:aws:s3:::b/*",
			"Condition": {"StringEquals": {"aws:SourceVpce": ["vpce-1", "vpce-2"]}}
		}]
	}`)
	if got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/k", policy.EvalContext{"aws:SourceVpce": "vpce-1"}); got != policy.Allow {
		t.Fatalf("matching condition: want Allow, got %v", got)
	}
	if got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/k", policy.EvalContext{"aws:SourceVpce": "vpce-9"}); got != policy.Deny {
		t.Fatalf("non-matching condition: want Deny, got %v", got)
	}
	if got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/k", nil); got != policy.Deny {
		t.Fatalf("missing condition key: want Deny, got %v", got)
	}
}

func TestEvaluate_AnonymousStarPrincipal(t *testing.T) {
	d := parse(t, `{
		"Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::b/*"}]
	}`)
	got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/k", nil)
	if got != policy.Allow {
		t.Fatalf("anonymous Principal=*: want Allow, got %v", got)
	}
}

func TestEvaluate_NoMatchDefaultDeny(t *testing.T) {
	d := parse(t, `{
		"Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::b/*"}]
	}`)
	got, _ := policy.Evaluate(d, "*", "s3:PutObject", "arn:aws:s3:::b/k", nil)
	if got != policy.Deny {
		t.Fatalf("default: want Deny, got %v", got)
	}
}

func TestEvaluate_WrongPrincipal(t *testing.T) {
	d := parse(t, `{
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": "arn:aws:iam::1:user/alice"},
			"Action": "s3:GetObject",
			"Resource": "arn:aws:s3:::b/*"
		}]
	}`)
	if got, _ := policy.Evaluate(d, "arn:aws:iam::1:user/bob", "s3:GetObject", "arn:aws:s3:::b/k", nil); got != policy.Deny {
		t.Fatalf("wrong principal: want Deny, got %v", got)
	}
	if got, _ := policy.Evaluate(d, "arn:aws:iam::1:user/alice", "s3:GetObject", "arn:aws:s3:::b/k", nil); got != policy.Allow {
		t.Fatalf("right principal: want Allow, got %v", got)
	}
}

func TestParse_InvalidEffect(t *testing.T) {
	_, err := policy.Parse([]byte(`{"Statement":[{"Effect":"Maybe","Principal":"*","Action":"s3:*","Resource":"*"}]}`))
	if err == nil || !strings.Contains(err.Error(), "invalid Effect") {
		t.Fatalf("want invalid Effect error, got %v", err)
	}
}

func TestParse_StringOrSlice(t *testing.T) {
	d := parse(t, `{
		"Statement": [{
			"Effect": "Allow", "Principal": "*",
			"Action": ["s3:GetObject", "s3:HeadObject"],
			"Resource": ["arn:aws:s3:::a", "arn:aws:s3:::b/*"]
		}]
	}`)
	if got, _ := policy.Evaluate(d, "*", "s3:HeadObject", "arn:aws:s3:::b/k", nil); got != policy.Allow {
		t.Fatalf("multi-action/multi-resource: want Allow, got %v", got)
	}
}

func TestEvaluate_NilDoc(t *testing.T) {
	if got, _ := policy.Evaluate(nil, "*", "s3:GetObject", "arn:aws:s3:::b/k", nil); got != policy.Deny {
		t.Fatalf("nil doc: want Deny, got %v", got)
	}
}

func TestEvaluate_UnsupportedConditionOp(t *testing.T) {
	d := parse(t, `{
		"Statement": [{
			"Effect": "Allow", "Principal": "*",
			"Action": "s3:*", "Resource": "*",
			"Condition": {"Bool": {"aws:SecureTransport": "true"}}
		}]
	}`)
	got, _ := policy.Evaluate(d, "*", "s3:GetObject", "arn:aws:s3:::b/k", policy.EvalContext{"aws:SecureTransport": "true"})
	if got != policy.Deny {
		t.Fatalf("unsupported condition op should not match: want Deny, got %v", got)
	}
}
