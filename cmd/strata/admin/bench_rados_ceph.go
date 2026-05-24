//go:build ceph

package admin

import (
	"github.com/danchupin/strata/cephimpl"
	"github.com/danchupin/strata/internal/data"
	datarados "github.com/danchupin/strata/internal/data/rados"
)

func newBenchRADOSBackend(cfg datarados.Config) (data.Backend, error) {
	return cephimpl.New(cfg)
}
