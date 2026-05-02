package rados

import (
	"strings"
	"testing"
)

func TestParseClustersEmpty(t *testing.T) {
	got, err := ParseClusters("")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestParseClustersSingle(t *testing.T) {
	got, err := ParseClusters("default:/etc/ceph/ceph.conf:/etc/ceph/ceph.client.admin.keyring")
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	spec, ok := got["default"]
	if !ok {
		t.Fatalf("missing default entry: %v", got)
	}
	if spec.ID != "default" {
		t.Errorf("ID=%q want default", spec.ID)
	}
	if spec.ConfigFile != "/etc/ceph/ceph.conf" {
		t.Errorf("ConfigFile=%q", spec.ConfigFile)
	}
	if spec.Keyring != "/etc/ceph/ceph.client.admin.keyring" {
		t.Errorf("Keyring=%q", spec.Keyring)
	}
}

func TestParseClustersMulti(t *testing.T) {
	got, err := ParseClusters("default:/etc/ceph/ceph.conf:/etc/ceph/ceph.keyring,cold-eu:/etc/ceph/cold.conf:/etc/ceph/cold.keyring")
	if err != nil {
		t.Fatalf("multi: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got["default"].ConfigFile != "/etc/ceph/ceph.conf" {
		t.Errorf("default conf wrong: %q", got["default"].ConfigFile)
	}
	if got["cold-eu"].Keyring != "/etc/ceph/cold.keyring" {
		t.Errorf("cold-eu keyring wrong: %q", got["cold-eu"].Keyring)
	}
}

func TestParseClustersOptionalKeyring(t *testing.T) {
	got, err := ParseClusters("default:/etc/ceph/ceph.conf")
	if err != nil {
		t.Fatalf("optional keyring: %v", err)
	}
	if got["default"].Keyring != "" {
		t.Errorf("expected empty keyring, got %q", got["default"].Keyring)
	}
}

func TestParseClustersWhitespace(t *testing.T) {
	got, err := ParseClusters("  default : /etc/ceph/ceph.conf : /etc/ceph/ceph.keyring  , ")
	if err != nil {
		t.Fatalf("whitespace: %v", err)
	}
	spec, ok := got["default"]
	if !ok {
		t.Fatalf("missing default entry: %v", got)
	}
	if spec.ConfigFile != "/etc/ceph/ceph.conf" {
		t.Errorf("ConfigFile=%q", spec.ConfigFile)
	}
}

func TestParseClustersErrors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"justanid", "must be"},
		{":/etc/ceph/ceph.conf:k", "empty cluster id"},
		{"default::k", "empty config path"},
		{"default:/a,default:/b", "duplicate cluster id"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			_, err := ParseClusters(c.in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestBuildClustersFromMultiOnly(t *testing.T) {
	cfg := Config{
		Clusters: map[string]ClusterSpec{
			"alpha": {ConfigFile: "/etc/alpha.conf"},
		},
	}
	got, err := BuildClusters(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got["alpha"].ID != "alpha" {
		t.Errorf("ID=%q want alpha", got["alpha"].ID)
	}
}

func TestBuildClustersFromLegacyOnly(t *testing.T) {
	cfg := Config{ConfigFile: "/etc/ceph/ceph.conf", Keyring: "/etc/ceph/ceph.keyring", User: "client.admin"}
	got, err := BuildClusters(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	spec, ok := got[DefaultCluster]
	if !ok {
		t.Fatalf("missing default entry: %v", got)
	}
	if spec.ConfigFile != "/etc/ceph/ceph.conf" {
		t.Errorf("legacy ConfigFile not threaded: %q", spec.ConfigFile)
	}
	if spec.Keyring != "/etc/ceph/ceph.keyring" {
		t.Errorf("legacy Keyring not threaded: %q", spec.Keyring)
	}
	if spec.User != "client.admin" {
		t.Errorf("legacy User not threaded: %q", spec.User)
	}
}

func TestBuildClustersMixed(t *testing.T) {
	cfg := Config{
		ConfigFile: "/etc/ceph/ceph.conf",
		Clusters: map[string]ClusterSpec{
			"cold-eu": {ConfigFile: "/etc/cold.conf"},
		},
	}
	got, err := BuildClusters(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d (%v)", len(got), got)
	}
	if _, ok := got[DefaultCluster]; !ok {
		t.Errorf("default entry missing when legacy fields present")
	}
	if _, ok := got["cold-eu"]; !ok {
		t.Errorf("cold-eu entry missing")
	}
}

func TestBuildClustersMultiOverridesDefault(t *testing.T) {
	// When the multi-cluster map already supplies "default", legacy fields
	// MUST NOT overwrite it.
	cfg := Config{
		ConfigFile: "/etc/legacy.conf",
		Clusters: map[string]ClusterSpec{
			DefaultCluster: {ConfigFile: "/etc/explicit.conf"},
		},
	}
	got, err := BuildClusters(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got[DefaultCluster].ConfigFile != "/etc/explicit.conf" {
		t.Errorf("legacy clobbered explicit default: %q", got[DefaultCluster].ConfigFile)
	}
}

func TestBuildClustersEmpty(t *testing.T) {
	_, err := BuildClusters(Config{})
	if err == nil {
		t.Fatal("expected error when no clusters configured")
	}
	if !strings.Contains(err.Error(), "no clusters configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateClusterRefsOK(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD": {Cluster: "default", Pool: "p"},
		"COLD":     {Cluster: "cold", Pool: "p"},
	}
	clusters := map[string]ClusterSpec{
		"default": {ID: "default"},
		"cold":    {ID: "cold"},
	}
	if err := ValidateClusterRefs(classes, clusters); err != nil {
		t.Fatalf("ok case: %v", err)
	}
}

func TestValidateClusterRefsEmptyClusterDefaults(t *testing.T) {
	classes := map[string]ClassSpec{"STANDARD": {Pool: "p"}} // Cluster=""
	clusters := map[string]ClusterSpec{DefaultCluster: {ID: DefaultCluster}}
	if err := ValidateClusterRefs(classes, clusters); err != nil {
		t.Fatalf("empty cluster should default to %s: %v", DefaultCluster, err)
	}
}

func TestValidateClusterRefsMissing(t *testing.T) {
	classes := map[string]ClassSpec{
		"COLD": {Cluster: "cold-eu", Pool: "p"},
	}
	clusters := map[string]ClusterSpec{DefaultCluster: {ID: DefaultCluster}}
	err := ValidateClusterRefs(classes, clusters)
	if err == nil {
		t.Fatal("expected missing-cluster error")
	}
	if !strings.Contains(err.Error(), "cold-eu") {
		t.Errorf("error should mention cold-eu: %v", err)
	}
}
