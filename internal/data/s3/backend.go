// Package s3 implements an S3-compatible data backend for Strata.
//
// US-002 (ralph/s3-multi-cluster) lifted Backend from single-bucket-per-
// instance into a multi-cluster shape: a map of per-cluster S3 SDK clients
// (built lazily by connFor) plus a per-storage-class routing table that
// maps each class to a (cluster, bucket) pair. Each per-cluster s3Cluster
// carries its own SSE / part-size / op-timeout config from S3ClusterSpec.
//
// The legacy single-cluster Open(ctx, Config{Bucket,Region,...}) entry-
// point is retained as a code-only compatibility shim — it synthesises a
// one-entry clusters map + one-entry classes map so existing in-package
// tests keep compiling until US-005 migrates them to the s3test fixture.
// US-004 retired the STRATA_S3_BACKEND_* env path; the gateway boots
// only from STRATA_S3_CLUSTERS + STRATA_S3_CLASSES via s3.New.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/logging"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
)

// BackendName is the canonical Manifest.BackendRef.Backend value emitted by
// the s3-over-s3 data backend.
const BackendName = "s3"

// ErrUnknownStorageClass is returned by resolveClass when the supplied
// storage-class label is not present in Backend.classes.
var ErrUnknownStorageClass = errors.New("s3: unknown storage class")

// ErrClassMissingCluster is returned by resolveClass when the matched
// ClassSpec.Cluster is empty.
var ErrClassMissingCluster = errors.New("s3: class missing cluster")

// Backend is the S3-over-S3 data backend. Holds a map of per-cluster
// connections + a per-class routing table.
//
// A zero-value Backend is a "stub" — clusters/classes are nil and every
// data-plane method returns errors.ErrUnsupported. Tests use this shape
// to exercise the not-wired path without paying for a synthetic backend.
type Backend struct {
	clusters map[string]*s3Cluster
	classes  map[string]ClassSpec
	mu       sync.Mutex

	// httpClient is the per-Backend HTTP client override (tests inject
	// synthetic transports). Applied to every cluster's SDK config on
	// first connFor.
	httpClient *http.Client
}

// s3Cluster carries the per-cluster SDK wiring + resolved config knobs.
// Built lazily by connFor on first use, cached in Backend.clusters under
// b.mu so concurrent first-use is race-free.
type s3Cluster struct {
	spec             S3ClusterSpec
	client           *awss3.Client
	uploader         *manager.Uploader
	opTimeout        time.Duration
	multipartTimeout time.Duration
	sseMode          string
	sseKMSKeyID      string

	// legacyAccessKey / legacySecretKey are the plaintext static creds
	// supplied by the deprecated single-cluster Open() shape. New() and
	// the env-driven multi-cluster path leave these empty — credentials
	// flow through spec.Credentials instead.
	legacyAccessKey string
	legacySecretKey string
}

// Config carries the wiring needed to build a Backend. Two coexisting
// shapes during the US-002 → US-004 migration:
//
//   - Multi-cluster (preferred, target shape for US-004): set
//     Clusters + Classes. Every class's Cluster must reference a known
//     cluster id; every cluster's Credentials must resolve at New time.
//
//   - Legacy single-cluster: set Endpoint/Region/Bucket/AccessKey/...
//     The Open(ctx, cfg) entry-point synthesises a single-entry Clusters
//     map under "default" and a single-entry Classes map under "STANDARD".
//     Retired by US-004.
type Config struct {
	// Multi-cluster shape.
	Clusters map[string]S3ClusterSpec
	Classes  map[string]ClassSpec

	// Legacy single-cluster fields. Deprecated — retired by US-004.
	Endpoint          string
	Region            string
	Bucket            string
	AccessKey         string
	SecretKey         string
	ForcePathStyle    bool
	PartSize          int64
	UploadConcurrency int
	MaxRetries        int
	OpTimeout         time.Duration
	MultipartTimeout  time.Duration
	SSEMode           string
	SSEKMSKeyID       string

	// HTTPClient overrides the SDK's default HTTP client. Tests inject
	// counting/synthetic transports here. Applies to every cluster
	// built by New / Open.
	HTTPClient *http.Client

	// SkipProbe disables the boot-time writability probe.
	SkipProbe bool

	// SkipCredsCheck disables the boot-time credentials resolution
	// validation done by New. Tests that don't want a real creds chain
	// flip this on.
	SkipCredsCheck bool
}

