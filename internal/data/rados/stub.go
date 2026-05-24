package rados

import (
	"github.com/danchupin/strata/internal/data"
)

// New is the stub constructor in the main module's rados package. The real
// librados-backed Backend lives in the separate cephimpl/ Go module
// (`github.com/danchupin/strata/cephimpl`) so the main module's go.mod
// stays free of github.com/ceph/go-ceph. Callers that need the real
// backend must import cephimpl directly and call cephimpl.New; this stub
// stays as the typed entry point for code paths that have no business
// pulling in librados (default-tag builds, unit tests, tooling).
func New(_ Config) (data.Backend, error) {
	return nil, data.ErrRADOSNotCompiled
}
