//go:build !ceph

package admin

import (
	"github.com/danchupin/strata/internal/data"
	datarados "github.com/danchupin/strata/internal/data/rados"
)

func newBenchRADOSBackend(_ datarados.Config) (data.Backend, error) {
	return nil, data.ErrRADOSNotCompiled
}