// Validate performs cross-field checks on the multi-cluster Config:
// every class's Cluster references a known cluster id; both maps are
// populated.
func (c Config) Validate() error {
	if len(c.Clusters) == 0 {
		return fmt.Errorf("s3: at least one cluster required")
	}
	if len(c.Classes) == 0 {
		return fmt.Errorf("s3: at least one storage class required")
	}
	for name, class := range c.Classes {
		if class.Cluster == "" {
			return fmt.Errorf("s3: class %q has empty cluster", name)
		}
		if class.Bucket == "" {
			return fmt.Errorf("s3: class %q has empty bucket", name)
		}
		if _, ok := c.Clusters[class.Cluster]; !ok {
			return fmt.Errorf("s3: class %q references unknown cluster %q", name, class.Cluster)
		}
	}
	return nil
}

// New builds a multi-cluster Backend from cfg.Clusters + cfg.Classes.
// Validates that every class's cluster ref resolves and every cluster's
// credentials resolve (fail-fast at boot — missing env var, missing
// file, or no SDK default chain → error from New, gateway refuses to
// start).
//
// SDK clients + Uploaders are NOT built here; that's lazy via connFor on
// first data-plane use, mirroring the rados backend.
func New(cfg Config) (*Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.SkipCredsCheck {
		for id, spec := range cfg.Clusters {
			if err := validateClusterCredentials(spec); err != nil {
				return nil, fmt.Errorf("s3: cluster %q credentials: %w", id, err)
			}
		}
	}
	RegisterMetrics()
	b := &Backend{
		clusters:   make(map[string]*s3Cluster, len(cfg.Clusters)),
		classes:    copyClasses(cfg.Classes),
		httpClient: cfg.HTTPClient,
	}
	for id, spec := range cfg.Clusters {
		spec.ID = id
		b.clusters[id] = &s3Cluster{
			spec:             spec,
			opTimeout:        opTimeoutFromSpec(spec),
			multipartTimeout: DefaultMultipartTimeout,
			sseMode:          sseModeFromSpec(spec),
			sseKMSKeyID:      spec.SSEKMSKeyID,
		}
	}
	return b, nil
}

