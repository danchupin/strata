package s3

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CredentialsRef is the per-cluster credential discriminator. Plaintext
// access keys never appear in cluster JSON; the spec carries a Type label
// + a reference string that the connection builder resolves at client
// build time (US-002).
//
//	Type="chain": SDK default chain (env / shared config / IRSA / IMDS).
//	  Ref must be empty.
//	Type="env":   read two named env vars at client-build time. Ref has
//	  the shape "<ACCESS_KEY_VAR>:<SECRET_KEY_VAR>" — colon-separated.
//	Type="file":  load a shared-config-style credentials file. Ref has
//	  the shape "<path>[:<profile>]" — profile defaults to "default" if
//	  the suffix is omitted.
type CredentialsRef struct {
	Type string `json:"type"`
	Ref  string `json:"ref,omitempty"`
}

// CredentialsRef.Type values. Keep in sync with the validation in
// ParseClusters + the resolver in connFor (US-002).
const (
	CredentialsChain = "chain"
	CredentialsEnv   = "env"
	CredentialsFile  = "file"
)

// S3ClusterSpec is the per-cluster connection config. Bucket-less — the
// per-class ClassSpec carries the bucket name. Two classes can therefore
// share one S3 cluster but route to different buckets.
//
// TLS, when non-nil, overrides the global Config.TLS bundle ENTIRELY for
// this cluster (no merge — any single key on the per-cluster block replaces
// the global block to avoid surprise semantics when one knob is omitted).
type S3ClusterSpec struct {
	ID                string         `json:"id"`
	Endpoint          string         `json:"endpoint"`
	Region            string         `json:"region"`
	SSEMode           string         `json:"sse_mode,omitempty"`
	SSEKMSKeyID       string         `json:"sse_kms_key_id,omitempty"`
	ForcePathStyle    bool           `json:"force_path_style,omitempty"`
	PartSize          int64          `json:"part_size,omitempty"`
	UploadConcurrency int64          `json:"upload_concurrency,omitempty"`
	MaxRetries        int64          `json:"max_retries,omitempty"`
	OpTimeoutSecs     int            `json:"op_timeout_secs,omitempty"`
	Credentials       CredentialsRef `json:"credentials"`
	TLS               *ClusterTLS    `json:"tls,omitempty"`
}

// ClusterTLS is the per-cluster mTLS bundle for the S3-upstream backend
// (US-006 harden-gateway). Presence on S3ClusterSpec replaces the global
// Config.TLS block for the cluster outright — there is no merge.
// CertFile + KeyFile must both be set or both unset; CAFile alone enables
// server-cert pinning without a client cert.
type ClusterTLS struct {
	CAFile     string `json:"ca_file,omitempty"`
	CertFile   string `json:"cert_file,omitempty"`
	KeyFile    string `json:"key_file,omitempty"`
	SkipVerify bool   `json:"skip_verify,omitempty"`
}

// HasAny returns true when any TLS knob is set — the resolver treats a
// fully-zero struct as "use global default" rather than as an explicit
// "force plain HTTP" override.
func (t ClusterTLS) HasAny() bool {
	return t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.SkipVerify
}

// ParseClusters parses a STRATA_S3_CLUSTERS JSON array into an id->spec
// map. Each element must carry a non-empty id, endpoint, and region, plus
// a recognised CredentialsRef.Type. Cross-validation against the class
// map (`ClassSpec.Cluster` references a known cluster id) happens at
// Backend construction time, not here — keeps the parser
// backend-independent.
func ParseClusters(jsonStr string) (map[string]S3ClusterSpec, error) {
	out := make(map[string]S3ClusterSpec)
	if strings.TrimSpace(jsonStr) == "" {
		return out, nil
	}
	var specs []S3ClusterSpec
	if err := json.Unmarshal([]byte(jsonStr), &specs); err != nil {
		return nil, fmt.Errorf("s3 clusters: parse JSON: %w", err)
	}
	for _, spec := range specs {
		spec.ID = strings.TrimSpace(spec.ID)
		spec.Endpoint = strings.TrimSpace(spec.Endpoint)
		spec.Region = strings.TrimSpace(spec.Region)
		if spec.ID == "" {
			return nil, fmt.Errorf("s3 clusters: entry has empty id")
		}
		if spec.Endpoint == "" {
			return nil, fmt.Errorf("s3 clusters: cluster %q has empty endpoint", spec.ID)
		}
		if spec.Region == "" {
			return nil, fmt.Errorf("s3 clusters: cluster %q has empty region", spec.ID)
		}
		if _, dup := out[spec.ID]; dup {
			return nil, fmt.Errorf("s3 clusters: duplicate cluster id %q", spec.ID)
		}
		if err := validateCredentialsRef(spec.ID, spec.Credentials); err != nil {
			return nil, err
		}
		if spec.TLS != nil {
			if (spec.TLS.CertFile == "") != (spec.TLS.KeyFile == "") {
				return nil, fmt.Errorf("s3 clusters: cluster %q tls.cert_file and tls.key_file must both be set or both unset",
					spec.ID)
			}
		}
		out[spec.ID] = spec
	}
	return out, nil
}

// validateCredentialsRef enforces the discriminator contract documented
// on CredentialsRef. Plaintext creds are rejected by shape — `env:` and
// `file:` types must carry a non-empty Ref; `chain` must carry an empty
// Ref. The Ref shape is also checked: env requires exactly one colon
// separating the two variable names; file accepts an optional :profile
// suffix.
func validateCredentialsRef(clusterID string, ref CredentialsRef) error {
	switch ref.Type {
	case CredentialsChain:
		if strings.TrimSpace(ref.Ref) != "" {
			return fmt.Errorf("s3 clusters: cluster %q credentials.type=%q must have empty ref, got %q",
				clusterID, CredentialsChain, ref.Ref)
		}
		return nil
	case CredentialsEnv:
		parts := strings.Split(ref.Ref, ":")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return fmt.Errorf("s3 clusters: cluster %q credentials.type=%q ref must be <ACCESS_KEY_VAR>:<SECRET_KEY_VAR>, got %q",
				clusterID, CredentialsEnv, ref.Ref)
		}
		return nil
	case CredentialsFile:
		path := ref.Ref
		if idx := strings.LastIndex(path, ":"); idx > 0 {
			path = path[:idx]
		}
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("s3 clusters: cluster %q credentials.type=%q ref must be <path>[:<profile>], got %q",
				clusterID, CredentialsFile, ref.Ref)
		}
		return nil
	case "":
		return fmt.Errorf("s3 clusters: cluster %q credentials.type required (one of %q/%q/%q)",
			clusterID, CredentialsChain, CredentialsEnv, CredentialsFile)
	default:
		return fmt.Errorf("s3 clusters: cluster %q unknown credentials.type %q (want %q/%q/%q)",
			clusterID, ref.Type, CredentialsChain, CredentialsEnv, CredentialsFile)
	}
}
