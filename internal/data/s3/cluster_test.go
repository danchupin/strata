package s3

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseClustersHappyPath(t *testing.T) {
	in := `[
		{"id":"primary","endpoint":"https://s3.example.com","region":"us-east-1","force_path_style":true,"part_size":16777216,"upload_concurrency":4,"max_retries":5,"op_timeout_secs":30,"credentials":{"type":"chain"}},
		{"id":"cold-eu","endpoint":"https://eu.example.com","region":"eu-west-1","sse_mode":"strata","credentials":{"type":"env","ref":"COLD_AK:COLD_SK"}},
		{"id":"file-cluster","endpoint":"https://file.example.com","region":"us-west-2","credentials":{"type":"file","ref":"/etc/strata/creds:profile-a"}}
	]`
	got, err := ParseClusters(in)
	if err != nil {
		t.Fatalf("ParseClusters: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3", len(got))
	}
	primary, ok := got["primary"]
	if !ok {
		t.Fatalf("missing primary")
	}
	if primary.Region != "us-east-1" || !primary.ForcePathStyle || primary.PartSize != 16777216 ||
		primary.UploadConcurrency != 4 || primary.MaxRetries != 5 || primary.OpTimeoutSecs != 30 {
		t.Fatalf("primary spec wrong: %+v", primary)
	}
	if primary.Credentials.Type != CredentialsChain {
		t.Fatalf("primary.Credentials.Type=%q", primary.Credentials.Type)
	}
	cold := got["cold-eu"]
	if cold.SSEMode != "strata" || cold.Credentials.Type != CredentialsEnv || cold.Credentials.Ref != "COLD_AK:COLD_SK" {
		t.Fatalf("cold spec wrong: %+v", cold)
	}
	fc := got["file-cluster"]
	if fc.Credentials.Type != CredentialsFile || fc.Credentials.Ref != "/etc/strata/creds:profile-a" {
		t.Fatalf("file-cluster spec wrong: %+v", fc)
	}
}

func TestParseClustersJSONRoundTrip(t *testing.T) {
	specs := map[string]S3ClusterSpec{
		"chain": {
			ID:                "chain",
			Endpoint:          "https://chain.example.com",
			Region:            "us-east-1",
			ForcePathStyle:    true,
			PartSize:          1 << 24,
			UploadConcurrency: 8,
			MaxRetries:        7,
			OpTimeoutSecs:     45,
			Credentials:       CredentialsRef{Type: CredentialsChain},
		},
		"env": {
			ID:          "env",
			Endpoint:    "https://env.example.com",
			Region:      "eu-west-1",
			SSEMode:     "strata",
			Credentials: CredentialsRef{Type: CredentialsEnv, Ref: "AK_VAR:SK_VAR"},
		},
		"file": {
			ID:          "file",
			Endpoint:    "https://file.example.com",
			Region:      "ap-south-1",
			SSEKMSKeyID: "alias/aws/s3",
			Credentials: CredentialsRef{Type: CredentialsFile, Ref: "/etc/aws/creds:default"},
		},
	}
	for name, spec := range specs {
		buf, err := json.Marshal([]S3ClusterSpec{spec})
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		got, err := ParseClusters(string(buf))
		if err != nil {
			t.Fatalf("%s parse: %v", name, err)
		}
		decoded, ok := got[spec.ID]
		if !ok {
			t.Fatalf("%s missing decoded entry", name)
		}
		if decoded.Credentials != spec.Credentials {
			t.Fatalf("%s creds mismatch: got %+v want %+v", name, decoded.Credentials, spec.Credentials)
		}
		if decoded.Endpoint != spec.Endpoint || decoded.Region != spec.Region {
			t.Fatalf("%s endpoint/region mismatch: got %+v", name, decoded)
		}
	}
}

func TestParseClustersEmptyReturnsEmptyMap(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t"} {
		got, err := ParseClusters(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("%q: len=%d, want 0", in, len(got))
		}
	}
}

func TestParseClustersRejectMalformed(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSub string
	}{
		{
			name:    "duplicate id",
			in:      `[{"id":"a","endpoint":"https://x","region":"r","credentials":{"type":"chain"}},{"id":"a","endpoint":"https://y","region":"r","credentials":{"type":"chain"}}]`,
			wantSub: "duplicate cluster id",
		},
		{
			name:    "empty id",
			in:      `[{"id":"","endpoint":"https://x","region":"r","credentials":{"type":"chain"}}]`,
			wantSub: "empty id",
		},
		{
			name:    "empty endpoint",
			in:      `[{"id":"a","endpoint":"","region":"r","credentials":{"type":"chain"}}]`,
			wantSub: "empty endpoint",
		},
		{
			name:    "empty region",
			in:      `[{"id":"a","endpoint":"https://x","region":"","credentials":{"type":"chain"}}]`,
			wantSub: "empty region",
		},
		{
			name:    "missing creds type",
			in:      `[{"id":"a","endpoint":"https://x","region":"r","credentials":{}}]`,
			wantSub: "credentials.type required",
		},
		{
			name:    "unknown creds type",
			in:      `[{"id":"a","endpoint":"https://x","region":"r","credentials":{"type":"kms"}}]`,
			wantSub: "unknown credentials.type",
		},
		{
			name:    "env ref missing colon",
			in:      `[{"id":"a","endpoint":"https://x","region":"r","credentials":{"type":"env","ref":"AKONLY"}}]`,
			wantSub: "ACCESS_KEY_VAR",
		},
		{
			name:    "env ref empty half",
			in:      `[{"id":"a","endpoint":"https://x","region":"r","credentials":{"type":"env","ref":"AK:"}}]`,
			wantSub: "ACCESS_KEY_VAR",
		},
		{
			name:    "chain with non-empty ref",
			in:      `[{"id":"a","endpoint":"https://x","region":"r","credentials":{"type":"chain","ref":"oops"}}]`,
			wantSub: "must have empty ref",
		},
		{
			name:    "file with empty ref",
			in:      `[{"id":"a","endpoint":"https://x","region":"r","credentials":{"type":"file","ref":""}}]`,
			wantSub: "<path>",
		},
		{
			name:    "malformed json",
			in:      `not json`,
			wantSub: "parse JSON",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClusters(tc.in)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err %q lacks %q", err.Error(), tc.wantSub)
			}
		})
	}
}