// Open is the legacy single-cluster entry-point: builds a Backend wired
// to one S3 endpoint + one bucket from cfg's legacy fields. Internally
// synthesises a Clusters map under "default" and a Classes map under
// "STANDARD", then delegates to New.
//
// Retired by US-004 — production wiring moves to STRATA_S3_CLUSTERS +
// STRATA_S3_CLASSES + s3.New(cfg).
func Open(ctx context.Context, cfg Config) (*Backend, error) {
	if len(cfg.Clusters) > 0 || len(cfg.Classes) > 0 {
		b, err := New(cfg)
		if err != nil {
			return nil, err
		}
		if !cfg.SkipProbe {
			if err := b.probeEachCluster(ctx); err != nil {
				return nil, err
			}
		}
		return b, nil
	}

	// Legacy single-cluster shape — synthesise one cluster + one class.
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket required")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("s3: region required")
	}
	sseMode := cfg.SSEMode
	if sseMode == "" {
		sseMode = data.SSEModePassthrough
	}
	switch sseMode {
	case data.SSEModePassthrough, data.SSEModeStrata, data.SSEModeBoth:
	default:
		return nil, fmt.Errorf("s3: sse_mode must be one of {passthrough, strata, both}, got %q", sseMode)
	}

	clusterID := "default"
	className := "STANDARD"
	spec := S3ClusterSpec{
		ID:                clusterID,
		Endpoint:          cfg.Endpoint,
		Region:            cfg.Region,
		ForcePathStyle:    cfg.ForcePathStyle,
		PartSize:          cfg.PartSize,
		UploadConcurrency: int64(cfg.UploadConcurrency),
		MaxRetries:        int64(cfg.MaxRetries),
		OpTimeoutSecs:     int(cfg.OpTimeout / time.Second),
		SSEMode:           sseMode,
		SSEKMSKeyID:       cfg.SSEKMSKeyID,
		Credentials:       CredentialsRef{Type: CredentialsChain},
	}
	newCfg := Config{
		Clusters: map[string]S3ClusterSpec{clusterID: spec},
		Classes: map[string]ClassSpec{className: {
			Cluster: clusterID,
			Bucket:  cfg.Bucket,
		}},
		HTTPClient:     cfg.HTTPClient,
		SkipProbe:      true, // probed below per legacy semantics
		SkipCredsCheck: true,
	}
	b, err := New(newCfg)
	if err != nil {
		return nil, err
	}
	// Carry the legacy multipart timeout + static creds onto the
	// synthesised cluster — these don't fit S3ClusterSpec's JSON shape.
	c := b.clusters[clusterID]
	if cfg.MultipartTimeout > 0 {
		c.multipartTimeout = cfg.MultipartTimeout
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		c.legacyAccessKey = cfg.AccessKey
		c.legacySecretKey = cfg.SecretKey
	}

	if !cfg.SkipProbe {
		if err := b.probeEachCluster(ctx); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// probeEachCluster runs the per-cluster boot-time writability probe
// against every cluster's first associated bucket.
func (b *Backend) probeEachCluster(ctx context.Context) error {
	for clusterID := range b.clusters {
		bucket := ""
		for _, class := range b.classes {
			if class.Cluster == clusterID {
				bucket = class.Bucket
				break
			}
		}
		if bucket == "" {
			continue
		}
		c, err := b.connFor(ctx, clusterID)
		if err != nil {
			return err
		}
		if err := probeCluster(ctx, c, bucket); err != nil {
			return err
		}
	}
	return nil
}

// resolveClass routes a Strata storage-class label to its target
// (cluster, bucket) pair. Returns ErrUnknownStorageClass when the class
// is missing and ErrClassMissingCluster when the matched class has an
// empty Cluster field.
func (b *Backend) resolveClass(class string) (clusterID, bucket string, err error) {
	if class == "" {
		class = "STANDARD"
	}
	spec, ok := b.classes[class]
	if !ok {
		return "", "", fmt.Errorf("%w: %s", ErrUnknownStorageClass, class)
	}
	if spec.Cluster == "" {
		return "", "", fmt.Errorf("%w: %s", ErrClassMissingCluster, class)
	}
	return spec.Cluster, spec.Bucket, nil
}

// connFor lazy-builds the SDK client + Uploader for the named cluster
// on first use under b.mu, then caches the result. Mirrors
// rados.Backend.connFor semantics.
func (b *Backend) connFor(ctx context.Context, id string) (*s3Cluster, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.clusters[id]
	if !ok {
		return nil, fmt.Errorf("s3: unknown cluster %q", id)
	}
	if c.client != nil {
		return c, nil
	}
	awscfg, err := loadAWSConfig(ctx, c.spec, c.legacyAccessKey, c.legacySecretKey, b.httpClient)
	if err != nil {
		return nil, fmt.Errorf("s3: cluster %q: %w", id, err)
	}
	client := awss3.NewFromConfig(awscfg, func(o *awss3.Options) {
		if c.spec.Endpoint != "" {
			endpoint := c.spec.Endpoint
			o.BaseEndpoint = &endpoint
		}
		o.UsePathStyle = c.spec.ForcePathStyle
		o.ClientLogMode |= aws.LogRetries
		o.Logger = retryWarnLogger{inner: o.Logger}
		o.APIOptions = append(o.APIOptions, instrumentStack)
	})
	partSize := c.spec.PartSize
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	concurrency := int(c.spec.UploadConcurrency)
	if concurrency <= 0 {
		concurrency = DefaultUploadConcurrency
	}
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = partSize
		u.Concurrency = concurrency
	})
	c.client = client
	c.uploader = uploader
	return c, nil
}

