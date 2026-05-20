//go:build !ceph

package rados

// Build-tag parity placeholder for the ceph-tagged connPool defined in
// pool.go. Backend construction lives behind the same !ceph stub in
// stub.go, so no caller references the type on this side.
