//go:build !ceph

package rados

import "errors"

// ErrRadosNotCompiled is the sentinel surfaced by the !ceph build of the
// rados package. The ops batching helpers in ops.go require librados
// linkage (build tag ceph); this stub exists so non-librados builds keep
// a compileable reference for future call sites that need a typed error.
var ErrRadosNotCompiled = errors.New("rados ops require build tag ceph with librados installed")