// clusterForClass is the class-routed entry-point used by every data-
// plane method that knows the storage class on the request (PutChunks)
// or the manifest (GetChunks / Delete / Presign). Returns ErrUnsupported
// when the Backend is a stub (zero clusters) so the existing test
// surface keeps the no-op semantics it had under singleCluster.
func (b *Backend) clusterForClass(ctx context.Context, class string) (*s3Cluster, string, error) {
	if len(b.clusters) == 0 {
		return nil, "", errors.ErrUnsupported
	}
	clusterID, bucket, err := b.resolveClass(class)
	if err != nil {
		return nil, "", err
	}
	c, err := b.connFor(ctx, clusterID)
	if err != nil {
		return nil, "", err
	}
	return c, bucket, nil
}

// singleCluster picks one cluster registered on this Backend. Used by
// helper / test-surface methods (Put / Get / GetRange / DeleteObject /
// DeleteBatch / Probe) that don't know the request's storage class.
//
// In the legacy single-cluster Open() path there is always exactly one
// cluster + one class, so this picks deterministically. With multiple
// clusters the implementation picks the lexicographically lowest class
// — production data-plane code MUST use clusterForClass instead.
func (b *Backend) singleCluster(ctx context.Context) (*s3Cluster, string, error) {
	if len(b.clusters) == 0 {
		return nil, "", errors.ErrUnsupported
	}
	var className string
	for name := range b.classes {
		if className == "" || name < className {
			className = name
		}
	}
	if className == "" {
		return nil, "", errors.ErrUnsupported
	}
	clusterID, bucket, err := b.resolveClass(className)
	if err != nil {
		return nil, "", err
	}
	c, err := b.connFor(ctx, clusterID)
	if err != nil {
		return nil, "", err
	}
	return c, bucket, nil
}

// loadAWSConfig resolves the AWS SDK config for one cluster.
func loadAWSConfig(ctx context.Context, spec S3ClusterSpec, legacyAK, legacySK string, httpClient *http.Client) (aws.Config, error) {
	maxRetries := int(spec.MaxRetries)
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(spec.Region),
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
		awsconfig.WithRetryMaxAttempts(maxRetries),
	}
	if httpClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(httpClient))
	}
	switch spec.Credentials.Type {
	case CredentialsChain, "":
		if legacyAK != "" && legacySK != "" {
			loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(legacyAK, legacySK, ""),
			))
		}
	case CredentialsEnv:
		parts := strings.SplitN(spec.Credentials.Ref, ":", 2)
		if len(parts) != 2 {
			return aws.Config{}, fmt.Errorf("malformed env ref %q", spec.Credentials.Ref)
		}
		ak := os.Getenv(parts[0])
		sk := os.Getenv(parts[1])
		if ak == "" || sk == "" {
			return aws.Config{}, fmt.Errorf("env vars %q/%q empty", parts[0], parts[1])
		}
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(ak, sk, ""),
		))
	case CredentialsFile:
		path := spec.Credentials.Ref
		profile := "default"
		if idx := strings.LastIndex(path, ":"); idx > 0 {
			profile = path[idx+1:]
			path = path[:idx]
		}
		loadOpts = append(loadOpts,
			awsconfig.WithSharedCredentialsFiles([]string{path}),
			awsconfig.WithSharedConfigProfile(profile),
		)
	default:
		return aws.Config{}, fmt.Errorf("unknown credentials.type %q", spec.Credentials.Type)
	}
	awscfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load aws config: %w", err)
	}
	return awscfg, nil
}

