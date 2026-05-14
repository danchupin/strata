package rados

import (
	"strings"
	"testing"
)

func TestParseClassesEmpty(t *testing.T) {
	got, err := ParseClasses("")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestParseClassesPoolOnly(t *testing.T) {
	got, err := ParseClasses("STANDARD=hot.pool")
	if err != nil {
		t.Fatalf("pool only: %v", err)
	}
	spec, ok := got["STANDARD"]
	if !ok {
		t.Fatalf("missing STANDARD: %v", got)
	}
	if spec.Pool != "hot.pool" {
		t.Errorf("Pool=%q", spec.Pool)
	}
	if spec.Cluster != DefaultCluster {
		t.Errorf("Cluster=%q want %q", spec.Cluster, DefaultCluster)
	}
	if spec.Namespace != "" {
		t.Errorf("Namespace=%q want empty", spec.Namespace)
	}
}

func TestParseClassesWithCluster(t *testing.T) {
	got, err := ParseClasses("COLD=cold.pool@cold-eu")
	if err != nil {
		t.Fatalf("with cluster: %v", err)
	}
	spec, ok := got["COLD"]
	if !ok {
		t.Fatalf("missing COLD: %v", got)
	}
	if spec.Pool != "cold.pool" {
		t.Errorf("Pool=%q", spec.Pool)
	}
	if spec.Cluster != "cold-eu" {
		t.Errorf("Cluster=%q", spec.Cluster)
	}
	if spec.Namespace != "" {
		t.Errorf("Namespace=%q want empty", spec.Namespace)
	}
}

func TestParseClassesWithClusterAndNamespace(t *testing.T) {
	got, err := ParseClasses("STANDARD=hot.pool,COLD=cold.pool@cold-eu/warm,GLACIER=ice.pool@cold-eu/frozen")
	if err != nil {
		t.Fatalf("multi: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d (%v)", len(got), got)
	}
	if got["STANDARD"].Cluster != DefaultCluster {
		t.Errorf("STANDARD cluster=%q want default", got["STANDARD"].Cluster)
	}
	cold := got["COLD"]
	if cold.Pool != "cold.pool" || cold.Cluster != "cold-eu" || cold.Namespace != "warm" {
		t.Errorf("COLD parsed wrong: %+v", cold)
	}
	gl := got["GLACIER"]
	if gl.Pool != "ice.pool" || gl.Cluster != "cold-eu" || gl.Namespace != "frozen" {
		t.Errorf("GLACIER parsed wrong: %+v", gl)
	}
}

func TestParseClassesNamespaceWithoutCluster(t *testing.T) {
	// "@" is required before "/" — without "@", "/" stays in the pool name.
	// This is the documented format; pool names with "/" are not legal in Ceph
	// anyway, so the lexical rule is fine.
	got, err := ParseClasses("X=p@/ns")
	if err != nil {
		t.Fatalf("at-slash-ns: %v", err)
	}
	spec := got["X"]
	if spec.Cluster != "" {
		t.Errorf("Cluster=%q want empty (parsed left of /ns)", spec.Cluster)
	}
	if spec.Namespace != "ns" {
		t.Errorf("Namespace=%q want ns", spec.Namespace)
	}
}

// ClusterPinned tracks whether the operator wrote an explicit `@cluster`
// suffix in STRATA_RADOS_CLASSES. PutChunks consults it under US-002
// cluster-weights: pinned classes bypass default-routing synthesis.
func TestParseClassesClusterPinnedFlag(t *testing.T) {
	got, err := ParseClasses("STANDARD=hot.pool,COLD=cold.pool@cold-eu,WARM=warm.pool@default/ns")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["STANDARD"].ClusterPinned {
		t.Errorf("STANDARD without @ suffix: ClusterPinned must be false")
	}
	if !got["COLD"].ClusterPinned {
		t.Errorf("COLD with @cold-eu: ClusterPinned must be true")
	}
	if !got["WARM"].ClusterPinned {
		t.Errorf("WARM with @default/ns: ClusterPinned must be true (explicit pin to default)")
	}
}

func TestParseClassesErrors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"NOEQ", "must be CLASS"},
		{"EMPTY=", "empty pool"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			_, err := ParseClasses(c.in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}
