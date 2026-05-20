package s3api_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPutObjectRetentionEmitsCompliancePut covers the US-006 audit verb that
// fires whenever PutObjectRetention persists a COMPLIANCE-mode retention. The
// row is stamped via SetAuditOverride so AuditMiddleware records
// `objectlock:CompliancePut` instead of the path-derived PutObjectRetention
// fallback.
func TestPutObjectRetentionEmitsCompliancePut(t *testing.T) {
	h := newAuditHarness(t)
	h.status(h.do("PUT", "/lockaudit", "", "x-amz-bucket-object-lock-enabled", "true"), 200)
	h.status(h.do("PUT", "/lockaudit/k", "x"), 200)

	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	body := "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>" + future + "</RetainUntilDate></Retention>"
	h.status(h.do("PUT", "/lockaudit/k?retention", body, "X-Request-Id", "req-comp-put"), 200)

	row := findAuditRow(t, h, "lockaudit", "req-comp-put")
	if row.Action != "objectlock:CompliancePut" {
		t.Fatalf("action=%q want objectlock:CompliancePut", row.Action)
	}
	if row.Resource != "object:lockaudit/k" {
		t.Fatalf("resource=%q want object:lockaudit/k", row.Resource)
	}
}

// TestPutObjectRetentionAttemptedReduceIsRejectedAndAudited covers the second
// US-006 verb: any request that would weaken an existing COMPLIANCE retention
// (mode downgrade, clearing, or earlier RetainUntilDate) is rejected with
// AccessDenied — but the attempt is still audited so reviewers can spot the
// reduce attempt.
func TestPutObjectRetentionAttemptedReduceIsRejectedAndAudited(t *testing.T) {
	h := newAuditHarness(t)
	h.status(h.do("PUT", "/lockaudit", "", "x-amz-bucket-object-lock-enabled", "true"), 200)
	h.status(h.do("PUT", "/lockaudit/k", "x"), 200)

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	body := "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>" + future + "</RetainUntilDate></Retention>"
	h.status(h.do("PUT", "/lockaudit/k?retention", body, "X-Request-Id", "req-set"), 200)

	earlier := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	shorter := "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>" + earlier + "</RetainUntilDate></Retention>"
	resp := h.do("PUT", "/lockaudit/k?retention", shorter, "X-Request-Id", "req-reduce")
	if resp.StatusCode != 403 {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}

	row := findAuditRow(t, h, "lockaudit", "req-reduce")
	if row.Action != "objectlock:ComplianceRetentionAttemptedReduce" {
		t.Fatalf("action=%q want objectlock:ComplianceRetentionAttemptedReduce", row.Action)
	}
	if row.Result != "403" {
		t.Fatalf("result=%q want 403", row.Result)
	}
}

// TestPutObjectRetentionGovernanceDoesNotEmitComplianceVerb pins the negative
// case: GOVERNANCE mode is a separate Object Lock surface; the compliance
// audit verbs should not leak into governance-only flows. The path-derived
// PutObjectRetention fallback row still appears.
func TestPutObjectRetentionGovernanceDoesNotEmitComplianceVerb(t *testing.T) {
	h := newAuditHarness(t)
	h.status(h.do("PUT", "/lockaudit", "", "x-amz-bucket-object-lock-enabled", "true"), 200)
	h.status(h.do("PUT", "/lockaudit/k", "x"), 200)

	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	body := "<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>" + future + "</RetainUntilDate></Retention>"
	h.status(h.do("PUT", "/lockaudit/k?retention", body, "X-Request-Id", "req-gov"), 200)

	row := findAuditRow(t, h, "lockaudit", "req-gov")
	if strings.HasPrefix(row.Action, "objectlock:Compliance") {
		t.Fatalf("unexpected compliance verb on governance write: %q", row.Action)
	}
}

// findAuditRow locates the audit_log row whose RequestID matches; fails the
// test if zero or more than one match is found.
func findAuditRow(t *testing.T, h *auditHarness, bucketName, requestID string) struct {
	Action, Resource, Result string
} {
	t.Helper()
	b, err := h.meta.GetBucket(context.Background(), bucketName)
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, err := h.meta.ListAudit(context.Background(), b.ID, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var matches []struct {
		Action, Resource, Result string
	}
	for _, r := range rows {
		if r.RequestID == requestID {
			matches = append(matches, struct {
				Action, Resource, Result string
			}{r.Action, r.Resource, r.Result})
		}
	}
	if len(matches) != 1 {
		t.Fatalf("requestID %q matched %d rows want 1; all rows = %+v", requestID, len(matches), rows)
	}
	return matches[0]
}