// validateClusterCredentials runs at New time (fail-fast): verifies that
// the cluster's CredentialsRef can resolve. For env refs we check the
// variables are set; for file refs we Stat the file; for chain we defer
// to the SDK (which errors at first connect — IMDS calls are too costly
// at boot).
func validateClusterCredentials(spec S3ClusterSpec) error {
	switch spec.Credentials.Type {
	case CredentialsChain, "":
		return nil
	case CredentialsEnv:
		parts := strings.SplitN(spec.Credentials.Ref, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("malformed env ref %q", spec.Credentials.Ref)
		}
		if os.Getenv(parts[0]) == "" {
			return fmt.Errorf("env var %q not set", parts[0])
		}
		if os.Getenv(parts[1]) == "" {
			return fmt.Errorf("env var %q not set", parts[1])
		}
		return nil
	case CredentialsFile:
		path := spec.Credentials.Ref
		if idx := strings.LastIndex(path, ":"); idx > 0 {
			path = path[:idx]
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("credentials file %q: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("unknown credentials.type %q", spec.Credentials.Type)
	}
}

func opTimeoutFromSpec(spec S3ClusterSpec) time.Duration {
	if spec.OpTimeoutSecs > 0 {
		return time.Duration(spec.OpTimeoutSecs) * time.Second
	}
	return DefaultOpTimeout
}

func sseModeFromSpec(spec S3ClusterSpec) string {
	if spec.SSEMode == "" {
		return data.SSEModePassthrough
	}
	return spec.SSEMode
}

func copyClasses(src map[string]ClassSpec) map[string]ClassSpec {
	out := make(map[string]ClassSpec, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// probeCluster runs the boot-time writability check against one
// (cluster, bucket) pair.
func probeCluster(ctx context.Context, c *s3Cluster, bucket string) error {
	if c.client == nil {
		return errors.ErrUnsupported
	}
	key := ProbeKey
	putCtx, putCancel := opCtxFor(ctx, c.opTimeout)
	out, err := c.client.PutObject(putCtx, &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   bytes.NewReader(nil),
	})
	putCancel()
	if err != nil {
		return fmt.Errorf("s3: probe-put bucket=%s key=%s: %w", bucket, key, err)
	}
	del := &awss3.DeleteObjectInput{Bucket: &bucket, Key: &key}
	if out.VersionId != nil && *out.VersionId != "" {
		v := *out.VersionId
		del.VersionId = &v
	}
	delCtx, delCancel := opCtxFor(ctx, c.opTimeout)
	defer delCancel()
	if _, err := c.client.DeleteObject(delCtx, del); err != nil {
		return fmt.Errorf("s3: probe-delete bucket=%s key=%s: %w", bucket, key, err)
	}
	return nil
}

// Probe is the boot-time writability check on the singular cluster
// configured via the legacy Open() shape. Multi-cluster deployments use
// probeEachCluster internally; this surface is retained for external
// callers and integration tests that reach for it directly.
func (b *Backend) Probe(ctx context.Context) error {
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return err
	}
	return probeCluster(ctx, c, bucket)
}

// opCtxFor wraps the parent ctx with the cluster's op timeout.
func opCtxFor(parent context.Context, opTimeout time.Duration) (context.Context, context.CancelFunc) {
	if opTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, opTimeout)
}

func uploadCtxFor(parent context.Context, multipartTimeout time.Duration) (context.Context, context.CancelFunc) {
	if multipartTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, multipartTimeout)
}

// Compile-time assertion that *Backend satisfies data.Backend.
var _ data.Backend = (*Backend)(nil)

