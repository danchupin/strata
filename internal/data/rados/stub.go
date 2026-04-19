//go:build !ceph

package rados

import (
	"errors"

	"github.com/danchupin/strata/internal/data"
)

func New(_ Config) (data.Backend, error) {
	return nil, errors.New(`rados backend requires build tag "ceph" with librados installed (build with: go build -tags ceph ./...)`)
}
