package s3

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ClassSpec is the per-storage-class routing entry: maps a Strata
// storage-class label (e.g. "STANDARD", "COLD") to a (cluster, bucket)
// pair. Both fields are REQUIRED — there is no DefaultCluster fallback.
// resolveClass on Backend returns ErrClassMissingCluster when Cluster is
// empty and ErrUnknownStorageClass when the class label is missing.
type ClassSpec struct {
	Cluster string `json:"cluster"`
	Bucket  string `json:"bucket"`
}

// ParseClasses parses a STRATA_S3_CLASSES JSON object (class -> spec)
// into the in-memory map. Empty class name, empty Cluster, or empty
// Bucket per entry are rejected here; cross-validation against the
// cluster map (`ClassSpec.Cluster` references a known cluster id)
// happens at Backend construction time.
func ParseClasses(jsonStr string) (map[string]ClassSpec, error) {
	out := make(map[string]ClassSpec)
	if strings.TrimSpace(jsonStr) == "" {
		return out, nil
	}
	var raw map[string]ClassSpec
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("s3 classes: parse JSON: %w", err)
	}
	for name, spec := range raw {
		name = strings.TrimSpace(name)
		spec.Cluster = strings.TrimSpace(spec.Cluster)
		spec.Bucket = strings.TrimSpace(spec.Bucket)
		if name == "" {
			return nil, fmt.Errorf("s3 classes: entry has empty class name")
		}
		if spec.Cluster == "" {
			return nil, fmt.Errorf("s3 classes: class %q has empty cluster", name)
		}
		if spec.Bucket == "" {
			return nil, fmt.Errorf("s3 classes: class %q has empty bucket", name)
		}
		out[name] = spec
	}
	return out, nil
}
