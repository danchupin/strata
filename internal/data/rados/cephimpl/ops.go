package cephimpl

import (
	"fmt"

	goceph "github.com/ceph/go-ceph/rados"
)

// writeChunkBatched bundles WriteFull + per-xattr SetXattr into one
// librados WriteOp so the PUT path pays one round-trip regardless of
// xattr count. xattrs may be nil — degrades to a single WriteFull,
// byte-identical to writeChunk. Wired in via the STRATA_RADOS_BATCH_OPS
// toggle (see rados.BatchOpsFromEnv).
func writeChunkBatched(ioctx *goceph.IOContext, oid string, body []byte, xattrs map[string]string) error {
	op := goceph.CreateWriteOp()
	defer op.Release()
	op.WriteFull(body)
	for k, v := range xattrs {
		op.SetXattr(k, []byte(v))
	}
	if err := op.Operate(ioctx, oid, goceph.OperationNoFlag); err != nil {
		return fmt.Errorf("rados: write %s: %w", oid, err)
	}
	return nil
}

// readChunkBatched performs a ReadOp.Read into a freshly-allocated buffer
// covering [off, off+length). When wantXattrs is true a follow-up
// ioctx.ListXattrs runs to populate the xattrs map — librados in go-ceph
// v0.39 does not expose `rados_read_op_getxattrs`.
func readChunkBatched(ioctx *goceph.IOContext, oid string, off uint64, length int64, wantXattrs bool) ([]byte, map[string]string, error) {
	if length < 0 {
		return nil, nil, fmt.Errorf("rados: read %s: negative length %d", oid, length)
	}
	op := goceph.CreateReadOp()
	defer op.Release()
	buf := make([]byte, length)
	step := op.Read(off, buf)
	if err := op.Operate(ioctx, oid, goceph.OperationNoFlag); err != nil {
		return nil, nil, fmt.Errorf("rados: read %s: %w", oid, err)
	}
	out := buf[:step.BytesRead]
	if !wantXattrs {
		return out, nil, nil
	}
	raw, err := ioctx.ListXattrs(oid)
	if err != nil {
		return nil, nil, fmt.Errorf("rados: list xattrs %s: %w", oid, err)
	}
	xattrs := make(map[string]string, len(raw))
	for k, v := range raw {
		xattrs[k] = string(v)
	}
	return out, xattrs, nil
}
