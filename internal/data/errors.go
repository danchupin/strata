package data

import "errors"

// ErrNotFound is returned by data.Backend implementations when the
// requested object does not exist (e.g. backend NoSuchKey). Gateway
// callers map this to S3 404 NoSuchKey instead of 500.
var ErrNotFound = errors.New("data: object not found")
