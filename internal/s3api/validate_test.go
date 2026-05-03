package s3api

import "testing"

func TestValidBucketName_Reserved(t *testing.T) {
	for _, name := range []string{"console", "admin", "metrics"} {
		if validBucketName(name) {
			t.Errorf("validBucketName(%q) = true, want false (reserved gateway prefix)", name)
		}
	}
}

func TestValidBucketName_AcceptsCommon(t *testing.T) {
	for _, name := range []string{"my-bucket", "logs.2026", "abc"} {
		if !validBucketName(name) {
			t.Errorf("validBucketName(%q) = false, want true", name)
		}
	}
}
