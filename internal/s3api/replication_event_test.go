package s3api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

type replicationHarness struct {
	*testHarness
	store *metamem.Store
}

func newReplicationHarness(t *testing.T) *replicationHarness {
	t.Helper()
	store := metamem.New()
	api := s3api.New(datamem.New(), store)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &replicationHarness{
		testHarness: &testHarness{t: t, ts: ts},
		store:       store,
	}
}

func (h *replicationHarness) listReplications(bucket string) []meta.ReplicationEvent {
	h.t.Helper()
	b, err := h.store.GetBucket(context.Background(), bucket)
	if err != nil {
		h.t.Fatalf("get bucket %q: %v", bucket, err)
	}
	rows, err := h.store.ListPendingReplications(context.Background(), b.ID, 100)
	if err != nil {
		h.t.Fatalf("list replications: %v", err)
	}
	return rows
}

func TestReplicationPutEnqueuesRowAndPending(t *testing.T) {
	h := newReplicationHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h.testHarness, "bkt")
	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationPrefixXML), 200)

	resp := h.doString("PUT", "/bkt/logs/2026/04.txt", "hello")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "PENDING" {
		t.Fatalf("status: got %q want PENDING", got)
	}

	rows := h.listReplications("bkt")
	if len(rows) != 1 {
		t.Fatalf("got %d rows want 1", len(rows))
	}
	r := rows[0]
	if r.Key != "logs/2026/04.txt" || r.RuleID != "logs" || r.DestinationBucket != "arn:aws:s3:::dest" {
		t.Fatalf("row mismatch: %+v", r)
	}
	if r.EventName != "s3:ObjectCreated:Put" {
		t.Fatalf("event name: got %q", r.EventName)
	}
	if r.VersionID == "" {
		t.Fatalf("version_id missing on versioned bucket row")
	}
}

func TestReplicationNoMatchEnqueuesNothing(t *testing.T) {
	h := newReplicationHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h.testHarness, "bkt")
	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationPrefixXML), 200)

	resp := h.doString("PUT", "/bkt/other/file.txt", "hello")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "" {
		t.Fatalf("status: got %q want empty", got)
	}
	if rows := h.listReplications("bkt"); len(rows) != 0 {
		t.Fatalf("got %d rows want 0", len(rows))
	}
}

func TestReplicationPutBucketReplicationRejectsUnversioned(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?replication=", replicationPrefixXML)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("expected InvalidRequest, got: %s", body)
	}
}

func TestReplicationPutBucketReplicationAllowsSuspended(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	// Toggle to Enabled (allowed) then to Suspended (also allowed).
	enableVersioning(h, "bkt")
	h.mustStatus(h.doString("PUT", "/bkt?versioning",
		"<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationPrefixXML), 200)
}

func TestReplicationCapturesDestinationEndpoint(t *testing.T) {
	h := newReplicationHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h.testHarness, "bkt")

	const ruleXML = `<ReplicationConfiguration>
		<Role>arn:aws:iam::1:role/r</Role>
		<Rule>
			<ID>logs</ID>
			<Status>Enabled</Status>
			<Filter><Prefix>logs/</Prefix></Filter>
			<Destination>
				<Bucket>arn:aws:s3:::dest</Bucket>
				<AccessControlTranslation><Owner>peer.example.com:443</Owner></AccessControlTranslation>
			</Destination>
		</Rule>
	</ReplicationConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?replication=", ruleXML), 200)

	resp := h.doString("PUT", "/bkt/logs/2026/04.txt", "hello")
	h.mustStatus(resp, 200)

	rows := h.listReplications("bkt")
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	if rows[0].DestinationEndpoint != "peer.example.com:443" {
		t.Fatalf("endpoint=%q want peer.example.com:443", rows[0].DestinationEndpoint)
	}
}

func TestReplicationCompleteMultipartEnqueuesRow(t *testing.T) {
	h := newReplicationHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h.testHarness, "bkt")
	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationPrefixXML), 200)

	// Initiate multipart on a key matching the rule prefix.
	resp := h.doString("POST", "/bkt/logs/big.bin?uploads=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	uploadID := extractUploadID(body)
	if uploadID == "" {
		t.Fatalf("UploadId missing from body: %s", body)
	}

	// One part is enough; multipart must have at least one part.
	partBody := strings.Repeat("x", 16)
	resp = h.doString("PUT", "/bkt/logs/big.bin?partNumber=1&uploadId="+uploadID, partBody)
	h.mustStatus(resp, 200)
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)

	completeBody := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>\"" + etag + "\"</ETag></Part></CompleteMultipartUpload>"
	resp = h.doString("POST", "/bkt/logs/big.bin?uploadId="+uploadID, completeBody)
	h.mustStatus(resp, 200)

	rows := h.listReplications("bkt")
	if len(rows) != 1 {
		t.Fatalf("got %d rows want 1", len(rows))
	}
	r := rows[0]
	if r.Key != "logs/big.bin" || r.RuleID != "logs" {
		t.Fatalf("row: %+v", r)
	}
	if r.EventName != "s3:ObjectCreated:CompleteMultipartUpload" {
		t.Fatalf("event name: %q", r.EventName)
	}
}

