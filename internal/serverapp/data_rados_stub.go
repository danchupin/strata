//go:build !ceph

package serverapp

import (
	"github.com/danchupin/strata/internal/data"
	datarados "github.com/danchupin/strata/internal/data/rados"
)

// newRADOSBackend returns the not-compiled sentinel on default-tag
// builds. Real ceph wiring lives in data_rados_ceph.go (build tag
// `ceph`) and pulls in github.com/danchupin/strata/cephimpl — the
// librados-linked Go module split out of main during the
// auth-dx-trailer-lima cycle (US-003).
func newRADOSBackend(_ datarados.Config) (data.Backend, error) {
	return nil, data.ErrRADOSNotCompiled
}