// PutResult is returned by Backend.Put. ETag is the backend object ETag
// with surrounding quotes stripped. VersionID carries the SDK response
// VersionId verbatim — three-state semantics per PRD US-002:
//
//	""           backend has no versioning OR versioning off
//	"null"       versioning Suspended
//	<other>      UUID-shaped version-id from versioning-enabled bucket
type PutResult struct {
	ETag      string
	VersionID string
	Size      int64
}

// Put streams r into the resolved cluster's bucket under key oid via
// the manager.Uploader — single-shot PutObject for small objects,
// multipart for large ones (transparently). US-003 will route Put per
// storage class; today it uses the single configured cluster.
func (b *Backend) Put(ctx context.Context, oid string, r io.Reader, size int64) (*PutResult, error) {
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return nil, err
	}
	key := oid
	in := &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   r,
	}
	c.applyPutSSE(in)
	upCtx, cancel := uploadCtxFor(ctx, c.multipartTimeout)
	defer cancel()
	out, err := c.uploader.Upload(upCtx, in)
	if err != nil {
		return nil, fmt.Errorf("s3: upload %s: %w", oid, err)
	}
	res := &PutResult{Size: size}
	if out.ETag != nil {
		res.ETag = strings.Trim(*out.ETag, `"`)
	}
	if out.VersionID != nil {
		res.VersionID = *out.VersionID
	}
	return res, nil
}

// applyPutSSE / applyMultipartSSE / backendSSEActive / manifestSSE are
// per-cluster helpers — the SSE mode + KMS key id live on s3Cluster,
// not on Backend.
func (c *s3Cluster) applyPutSSE(in *awss3.PutObjectInput) {
	if !c.backendSSEActive() {
		return
	}
	if c.sseKMSKeyID != "" {
		in.ServerSideEncryption = s3types.ServerSideEncryptionAwsKms
		k := c.sseKMSKeyID
		in.SSEKMSKeyId = &k
		return
	}
	in.ServerSideEncryption = s3types.ServerSideEncryptionAes256
}

func (c *s3Cluster) applyMultipartSSE(in *awss3.CreateMultipartUploadInput) {
	if !c.backendSSEActive() {
		return
	}
	if c.sseKMSKeyID != "" {
		in.ServerSideEncryption = s3types.ServerSideEncryptionAwsKms
		k := c.sseKMSKeyID
		in.SSEKMSKeyId = &k
		return
	}
	in.ServerSideEncryption = s3types.ServerSideEncryptionAes256
}

func (c *s3Cluster) backendSSEActive() bool {
	return c.sseMode == data.SSEModePassthrough || c.sseMode == data.SSEModeBoth
}

func (c *s3Cluster) manifestSSE() *data.SSEInfo {
	if c.sseMode == "" {
		return nil
	}
	info := &data.SSEInfo{Mode: c.sseMode}
	if c.backendSSEActive() {
		if c.sseKMSKeyID != "" {
			info.Algorithm = data.SSEAlgorithmKMS
			info.KMSKeyID = c.sseKMSKeyID
		} else {
			info.Algorithm = data.SSEAlgorithmAES256
		}
	}
	return info
}

// Get streams the full backend object body for oid. US-003 adds
// per-class routing; today singleCluster picks the only configured one.
func (b *Backend) Get(ctx context.Context, oid string) (io.ReadCloser, error) {
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return nil, err
	}
	key := oid
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	out, err := c.client.GetObject(opCtx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		cancel()
		return nil, mapGetError(oid, err)
	}
	return &cancelOnCloseReader{ReadCloser: out.Body, cancel: cancel}, nil
}

// GetRange streams [off, off+length) of the backend object body for oid.
func (b *Backend) GetRange(ctx context.Context, oid string, off, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return nil, fmt.Errorf("s3: range length must be positive, got %d", length)
	}
	if off < 0 {
		return nil, fmt.Errorf("s3: range offset must be non-negative, got %d", off)
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return nil, err
	}
	key := oid
	rangeHeader := fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	out, err := c.client.GetObject(opCtx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Range:  &rangeHeader,
	})
	if err != nil {
		cancel()
		return nil, mapGetError(oid, err)
	}
	return &cancelOnCloseReader{ReadCloser: out.Body, cancel: cancel}, nil
}

