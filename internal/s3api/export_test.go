package s3api

// SetMultipartMinPartSizeForTest overrides the 5 MiB minimum that
// CompleteMultipartUpload enforces on non-last parts. Returns a restore
// function for t.Cleanup. Tests that exercise multipart surfaces unrelated
// to size validation use this to keep their part bodies small (and the
// unit suite fast).
func SetMultipartMinPartSizeForTest(n int64) (restore func()) {
	prev := multipartMinPartSize
	multipartMinPartSize = n
	return func() { multipartMinPartSize = prev }
}
