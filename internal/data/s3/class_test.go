package s3

import (
	"strings"
	"testing"
)

func TestParseClassesHappyPath(t *testing.T) {
	in := `{"STANDARD":{"cluster":"primary","bucket":"hot-tier"},"COLD":{"cluster":"cold-eu","bucket":"glacier-eu"}}`
	got, err := ParseClasses(in)
	if err != nil {
		t.Fatalf("ParseClasses: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	std := got["STANDARD"]
	if std.Cluster != "primary" || std.Bucket != "hot-tier" {
		t.Fatalf("STANDARD wrong: %+v", std)
	}
	cold := got["COLD"]
	if cold.Cluster != "cold-eu" || cold.Bucket != "glacier-eu" {
		t.Fatalf("COLD wrong: %+v", cold)
	}
}

func TestParseClassesEmptyReturnsEmptyMap(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t"} {
		got, err := ParseClasses(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("%q: len=%d, want 0", in, len(got))
		}
	}
}

func TestParseClassesRejectMalformed(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSub string
	}{
		{
			name:    "empty cluster",
			in:      `{"STANDARD":{"cluster":"","bucket":"b"}}`,
			wantSub: "empty cluster",
		},
		{
			name:    "empty bucket",
			in:      `{"STANDARD":{"cluster":"c","bucket":""}}`,
			wantSub: "empty bucket",
		},
		{
			name:    "missing cluster field",
			in:      `{"STANDARD":{"bucket":"b"}}`,
			wantSub: "empty cluster",
		},
		{
			name:    "missing bucket field",
			in:      `{"STANDARD":{"cluster":"c"}}`,
			wantSub: "empty bucket",
		},
		{
			name:    "empty class name",
			in:      `{"":{"cluster":"c","bucket":"b"}}`,
			wantSub: "empty class name",
		},
		{
			name:    "malformed json",
			in:      `not json`,
			wantSub: "parse JSON",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClasses(tc.in)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err %q lacks %q", err.Error(), tc.wantSub)
			}
		})
	}
}
