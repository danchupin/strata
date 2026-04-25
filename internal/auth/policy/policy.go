// Package policy implements a minimal IAM-style JSON policy evaluator.
//
// It is deliberately small: the only condition operator supported is
// StringEquals; principals support "*" and AWS lists; actions and resources
// support "*" and "?" glob wildcards.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Decision is the result of evaluating a Document.
type Decision int

const (
	// Deny is the default outcome when no statement matches or any matching
	// statement uses Effect=Deny.
	Deny Decision = iota
	// Allow is returned when at least one statement matches with Effect=Allow
	// and no matching statement has Effect=Deny.
	Allow
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "Allow"
	default:
		return "Deny"
	}
}

const (
	EffectAllow = "Allow"
	EffectDeny  = "Deny"
)

// Document is a parsed IAM-style policy.
type Document struct {
	Version    string      `json:"Version,omitempty"`
	ID         string      `json:"Id,omitempty"`
	Statements []Statement `json:"Statement"`
}

// Statement is a single rule within a Document.
type Statement struct {
	Sid       string                            `json:"Sid,omitempty"`
	Effect    string                            `json:"Effect"`
	Principal Principal                         `json:"Principal,omitempty"`
	Action    StringOrSlice                     `json:"Action,omitempty"`
	Resource  StringOrSlice                     `json:"Resource,omitempty"`
	Condition map[string]map[string]StringSlice `json:"Condition,omitempty"`
}

// Principal models an IAM Principal field, which may be the string "*" or
// an object such as {"AWS": "arn:aws:iam::1:user/u"}.
type Principal struct {
	All bool
	AWS []string
}

// StringOrSlice models a JSON field that may be a single string or an array.
type StringOrSlice []string

// StringSlice mirrors StringOrSlice for use inside Condition maps where the
// value side may also be a string or array.
type StringSlice []string

// EvalContext carries condition keys (e.g. "aws:SourceIp") supplied by the
// caller to evaluate Condition blocks against the request.
type EvalContext map[string]string

// Parse decodes a JSON policy document.
func Parse(b []byte) (*Document, error) {
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	for i, st := range d.Statements {
		switch st.Effect {
		case EffectAllow, EffectDeny:
		default:
			return nil, fmt.Errorf("policy: statement[%d]: invalid Effect %q", i, st.Effect)
		}
	}
	return &d, nil
}

// Evaluate returns Allow only if some statement matches with Effect=Allow and
// no matching statement has Effect=Deny. A nil document or no statements
// always returns Deny.
func Evaluate(doc *Document, principal, action, resource string, ctx EvalContext) (Decision, error) {
	if doc == nil {
		return Deny, nil
	}
	allow := false
	for i, st := range doc.Statements {
		if !st.matches(principal, action, resource, ctx) {
			continue
		}
		switch st.Effect {
		case EffectDeny:
			return Deny, nil
		case EffectAllow:
			allow = true
		default:
			return Deny, fmt.Errorf("policy: statement[%d]: invalid Effect %q", i, st.Effect)
		}
	}
	if allow {
		return Allow, nil
	}
	return Deny, nil
}

func (s *Statement) matches(principal, action, resource string, ctx EvalContext) bool {
	if !s.Principal.matches(principal) {
		return false
	}
	if !matchAny(s.Action, action) {
		return false
	}
	if !matchAny(s.Resource, resource) {
		return false
	}
	return s.matchConditions(ctx)
}

func (s *Statement) matchConditions(ctx EvalContext) bool {
	for op, kv := range s.Condition {
		if op != "StringEquals" {
			return false
		}
		for key, want := range kv {
			got, ok := ctx[key]
			if !ok {
				return false
			}
			matched := false
			for _, w := range want {
				if got == w {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

func (p Principal) matches(principal string) bool {
	if p.All {
		return true
	}
	if principal == "*" && len(p.AWS) == 0 {
		return false
	}
	for _, want := range p.AWS {
		if want == "*" || want == principal {
			return true
		}
	}
	return false
}

func matchAny(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, p := range patterns {
		if glob(p, value) {
			return true
		}
	}
	return false
}

// glob matches with two wildcards: '*' matches any run of characters, '?'
// matches a single character. Used for both s3:* style actions and ARN
// resources.
func glob(pattern, s string) bool {
	pi, si := 0, 0
	starP, starS := -1, -1
	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			starP = pi
			starS = si
			pi++
			continue
		}
		if starP != -1 {
			pi = starP + 1
			starS++
			si = starS
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// UnmarshalJSON accepts both `"value"` and `["v1","v2"]`.
func (s *StringOrSlice) UnmarshalJSON(b []byte) error {
	return unmarshalStrings((*[]string)(s), b)
}

// MarshalJSON emits a single string when only one element is present, matching
// the canonical AWS JSON form.
func (s StringOrSlice) MarshalJSON() ([]byte, error) {
	return marshalStrings([]string(s))
}

// UnmarshalJSON accepts both `"value"` and `["v1","v2"]`.
func (s *StringSlice) UnmarshalJSON(b []byte) error {
	return unmarshalStrings((*[]string)(s), b)
}

// MarshalJSON mirrors StringOrSlice.
func (s StringSlice) MarshalJSON() ([]byte, error) {
	return marshalStrings([]string(s))
}

func unmarshalStrings(dst *[]string, b []byte) error {
	if len(b) == 0 {
		return errors.New("empty value")
	}
	if b[0] == '[' {
		var v []string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*dst = v
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*dst = []string{v}
	return nil
}

func marshalStrings(v []string) ([]byte, error) {
	if len(v) == 1 {
		return json.Marshal(v[0])
	}
	return json.Marshal([]string(v))
}

// UnmarshalJSON handles the two AWS shapes:
//
//	"Principal": "*"
//	"Principal": { "AWS": "arn:..." | ["arn:...", ...] }
func (p *Principal) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return errors.New("empty principal")
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "*" {
			p.All = true
			return nil
		}
		return fmt.Errorf("policy: principal %q must be \"*\" or an object", s)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["AWS"]; ok {
		var s StringOrSlice
		if err := s.UnmarshalJSON(v); err != nil {
			return err
		}
		for _, e := range s {
			if e == "*" {
				p.All = true
			}
		}
		p.AWS = []string(s)
	}
	return nil
}

// MarshalJSON renders the Principal back to canonical JSON.
func (p Principal) MarshalJSON() ([]byte, error) {
	if p.All && len(p.AWS) == 0 {
		return json.Marshal("*")
	}
	out := map[string]any{}
	if len(p.AWS) > 0 {
		if len(p.AWS) == 1 {
			out["AWS"] = p.AWS[0]
		} else {
			out["AWS"] = p.AWS
		}
	}
	return json.Marshal(out)
}
