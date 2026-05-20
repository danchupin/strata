package lifecycle

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
)

// TestLifecycleExpireEmitsComplianceRetentionExpiredAudit covers the US-006
// audit hook: when the lifecycle worker successfully expires an object that
// previously held a COMPLIANCE retention whose RetainUntilDate has elapsed,
// it must append one `objectlock:ComplianceRetentionExpired` row to
// audit_log so reviewers can grep for retention-elapsed deletions.
func TestLifecycleExpireEmitsComplianceRetentionExpiredAudit(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	be := newMultiClusterBackend()

	b, err := store.CreateBucket(ctx, "lc-comp", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	payload := []byte("compliance-payload")
	manifest, err := be.PutChunks(ctx, bytes.NewReader(payload), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          "locked.bin",
		Size:         int64(len(payload)),
		ETag:         manifest.ETag,
		StorageClass: "STANDARD",
		Mtime:        time.Now().Add(-48 * time.Hour),
		Manifest:     manifest,
	}
	if err := store.PutObject(ctx, obj, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	// Retention elapsed 1h ago: lifecycle is free to expire, and the audit
	// hook must fire on the way out.
	if err := store.SetObjectRetention(ctx, b.ID, "locked.bin", "", meta.LockModeCompliance, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SetObjectRetention: %v", err)
	}

	rule := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Expiration><Days>1</Days></Expiration>
	</Rule></LifecycleConfiguration>`)
	if err := store.SetBucketLifecycle(ctx, b.ID, rule); err != nil {
		t.Fatalf("SetBucketLifecycle: %v", err)
	}

	w := &Worker{
		Meta:    store,
		Data:    be,
		Region:  "default",
		AgeUnit: time.Hour,
		Logger:  slog.Default(),
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if _, err := store.GetObject(ctx, b.ID, "locked.bin", ""); err == nil {
		t.Fatalf("object still present after lifecycle expire")
	}
	rows, err := store.ListAudit(ctx, b.ID, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var matched int
	for _, r := range rows {
		if r.Action != "objectlock:ComplianceRetentionExpired" {
			continue
		}
		matched++
		if r.Principal != "system:lifecycle-worker" {
			t.Errorf("principal=%q want system:lifecycle-worker", r.Principal)
		}
		if !strings.HasSuffix(r.Resource, "/locked.bin") {
			t.Errorf("resource=%q want suffix /locked.bin", r.Resource)
		}
	}
	if matched != 1 {
		t.Fatalf("ComplianceRetentionExpired rows=%d want 1; all rows=%+v", matched, rows)
	}
}

// TestLifecycleExpireSkipsAuditForNonComplianceRetention pins the negative
// path: GOVERNANCE retention or absent retention must not emit the compliance
// audit verb when lifecycle expires the object.
func TestLifecycleExpireSkipsAuditForNonComplianceRetention(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	be := newMultiClusterBackend()

	b, err := store.CreateBucket(ctx, "lc-gov", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	payload := []byte("governance-payload")
	manifest, _ := be.PutChunks(ctx, bytes.NewReader(payload), "STANDARD")
	if err := store.PutObject(ctx, &meta.Object{
		BucketID: b.ID, Key: "g.bin", Size: int64(len(payload)), ETag: manifest.ETag,
		StorageClass: "STANDARD", Mtime: time.Now().Add(-48 * time.Hour), Manifest: manifest,
	}, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if err := store.SetObjectRetention(ctx, b.ID, "g.bin", "", meta.LockModeGovernance, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SetObjectRetention: %v", err)
	}
	rule := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Expiration><Days>1</Days></Expiration>
	</Rule></LifecycleConfiguration>`)
	if err := store.SetBucketLifecycle(ctx, b.ID, rule); err != nil {
		t.Fatalf("SetBucketLifecycle: %v", err)
	}

	w := &Worker{Meta: store, Data: be, Region: "default", AgeUnit: time.Hour, Logger: slog.Default()}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows, _ := store.ListAudit(ctx, b.ID, 100)
	for _, r := range rows {
		if r.Action == "objectlock:ComplianceRetentionExpired" {
			t.Fatalf("unexpected ComplianceRetentionExpired row on GOVERNANCE retention: %+v", r)
		}
	}
}
