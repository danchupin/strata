package rados

import "testing"

func TestBatchOpsFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{name: "empty defaults off", env: "", want: false},
		{name: "true on", env: "true", want: true},
		{name: "1 on", env: "1", want: true},
		{name: "false off", env: "false", want: false},
		{name: "0 off", env: "0", want: false},
		{name: "garbage falls back off", env: "yes-please", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STRATA_RADOS_BATCH_OPS", tc.env)
			if got := batchOpsFromEnv(); got != tc.want {
				t.Fatalf("batchOpsFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}