// retryWarnLogger wraps the SDK's aws.Logger so retry attempt lines
// surface in our slog pipeline at Warn — matches US-006 AC.
type retryWarnLogger struct {
	inner logging.Logger
}

func (l retryWarnLogger) Logf(c logging.Classification, format string, args ...any) {
	if strings.HasPrefix(format, "retrying request") {
		slog.Warn("s3 backend retry: " + fmt.Sprintf(format, args...))
		return
	}
	if l.inner != nil {
		l.inner.Logf(c, format, args...)
	}
}

// cancelOnCloseReader pairs an SDK response Body with the per-op
// context.CancelFunc so the caller's Close() releases the context.
type cancelOnCloseReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseReader) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func mapGetError(oid string, err error) error {
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return fmt.Errorf("s3: get %s: %w", oid, data.ErrNotFound)
	}
	return fmt.Errorf("s3: get %s: %w", oid, err)
}

// ObjectRef identifies a single backend object for DeleteBatch.
type ObjectRef struct {
	Key       string
	VersionID string
}

// DeleteFailure records a per-ref failure inside a DeleteBatch response.
type DeleteFailure struct {
	Ref ObjectRef
	Err error
}

// DeleteBatchLimit is the S3 protocol cap on objects per DeleteObjects
// request.
const DeleteBatchLimit = 1000

// DeleteObject removes a single backend object on the singular cluster
// configured via the legacy Open() shape. Idempotent — NoSuchKey is
// treated as success. Used by integration tests + admin tools that
// don't carry a storage class; production data-plane code goes through
// Delete(m) which routes via m.Class.
func (b *Backend) DeleteObject(ctx context.Context, oid, versionID string) error {
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return err
	}
	return deleteFromCluster(ctx, c, bucket, oid, versionID)
}

// DeleteBatch removes up to len(refs) backend objects via DeleteObjects.
// Empty refs is a no-op (nil, nil).
func (b *Backend) DeleteBatch(ctx context.Context, refs []ObjectRef) ([]DeleteFailure, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return nil, err
	}
	quiet := true
	var failures []DeleteFailure
	for start := 0; start < len(refs); start += DeleteBatchLimit {
		end := min(start+DeleteBatchLimit, len(refs))
		batch := refs[start:end]
		ids := make([]s3types.ObjectIdentifier, len(batch))
		for i, ref := range batch {
			key := ref.Key
			ids[i] = s3types.ObjectIdentifier{Key: &key}
			if ref.VersionID != "" {
				v := ref.VersionID
				ids[i].VersionId = &v
			}
		}
		opCtx, cancel := opCtxFor(ctx, c.opTimeout)
		out, err := c.client.DeleteObjects(opCtx, &awss3.DeleteObjectsInput{
			Bucket: &bucket,
			Delete: &s3types.Delete{Objects: ids, Quiet: &quiet},
		})
		cancel()
		if err != nil {
			return failures, fmt.Errorf("s3: delete batch [%d:%d]: %w", start, end, err)
		}
		for _, e := range out.Errors {
			ref := ObjectRef{}
			if e.Key != nil {
				ref.Key = *e.Key
			}
			if e.VersionId != nil {
				ref.VersionID = *e.VersionId
			}
			code := ""
			msg := ""
			if e.Code != nil {
				code = *e.Code
			}
			if e.Message != nil {
				msg = *e.Message
			}
			if code == "NoSuchKey" {
				continue
			}
			failures = append(failures, DeleteFailure{
				Ref: ref,
				Err: fmt.Errorf("s3: delete %s (version %q): %s: %s", ref.Key, ref.VersionID, code, msg),
			})
		}
	}
	return failures, nil
}

