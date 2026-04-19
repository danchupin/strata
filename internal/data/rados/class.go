package rados

import (
	"fmt"
	"strings"
)

const DefaultCluster = "default"

type ClassSpec struct {
	Cluster   string
	Pool      string
	Namespace string
}

// ParseClasses parses a STRATA_RADOS_CLASSES string.
// Format: CLASS=pool[@cluster[/namespace]], comma-separated.
// Example: "STANDARD=strata.rgw.buckets.data,STANDARD_IA=strata.rgw.buckets.ia@default/warm"
func ParseClasses(s string) (map[string]ClassSpec, error) {
	out := make(map[string]ClassSpec)
	if s == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, spec, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("rados classes: entry %q must be CLASS=pool[@cluster[/ns]]", entry)
		}
		cls := ClassSpec{Cluster: DefaultCluster}
		poolPart, loc, hasLoc := strings.Cut(spec, "@")
		cls.Pool = strings.TrimSpace(poolPart)
		if cls.Pool == "" {
			return nil, fmt.Errorf("rados classes: entry %q has empty pool", entry)
		}
		if hasLoc {
			clusterPart, ns, hasNS := strings.Cut(loc, "/")
			cls.Cluster = strings.TrimSpace(clusterPart)
			if hasNS {
				cls.Namespace = strings.TrimSpace(ns)
			}
		}
		out[strings.TrimSpace(name)] = cls
	}
	return out, nil
}
