//go:build ceph

package serverapp

import (
	"github.com/danchupin/strata/cephimpl"
	"github.com/danchupin/strata/internal/data"
	datarados "github.com/danchupin/strata/internal/data/rados"
)

// newRADOSBackend wires the librados-linked cephimpl module under the
// `ceph` build tag. cephimpl was split out of internal/data/rados as a
// separate Go module so the main module's go.mod stays free of
// github.com/ceph/go-ceph; go.work at the repo root unifies both
// modules at dev time.
func newRADOSBackend(cfg datarados.Config) (data.Backend, error) {
	return cephimpl.New(cfg)
}
