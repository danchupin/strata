package cassandra

import (
	"encoding/json"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

func encodeManifest(m *data.Manifest) ([]byte, error) {
	return data.EncodeManifest(m)
}

func decodeManifest(b []byte) (*data.Manifest, error) {
	return data.DecodeManifest(b)
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
