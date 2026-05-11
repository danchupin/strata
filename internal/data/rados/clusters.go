package rados

// ClusterSpec is the per-RADOS-cluster connection config. ID is the operator
// label (e.g. "default", "cold-eu") referenced by ClassSpec.Cluster. ConfigFile
// is the path to the cluster's ceph.conf; Keyring overrides the cephx keyring
// path; User is the cephx client name (defaults to "admin" at dial time).
//
// Decoded from the opaque JSON Spec column of a cluster_registry row by the
// RegistryWatcher's reconcile path; tests + the bench harness populate
// Config.Clusters with this shape directly.
type ClusterSpec struct {
	ID         string
	ConfigFile string
	Keyring    string
	User       string
}