// PutChunks streams r straight into the resolved (cluster, bucket) pair
// as ONE object. Routing is per storage class via clusterForClass —
// each class maps to a (cluster, bucket) pair in the multi-cluster
// config, so two classes can share a cluster but live in different
// buckets.
func (b *Backend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	if class == "" {
		class = "STANDARD"
	}
	c, bucket, err := b.clusterForClass(ctx, class)
	if err != nil {
		return nil, err
	}
	key := b.objectKey(ctx)

	cr := &countingReader{r: r}
	in := &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   cr,
	}
	c.applyPutSSE(in)
	upCtx, cancel := uploadCtxFor(ctx, c.multipartTimeout)
	defer cancel()
	out, err := c.uploader.Upload(upCtx, in)
	if err != nil {
		return nil, fmt.Errorf("s3: upload %s: %w", key, err)
	}
	etag := ""
	if out.ETag != nil {
		etag = strings.Trim(*out.ETag, `"`)
	}
	versionID := ""
	if out.VersionID != nil {
		versionID = *out.VersionID
	}

	m := &data.Manifest{
		Class:     class,
		Size:      cr.n,
		ChunkSize: data.DefaultChunkSize,
		ETag:      etag,
		BackendRef: &data.BackendRef{
			Backend:   BackendName,
			Key:       key,
			ETag:      etag,
			Size:      cr.n,
			VersionID: versionID,
		},
		SSE: c.manifestSSE(),
	}
	return m, nil
}

// GetChunks streams [offset, offset+length) of the manifest's backend
// object. The s3 backend serves only BackendRef-shape manifests.
func (b *Backend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	if m == nil || m.BackendRef == nil {
		return nil, errors.ErrUnsupported
	}
	if offset < 0 || offset > m.Size {
		return nil, fmt.Errorf("s3: offset %d out of range (size %d)", offset, m.Size)
	}
	if length <= 0 || offset+length > m.Size {
		length = m.Size - offset
	}
	if length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return b.GetRange(ctx, m.BackendRef.Key, offset, length)
}

// Delete removes the manifest's backend object via DeleteObject on the
// cluster + bucket resolved from m.Class. Idempotent — NoSuchKey is
// success. Manifests without BackendRef (legacy/rados-shape) are no-ops.
func (b *Backend) Delete(ctx context.Context, m *data.Manifest) error {
	if m == nil || m.BackendRef == nil {
		if len(b.clusters) == 0 {
			return errors.ErrUnsupported
		}
		return nil
	}
	c, bucket, err := b.clusterForClass(ctx, m.Class)
	if err != nil {
		return err
	}
	return deleteFromCluster(ctx, c, bucket, m.BackendRef.Key, m.BackendRef.VersionID)
}

// deleteFromCluster issues a single DeleteObject against the supplied
// (cluster, bucket) pair. Idempotent — NoSuchKey is success. Used by
// both class-routed Delete(m) and the legacy single-cluster
// DeleteObject helper.
func deleteFromCluster(ctx context.Context, c *s3Cluster, bucket, oid, versionID string) error {
	key := oid
	in := &awss3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	if versionID != "" {
		v := versionID
		in.VersionId = &v
	}
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	if _, err := c.client.DeleteObject(opCtx, in); err != nil {
		var noSuchKey *s3types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil
		}
		return fmt.Errorf("s3: delete %s: %w", oid, err)
	}
	return nil
}

func (b *Backend) Close() error { return nil }

// objectKey builds the backend object key. Format <bucket-uuid>/<object-
// uuid> — UUID-shaped prefix gives random distribution for AWS-side
// automatic prefix partitioning.
func (b *Backend) objectKey(ctx context.Context) string {
	objectID := uuid.NewString()
	if bucketID, ok := data.BucketIDFromContext(ctx); ok {
		return bucketID.String() + "/" + objectID
	}
	return uuid.NewString() + "/" + objectID
}

// countingReader wraps io.Reader to tally bytes seen by PutChunks.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
