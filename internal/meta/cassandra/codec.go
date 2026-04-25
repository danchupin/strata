package cassandra

import (
	"encoding/json"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

func encodeManifest(m *data.Manifest) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

func decodeManifest(b []byte) (*data.Manifest, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m data.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func encodeGrants(g []meta.Grant) ([]byte, error) {
	if len(g) == 0 {
		return nil, nil
	}
	return json.Marshal(g)
}

func decodeGrants(b []byte) ([]meta.Grant, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var g []meta.Grant
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, err
	}
	return g, nil
}
