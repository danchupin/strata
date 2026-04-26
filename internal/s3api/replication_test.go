package s3api_test

import (
	"strings"
	"testing"
)

const replicationPrefixXML = `<ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Role>arn:aws:iam::123456789012:role/repl</Role>
	<Rule>
		<ID>logs</ID>
		<Status>Enabled</Status>
		<Filter>
			<Prefix>logs/</Prefix>
		</Filter>
		<Destination>
			<Bucket>arn:aws:s3:::dest</Bucket>
		</Destination>
	</Rule>
</ReplicationConfiguration>`

const replicationTagXML = `<ReplicationConfiguration>
	<Role>arn:aws:iam::123456789012:role/repl</Role>
	<Rule>
		<ID>tagged</ID>
		<Status>Enabled</Status>
		<Filter>
			<And>
				<Prefix>data/</Prefix>
				<Tag><Key>repl</Key><Value>yes</Value></Tag>
			</And>
		</Filter>
		<Destination>
			<Bucket>arn:aws:s3:::dest</Bucket>
		</Destination>
	</Rule>
</ReplicationConfiguration>`

func TestBucketReplicationCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?replication=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "ReplicationConfigurationNotFoundError") {
		t.Fatalf("expected ReplicationConfigurationNotFoundError, got: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationPrefixXML), 200)

	resp = h.doString("GET", "/bkt?replication=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<ID>logs</ID>") || !strings.Contains(body, "arn:aws:s3:::dest") {
		t.Fatalf("GET replication body missing rule: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?replication=", ""), 204)

	resp = h.doString("GET", "/bkt?replication=", "")
	h.mustStatus(resp, 404)
}

func TestBucketReplicationMalformedBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt?replication=", "<ReplicationConfiguration><nope")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML, got: %s", body)
	}
}

func TestBucketReplicationNoRulesRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt?replication=",
		"<ReplicationConfiguration><Role>arn:aws:iam::1:role/r</Role></ReplicationConfiguration>")
	h.mustStatus(resp, 400)
}

func TestBucketReplicationRuleWithoutDestinationRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	xmlBody := `<ReplicationConfiguration>
		<Role>arn:aws:iam::1:role/r</Role>
		<Rule><ID>no-dest</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter></Rule>
	</ReplicationConfiguration>`
	resp := h.doString("PUT", "/bkt?replication=", xmlBody)
	h.mustStatus(resp, 400)
}

func TestPutObjectReplicationStatusMatchPrefix(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationPrefixXML), 200)

	resp := h.doString("PUT", "/bkt/logs/2026/04.txt", "hello")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "PENDING" {
		t.Fatalf("replication-status: got %q want PENDING", got)
	}
}

func TestPutObjectReplicationStatusNoMatch(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationPrefixXML), 200)

	resp := h.doString("PUT", "/bkt/other/file.txt", "hello")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "" {
		t.Fatalf("replication-status: got %q want empty", got)
	}
}

func TestPutObjectReplicationStatusNoConfig(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/anything.txt", "hello")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "" {
		t.Fatalf("replication-status: got %q want empty without config", got)
	}
}

func TestPutObjectReplicationStatusDisabledRuleSkipped(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	disabled := strings.Replace(replicationPrefixXML, "<Status>Enabled</Status>", "<Status>Disabled</Status>", 1)
	h.mustStatus(h.doString("PUT", "/bkt?replication=", disabled), 200)

	resp := h.doString("PUT", "/bkt/logs/2026/04.txt", "hello")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "" {
		t.Fatalf("replication-status: got %q want empty for disabled rule", got)
	}
}

func TestPutObjectReplicationStatusTagFilter(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?replication=", replicationTagXML), 200)

	// Prefix matches but tags missing → no replication.
	resp := h.doString("PUT", "/bkt/data/file.txt", "x")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "" {
		t.Fatalf("missing tag: got %q want empty", got)
	}

	// Prefix matches and tag matches → PENDING.
	resp = h.doString("PUT", "/bkt/data/file2.txt", "x", "x-amz-tagging", "repl=yes")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "PENDING" {
		t.Fatalf("with tag: got %q want PENDING", got)
	}

	// Tag wrong value → no replication.
	resp = h.doString("PUT", "/bkt/data/file3.txt", "x", "x-amz-tagging", "repl=no")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "" {
		t.Fatalf("wrong tag value: got %q want empty", got)
	}

	// Wrong prefix → no replication even with tag.
	resp = h.doString("PUT", "/bkt/other/file.txt", "x", "x-amz-tagging", "repl=yes")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-replication-status"); got != "" {
		t.Fatalf("wrong prefix: got %q want empty", got)
	}
}
