package rados

import "errors"

// ErrRadosNotCompiled is the legacy in-package sentinel kept for callers
// that still reference it. New code should switch to data.ErrRADOSNotCompiled
// which is the canonical cross-package sentinel after the cephimpl/ split.
var ErrRadosNotCompiled = errors.New("rados backend not compiled; see github.com/danchupin/strata/cephimpl for the librados-linked impl")
