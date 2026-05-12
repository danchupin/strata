// Aggregated typed wrappers around /admin/v1/*. Pages should import from this
// module (rather than the per-resource files) so the surface is one place to
// audit. Wrappers throw on non-2xx so TanStack Query can surface them via
// `error` + the global error toast (see web/src/components/query-error-toast.tsx).

export {
  fetchClusterStatus,
  fetchClusterNodes,
  type ClusterStatus,
  type ClusterNode,
  type ClusterNodesResponse,
} from './cluster';

export {
  fetchTopBuckets,
  fetchTopConsumers,
  type BucketTop,
  type BucketsTopBy,
  type BucketsTopResponse,
  type ConsumerTop,
  type ConsumersTopBy,
  type ConsumersTopResponse,
} from './widgets';

export {
  fetchStorageMeta,
  fetchStorageData,
  fetchStorageClasses,
  fetchStorageHealth,
  type NodeStatus,
  type MetaHealthReport,
  type PoolStatus,
  type DataHealthReport,
  type StorageClassEntry,
  type StorageClassesResponse,
  type StorageHealthResponse,
} from './storage';

export {
  fetchClusters,
  drainCluster,
  undrainCluster,
  type ClusterStateEntry,
  type ClustersListResponse,
} from './clusters';

export {
  login,
  logout,
  whoami,
  AuthError,
  type SessionInfo,
  type LoginRequest,
} from './auth';

// Placeholder wrappers for endpoints that land in later stories. Importing the
// names here lets components reference them today without per-page file churn
// when US-009/US-010/US-011 wire the real fetchers.

export interface BucketSummary {
  name: string;
  owner: string;
  region: string;
  created_at: number;
  size_bytes: number;
  object_count: number;
}

export interface BucketsListResponse {
  buckets: BucketSummary[];
  total: number;
}

export async function fetchBucketsList(params: {
  query?: string;
  sort?: string;
  order?: 'asc' | 'desc';
  page?: number;
  pageSize?: number;
}): Promise<BucketsListResponse> {
  const usp = new URLSearchParams();
  if (params.query) usp.set('query', params.query);
  if (params.sort) usp.set('sort', params.sort);
  if (params.order) usp.set('order', params.order);
  if (params.page != null) usp.set('page', String(params.page));
  if (params.pageSize != null) usp.set('page_size', String(params.pageSize));
  const qs = usp.toString();
  const resp = await fetch(`/admin/v1/buckets${qs ? `?${qs}` : ''}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw new Error(`buckets: ${resp.status} ${resp.statusText}`);
  const body = (await resp.json()) as BucketsListResponse;
  return { buckets: body.buckets ?? [], total: body.total ?? 0 };
}

export type BucketVersioning = 'Enabled' | 'Suspended' | 'Off';

export interface BucketDetail {
  name: string;
  owner: string;
  region: string;
  created_at: number;
  versioning: BucketVersioning;
  object_lock: boolean;
  size_bytes: number;
  object_count: number;
  backend_presign: boolean;
  // shard_count is the active sharding factor for the bucket's `objects`
  // table partition. The Hot Shards drill panel reproduces shard via
  // FNV-1a(key) % shard_count to match the Go shardOf helper.
  shard_count: number;
  // replication_configured is true when the bucket has a non-empty
  // replication configuration (set via PutBucketReplication on the S3
  // surface). Gates the per-bucket Replication tab (US-014) — only buckets
  // with a configuration get the tab.
  replication_configured?: boolean;
}

export interface CreateBucketBody {
  name: string;
  region?: string;
  versioning?: 'Enabled' | 'Suspended';
  object_lock_enabled?: boolean;
}

export interface CreateBucketError extends Error {
  code: string;
  status: number;
}

// createBucket calls POST /admin/v1/buckets (US-001). Throws a CreateBucketError
// carrying the {code, status} pair the dialog renders inline so the operator
// gets a server-validated error message rather than a generic "Failed to fetch".
export async function createBucket(body: CreateBucketBody): Promise<BucketDetail> {
  const resp = await fetch('/admin/v1/buckets', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    let code = `HTTP${resp.status}`;
    let message = resp.statusText || 'request failed';
    try {
      const j = (await resp.json()) as { code?: string; message?: string };
      if (j.code) code = j.code;
      if (j.message) message = j.message;
    } catch {
      // body wasn't JSON — keep statusText
    }
    const err = new Error(message) as CreateBucketError;
    err.code = code;
    err.status = resp.status;
    throw err;
  }
  return (await resp.json()) as BucketDetail;
}

// setBucketVersioning calls PUT /admin/v1/buckets/{name}/versioning (US-003).
// Throws AdminApiError on 4xx/5xx so the form can render code+message inline.
export async function setBucketVersioning(
  name: string,
  state: 'Enabled' | 'Suspended',
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/versioning`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ state }),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set versioning failed');
}

// setBucketBackendPresign calls PUT /admin/v1/buckets/{name}/backend-presign
// (US-020). Flips the per-bucket s3-over-s3 presign-passthrough flag.
export async function setBucketBackendPresign(
  name: string,
  enabled: boolean,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/backend-presign`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled }),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set backend-presign failed');
}

export type ObjectLockMode = 'GOVERNANCE' | 'COMPLIANCE';

export interface ObjectLockDefaultRetention {
  mode?: ObjectLockMode;
  days?: number;
  years?: number;
}

export interface ObjectLockRule {
  default_retention?: ObjectLockDefaultRetention;
}

export interface ObjectLockConfig {
  object_lock_enabled?: string;
  rule?: ObjectLockRule;
}

// fetchBucketObjectLock fetches the bucket's ObjectLockConfiguration (US-003).
// Returns the AWS-shape JSON. 404 if the bucket is missing.
export async function fetchBucketObjectLock(name: string): Promise<ObjectLockConfig> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/object-lock`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch object-lock failed');
  return (await resp.json()) as ObjectLockConfig;
}

export async function setBucketObjectLock(
  name: string,
  cfg: ObjectLockConfig,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/object-lock`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set object-lock failed');
}

// Lifecycle (US-004) — JSON wire shape. Mirrors LifecycleConfigJSON in
// internal/adminapi/buckets_lifecycle.go. The visual editor binds to this
// shape directly; the JSON tab paste-path round-trips through it too.
export interface LifecycleTag {
  key: string;
  value: string;
}

export interface LifecycleFilter {
  prefix?: string;
  tags?: LifecycleTag[];
}

export interface LifecycleExpiration {
  days?: number;
  date?: string;
  expired_object_delete_marker?: boolean;
}

export interface LifecycleTransition {
  days?: number;
  date?: string;
  storage_class: string;
}

export interface NoncurrentExpiration {
  noncurrent_days: number;
}

export interface NoncurrentTransition {
  noncurrent_days: number;
  storage_class: string;
}

export interface AbortIncompleteMultipart {
  days_after_initiation: number;
}

export interface LifecycleRule {
  id: string;
  status: 'Enabled' | 'Disabled';
  prefix?: string;
  filter?: LifecycleFilter;
  expiration?: LifecycleExpiration;
  transitions?: LifecycleTransition[];
  noncurrent_version_expiration?: NoncurrentExpiration;
  noncurrent_version_transitions?: NoncurrentTransition[];
  abort_incomplete_multipart_upload?: AbortIncompleteMultipart;
}

export interface LifecycleConfig {
  rules: LifecycleRule[];
}

// fetchBucketLifecycle returns the bucket's LifecycleConfig (US-004).
// Returns null on 404 NoSuchLifecycleConfiguration so the editor can render
// the empty-state without forcing the caller to catch the error.
export async function fetchBucketLifecycle(name: string): Promise<LifecycleConfig | null> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/lifecycle`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (resp.status === 404) return null;
  if (!resp.ok) throw await buildAdminError(resp, 'fetch lifecycle failed');
  return (await resp.json()) as LifecycleConfig;
}

export async function setBucketLifecycle(
  name: string,
  cfg: LifecycleConfig,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/lifecycle`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set lifecycle failed');
}

// CORS (US-005) — JSON wire shape mirrors CORSConfigJSON in
// internal/adminapi/buckets_cors.go. The visual editor binds to this shape;
// the JSON tab paste-path round-trips through it too.
export interface CORSRule {
  id?: string;
  allowed_methods: string[];
  allowed_origins: string[];
  allowed_headers?: string[];
  expose_headers?: string[];
  max_age_seconds?: number;
}

export interface CORSConfig {
  rules: CORSRule[];
}

// fetchBucketCORS returns the bucket's CORSConfig (US-005). Returns null on
// 404 NoSuchCORSConfiguration so the editor can render the empty-state
// without forcing the caller to catch the error.
export async function fetchBucketCORS(name: string): Promise<CORSConfig | null> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/cors`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (resp.status === 404) return null;
  if (!resp.ok) throw await buildAdminError(resp, 'fetch cors failed');
  return (await resp.json()) as CORSConfig;
}

export async function setBucketCORS(name: string, cfg: CORSConfig): Promise<void> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/cors`, {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(cfg),
  });
  if (!resp.ok) throw await buildAdminError(resp, 'set cors failed');
}

export async function deleteBucketCORS(name: string): Promise<void> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/cors`, {
    method: 'DELETE',
    credentials: 'same-origin',
  });
  if (resp.status === 204) return;
  if (resp.status === 404) return;
  throw await buildAdminError(resp, 'delete cors failed');
}

// Bucket Policy (US-006) — the wire shape is the raw IAM policy JSON document
// (Version, Statement[Effect, Action, Resource, Principal, Condition]). The
// admin API persists what the operator types verbatim (after canonical
// re-indenting); the GET response Content-Type is application/json so we
// return the parsed JSON value untouched and let the editor format it.

// fetchBucketPolicyText returns the stored policy as a JSON string. Returns
// null on 404 NoSuchBucketPolicy so the editor can render an empty state.
export async function fetchBucketPolicyText(name: string): Promise<string | null> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/policy`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (resp.status === 404) return null;
  if (!resp.ok) throw await buildAdminError(resp, 'fetch policy failed');
  return await resp.text();
}

export async function setBucketPolicy(name: string, policy: string): Promise<void> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/policy`, {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: policy,
  });
  if (!resp.ok) throw await buildAdminError(resp, 'set policy failed');
}

export async function deleteBucketPolicy(name: string): Promise<void> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/policy`, {
    method: 'DELETE',
    credentials: 'same-origin',
  });
  if (resp.status === 204) return;
  if (resp.status === 404) return;
  throw await buildAdminError(resp, 'delete policy failed');
}

export interface PolicyDryRunResult {
  valid: boolean;
  message?: string;
}

// dryRunBucketPolicy validates the policy server-side without persisting.
// Server returns 200 {valid:true} on accept, 400 {valid:false, message} on
// parse error — both deserialise to PolicyDryRunResult so callers can
// branch on .valid.
export async function dryRunBucketPolicy(
  name: string,
  policy: string,
): Promise<PolicyDryRunResult> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/policy/dry-run`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: policy,
    },
  );
  if (resp.status === 200 || resp.status === 400) {
    return (await resp.json()) as PolicyDryRunResult;
  }
  throw await buildAdminError(resp, 'dry-run policy failed');
}

// ACL types (US-007). canned is one of:
//   private | public-read | public-read-write | authenticated-read | log-delivery-write
// grants is the explicit Grant list (independent of canned).
export type ACLCanned =
  | 'private'
  | 'public-read'
  | 'public-read-write'
  | 'authenticated-read'
  | 'log-delivery-write';

export type ACLGranteeType = 'CanonicalUser' | 'Group' | 'AmazonCustomerByEmail';
export type ACLPermission =
  | 'FULL_CONTROL'
  | 'READ'
  | 'WRITE'
  | 'READ_ACP'
  | 'WRITE_ACP';

export interface ACLGrant {
  grantee_type: ACLGranteeType;
  id?: string;
  uri?: string;
  display_name?: string;
  email?: string;
  permission: ACLPermission;
}

export interface ACLConfig {
  canned: ACLCanned;
  grants: ACLGrant[];
}

export async function fetchBucketACL(name: string): Promise<ACLConfig> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/acl`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'fetch acl failed');
  return (await resp.json()) as ACLConfig;
}

export async function setBucketACL(name: string, body: ACLConfig): Promise<void> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}/acl`, {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw await buildAdminError(resp, 'set acl failed');
}

// Inventory (US-008) — JSON wire shape mirrors InventoryConfigJSON in
// internal/adminapi/buckets_inventory.go. The admin layer translates JSON↔XML
// so the s3api consumer keeps reading the AWS XML shape unchanged.
export type InventoryFormat = 'CSV' | 'ORC' | 'Parquet';
export type InventoryFrequency = 'Daily' | 'Hourly' | 'Weekly';
export type InventoryVersions = 'All' | 'Current';

export interface InventoryDestination {
  bucket: string;
  format: InventoryFormat;
  prefix?: string;
  account_id?: string;
}

export interface InventorySchedule {
  frequency: InventoryFrequency;
}

export interface InventoryFilter {
  prefix?: string;
}

export interface InventoryConfig {
  id: string;
  is_enabled: boolean;
  destination: InventoryDestination;
  schedule: InventorySchedule;
  included_object_versions: InventoryVersions;
  filter?: InventoryFilter;
  optional_fields?: string[];
}

export interface InventoryConfigsList {
  configurations: InventoryConfig[];
}

export async function listBucketInventory(name: string): Promise<InventoryConfigsList> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/inventory`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'list inventory failed');
  const body = (await resp.json()) as InventoryConfigsList;
  return { configurations: body.configurations ?? [] };
}

export async function setBucketInventory(
  name: string,
  configID: string,
  cfg: InventoryConfig,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/inventory/${encodeURIComponent(configID)}`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set inventory failed');
}

export async function deleteBucketInventory(
  name: string,
  configID: string,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/inventory/${encodeURIComponent(configID)}`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete inventory failed');
}

// Logging (US-009) — JSON wire shape mirrors LoggingConfigJSON in
// internal/adminapi/buckets_logging.go. The admin layer translates JSON↔XML
// so the s3api consumer / access-log worker keep reading the AWS XML shape
// unchanged.
export type LoggingPermission = 'FULL_CONTROL' | 'READ' | 'WRITE';

export interface LoggingGrant {
  grantee_type: ACLGranteeType;
  id?: string;
  uri?: string;
  display_name?: string;
  email?: string;
  permission: LoggingPermission;
}

export interface LoggingConfig {
  target_bucket: string;
  target_prefix: string;
  target_grants?: LoggingGrant[];
}

// fetchBucketLogging returns the bucket's logging configuration. Returns null
// on 404 NoSuchBucketLoggingConfiguration so the editor can render the empty
// state without forcing the caller to catch the error.
export async function fetchBucketLogging(name: string): Promise<LoggingConfig | null> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/logging`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (resp.status === 404) return null;
  if (!resp.ok) throw await buildAdminError(resp, 'fetch logging failed');
  return (await resp.json()) as LoggingConfig;
}

export async function setBucketLogging(
  name: string,
  cfg: LoggingConfig,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/logging`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set logging failed');
}

export async function deleteBucketLogging(name: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/logging`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete logging failed');
}

export async function fetchBucket(name: string): Promise<BucketDetail> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw new Error(`buckets/${name}: ${resp.status} ${resp.statusText}`);
  return (await resp.json()) as BucketDetail;
}

// AdminApiError is the shared error shape thrown by admin API wrappers when
// the server returns a structured {code, message} JSON body. Callers render
// the code+message inline in dialogs/banners.
export interface AdminApiError extends Error {
  code: string;
  status: number;
}

async function buildAdminError(resp: Response, fallback: string): Promise<AdminApiError> {
  let code = `HTTP${resp.status}`;
  let message = resp.statusText || fallback;
  try {
    const j = (await resp.json()) as { code?: string; message?: string };
    if (j.code) code = j.code;
    if (j.message) message = j.message;
  } catch {
    // body wasn't JSON — keep statusText
  }
  const err = new Error(message) as AdminApiError;
  err.code = code;
  err.status = resp.status;
  return err;
}

// deleteBucket calls DELETE /admin/v1/buckets/{name} (US-002). Resolves to
// void on 204; throws an AdminApiError on 4xx/5xx so the dialog can render
// the {code, message} pair (e.g. BucketNotEmpty/NoSuchBucket).
export async function deleteBucket(name: string): Promise<void> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
  });
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete bucket failed');
}

export type ForceEmptyJobState = 'pending' | 'running' | 'done' | 'error';

export interface ForceEmptyJob {
  job_id: string;
  bucket: string;
  state: ForceEmptyJobState;
  deleted: number;
  message?: string;
  started_at: number;
  updated_at: number;
  finished_at?: number;
}

// startForceEmpty kicks the per-bucket force-empty drain. Returns the
// initial job row (state="pending"). Throws AdminApiError on 4xx/5xx —
// e.g. ForceEmptyInProgress when another job is already running.
export async function startForceEmpty(bucket: string): Promise<ForceEmptyJob> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/force-empty`,
    { method: 'POST', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'force-empty start failed');
  return (await resp.json()) as ForceEmptyJob;
}

// fetchForceEmptyJob polls the job status. Throws on 4xx/5xx so the
// caller can stop the polling loop on JobNotFound or transient failure.
export async function fetchForceEmptyJob(
  bucket: string,
  jobID: string,
): Promise<ForceEmptyJob> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/force-empty/${encodeURIComponent(jobID)}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'force-empty status failed');
  return (await resp.json()) as ForceEmptyJob;
}

export interface ObjectEntry {
  key: string;
  size: number;
  last_modified: number;
  etag: string;
  storage_class: string;
}

export interface ObjectsResponse {
  objects: ObjectEntry[];
  common_prefixes: string[];
  next_marker?: string;
  is_truncated: boolean;
}

export async function fetchObjects(
  bucket: string,
  params: { prefix?: string; marker?: string; delimiter?: string; pageSize?: number },
): Promise<ObjectsResponse> {
  const usp = new URLSearchParams();
  if (params.prefix) usp.set('prefix', params.prefix);
  if (params.marker) usp.set('marker', params.marker);
  if (params.delimiter) usp.set('delimiter', params.delimiter);
  if (params.pageSize != null) usp.set('page_size', String(params.pageSize));
  const qs = usp.toString();
  const url = `/admin/v1/buckets/${encodeURIComponent(bucket)}/objects${qs ? `?${qs}` : ''}`;
  const resp = await fetch(url, { method: 'GET', credentials: 'same-origin' });
  if (!resp.ok) throw new Error(`objects: ${resp.status} ${resp.statusText}`);
  const body = (await resp.json()) as ObjectsResponse;
  return {
    objects: body.objects ?? [],
    common_prefixes: body.common_prefixes ?? [],
    next_marker: body.next_marker,
    is_truncated: Boolean(body.is_truncated),
  };
}

export interface MetricsPoint {
  0: number; // epoch-ms
  1: number; // value
}

export interface MetricsSeries {
  name: string;
  points: Array<[number, number]>;
}

export interface MetricsTimeseriesResponse {
  series: MetricsSeries[];
  metrics_available?: boolean;
}

export async function fetchMetricsTimeseries(params: {
  metric: string;
  range: string;
  step?: string;
}): Promise<MetricsTimeseriesResponse> {
  const usp = new URLSearchParams();
  usp.set('metric', params.metric);
  usp.set('range', params.range);
  if (params.step) usp.set('step', params.step);
  const resp = await fetch(`/admin/v1/metrics/timeseries?${usp.toString()}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw new Error(`metrics/timeseries: ${resp.status} ${resp.statusText}`);
  const body = (await resp.json()) as MetricsTimeseriesResponse;
  return { series: body.series ?? [], metrics_available: body.metrics_available };
}

// IAM Users (US-011) — paginated list + create + cascading delete. Wire shape
// mirrors internal/adminapi/iam_users.go.
export interface IAMUserSummary {
  user_name: string;
  user_id: string;
  path: string;
  created_at: number;
  access_key_count: number;
}

export interface IAMUsersListResponse {
  users: IAMUserSummary[];
  total: number;
}

export async function fetchIAMUsers(params: {
  query?: string;
  page?: number;
  pageSize?: number;
}): Promise<IAMUsersListResponse> {
  const usp = new URLSearchParams();
  if (params.query) usp.set('query', params.query);
  if (params.page != null) usp.set('page', String(params.page));
  if (params.pageSize != null) usp.set('page_size', String(params.pageSize));
  const qs = usp.toString();
  const resp = await fetch(`/admin/v1/iam/users${qs ? `?${qs}` : ''}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'fetch iam users failed');
  const body = (await resp.json()) as IAMUsersListResponse;
  return { users: body.users ?? [], total: body.total ?? 0 };
}

export interface CreateIAMUserBody {
  user_name: string;
  path?: string;
}

export async function createIAMUser(body: CreateIAMUserBody): Promise<IAMUserSummary> {
  const resp = await fetch('/admin/v1/iam/users', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw await buildAdminError(resp, 'create iam user failed');
  return (await resp.json()) as IAMUserSummary;
}

export async function deleteIAMUser(userName: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete iam user failed');
}

// IAM Access Keys (US-012). Per-user list + secret-once create + flip
// disabled + delete. The SecretAccessKey only ever appears in the
// IAMAccessKeyCreateResponse — every other shape strips it.
export interface IAMAccessKeySummary {
  access_key_id: string;
  user_name: string;
  created_at: number;
  disabled: boolean;
}

export interface IAMAccessKeyListResponse {
  access_keys: IAMAccessKeySummary[];
}

export interface IAMAccessKeyCreateResponse {
  access_key_id: string;
  secret_access_key: string;
  user_name: string;
  created_at: number;
  disabled: boolean;
}

export async function fetchIAMAccessKeys(userName: string): Promise<IAMAccessKeySummary[]> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/access-keys`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch access keys failed');
  const body = (await resp.json()) as IAMAccessKeyListResponse;
  return body.access_keys ?? [];
}

export async function createIAMAccessKey(userName: string): Promise<IAMAccessKeyCreateResponse> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/access-keys`,
    { method: 'POST', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'create access key failed');
  return (await resp.json()) as IAMAccessKeyCreateResponse;
}

export async function updateIAMAccessKeyDisabled(
  accessKeyID: string,
  disabled: boolean,
): Promise<IAMAccessKeySummary> {
  const resp = await fetch(
    `/admin/v1/iam/access-keys/${encodeURIComponent(accessKeyID)}`,
    {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ disabled }),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'update access key failed');
  return (await resp.json()) as IAMAccessKeySummary;
}

export async function deleteIAMAccessKey(accessKeyID: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/iam/access-keys/${encodeURIComponent(accessKeyID)}`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete access key failed');
}

// Managed policies (US-013). The admin layer mints ARNs under the
// `arn:aws:iam::strata:policy<path><name>` namespace; PUT/DELETE accept the
// full ARN as a trailing-wildcard URL segment so slashes inside the ARN do
// not break Go's mux pattern matching.
export interface ManagedPolicySummary {
  arn: string;
  name: string;
  path: string;
  description?: string;
  document: string;
  created_at: number;
  updated_at: number;
  attachment_count: number;
}

export interface ManagedPoliciesListResponse {
  policies: ManagedPolicySummary[];
}

export interface CreateManagedPolicyBody {
  name: string;
  path?: string;
  description?: string;
  document: string;
}

// PolicyAttachedError carries the attached_to user list returned by a 409
// from DELETE /admin/v1/iam/policies/{arn} — surfaced inline in the delete
// dialog so the operator can detach those users (US-014) before retrying.
export interface PolicyAttachedError extends AdminApiError {
  code: 'PolicyAttached';
  attachedTo: string[];
}

export async function fetchManagedPolicies(): Promise<ManagedPolicySummary[]> {
  const resp = await fetch('/admin/v1/iam/policies', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'fetch managed policies failed');
  const body = (await resp.json()) as ManagedPoliciesListResponse;
  return body.policies ?? [];
}

export async function createManagedPolicy(
  body: CreateManagedPolicyBody,
): Promise<ManagedPolicySummary> {
  const resp = await fetch('/admin/v1/iam/policies', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw await buildAdminError(resp, 'create managed policy failed');
  return (await resp.json()) as ManagedPolicySummary;
}

export async function updateManagedPolicyDocument(
  arn: string,
  document: string,
): Promise<ManagedPolicySummary> {
  const resp = await fetch(`/admin/v1/iam/policies/${arn}`, {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ document }),
  });
  if (!resp.ok) throw await buildAdminError(resp, 'update managed policy failed');
  return (await resp.json()) as ManagedPolicySummary;
}

export async function deleteManagedPolicy(arn: string): Promise<void> {
  const resp = await fetch(`/admin/v1/iam/policies/${arn}`, {
    method: 'DELETE',
    credentials: 'same-origin',
  });
  if (resp.status === 204) return;
  if (resp.status === 409) {
    let attachedTo: string[] = [];
    let message = 'managed policy is attached to one or more users';
    try {
      const j = (await resp.json()) as { message?: string; attached_to?: string[] };
      if (j.message) message = j.message;
      if (Array.isArray(j.attached_to)) attachedTo = j.attached_to;
    } catch {
      // body wasn't JSON — keep defaults
    }
    const err = new Error(message) as PolicyAttachedError;
    err.code = 'PolicyAttached';
    err.status = 409;
    err.attachedTo = attachedTo;
    throw err;
  }
  throw await buildAdminError(resp, 'delete managed policy failed');
}

// User-policy attachments (US-014). The list endpoint enriches each ARN with
// the policy name+path via a server-side GetManagedPolicy lookup so the table
// can render an operator-friendly row without re-fetching every policy.
export interface UserPolicyAttachment {
  arn: string;
  name: string;
  path: string;
}

export interface UserPoliciesListResponse {
  policies: UserPolicyAttachment[];
}

export async function fetchIAMUserPolicies(userName: string): Promise<UserPolicyAttachment[]> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/policies`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch user policies failed');
  const body = (await resp.json()) as UserPoliciesListResponse;
  return body.policies ?? [];
}

export async function attachUserPolicy(userName: string, policyArn: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/policies`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ policy_arn: policyArn }),
    },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'attach policy failed');
}

export async function detachUserPolicy(userName: string, policyArn: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/policies/${policyArn}`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'detach policy failed');
}

export async function fetchIAMUser(userName: string): Promise<IAMUserSummary> {
  // The admin API has no per-user GET endpoint — emulate via the list endpoint
  // with a query filter so the user-detail page can render cheap metadata
  // without a dedicated /admin/v1/iam/users/{name} round-trip.
  const list = await fetchIAMUsers({ query: userName, page: 1, pageSize: 50 });
  const exact = list.users.find((u) => u.user_name === userName);
  if (!exact) {
    const err = new Error(`user ${userName} not found`) as AdminApiError;
    err.code = 'NoSuchEntity';
    err.status = 404;
    throw err;
  }
  return exact;
}

// US-015: Object upload wizard via per-part presigned URLs.
//
// The browser:
//   1. POSTs an Initiate -> gets {upload_id, part_size}
//   2. Slices the file into part_size chunks; per chunk asks the admin API
//      to mint a 5-minute presigned PUT URL, then PUTs the bytes directly
//      to the gateway with progress reported via XHR upload events.
//   3. POSTs Complete with the part list -> admin forwards to s3api which
//      composes the final ETag and persists the object row.
//
// For files <=5 MiB the single-PUT path is preferred (one presigned URL,
// one PUT, no multipart bookkeeping).

export interface UploadInitBody {
  key: string;
  storage_class?: string;
  content_type?: string;
  cache_control?: string;
  content_disposition?: string;
  user_meta?: Record<string, string>;
  tags?: Record<string, string>;
}

export interface UploadInitResponse {
  upload_id: string;
  key: string;
  bucket: string;
  part_size: number;
}

export async function initiateUpload(
  bucket: string,
  body: UploadInitBody,
): Promise<UploadInitResponse> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/uploads`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'initiate upload failed');
  return (await resp.json()) as UploadInitResponse;
}

export interface UploadPartPresignResponse {
  url: string;
  expires_at: number;
  part_number: number;
}

export async function presignUploadPart(
  bucket: string,
  uploadID: string,
  partNumber: number,
): Promise<UploadPartPresignResponse> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/uploads/${encodeURIComponent(uploadID)}/parts/${partNumber}/presign`,
    {
      method: 'POST',
      credentials: 'same-origin',
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'presign part failed');
  return (await resp.json()) as UploadPartPresignResponse;
}

export interface UploadCompletePart {
  part_number: number;
  etag: string;
}

export async function completeUpload(
  bucket: string,
  uploadID: string,
  parts: UploadCompletePart[],
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/uploads/${encodeURIComponent(uploadID)}/complete`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ parts }),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'complete upload failed');
}

export async function abortUpload(bucket: string, uploadID: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/uploads/${encodeURIComponent(uploadID)}`,
    {
      method: 'DELETE',
      credentials: 'same-origin',
    },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'abort upload failed');
}

export interface SinglePresignResponse {
  url: string;
  expires_at: number;
  storage_class?: string;
}

export async function presignSingleGet(
  bucket: string,
  key: string,
  versionId?: string,
): Promise<SinglePresignResponse> {
  const body: Record<string, string> = { key };
  if (versionId) body.version_id = versionId;
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/single-get-presign`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'single-GET presign failed');
  return (await resp.json()) as SinglePresignResponse;
}

export async function presignSinglePut(
  bucket: string,
  key: string,
  storageClass?: string,
): Promise<SinglePresignResponse> {
  const body: Record<string, string> = { key };
  if (storageClass && storageClass !== 'STANDARD') {
    body.storage_class = storageClass;
  }
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/single-presign`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'single-PUT presign failed');
  return (await resp.json()) as SinglePresignResponse;
}

// US-016 — Object detail / tags / retention / legal-hold / versions / delete.
// AC names PUT /objects/{key}/tags etc but Go 1.22 mux trailing wildcards
// must be the LAST segment, so the per-shape endpoints carry the key in
// the JSON body. The DELETE shape keeps {key...} (no subroute follows) so
// the AC URL works there.

export interface ObjectDetail {
  key: string;
  version_id?: string;
  is_latest: boolean;
  is_delete_marker: boolean;
  size: number;
  etag: string;
  content_type?: string;
  storage_class?: string;
  last_modified: number;
  tags: Record<string, string>;
  retain_mode?: 'GOVERNANCE' | 'COMPLIANCE' | '';
  retain_until?: number;
  legal_hold: boolean;
}

export async function fetchObjectDetail(
  bucket: string,
  key: string,
  versionID?: string,
): Promise<ObjectDetail> {
  const usp = new URLSearchParams();
  usp.set('key', key);
  if (versionID) usp.set('versionId', versionID);
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/object?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch object failed');
  return (await resp.json()) as ObjectDetail;
}

export interface ObjectVersionEntry {
  version_id: string;
  is_latest: boolean;
  is_delete_marker: boolean;
  size: number;
  etag: string;
  storage_class?: string;
  last_modified: number;
}

export async function fetchObjectVersions(
  bucket: string,
  key: string,
): Promise<ObjectVersionEntry[]> {
  const usp = new URLSearchParams();
  usp.set('key', key);
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/object-versions?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch object versions failed');
  const body = (await resp.json()) as { versions?: ObjectVersionEntry[] };
  return body.versions ?? [];
}

export async function setObjectTags(
  bucket: string,
  key: string,
  tags: Record<string, string>,
  versionID?: string,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/object-tags`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key, version_id: versionID, tags }),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set object tags failed');
}

export type ObjectRetentionMode = 'GOVERNANCE' | 'COMPLIANCE' | 'None';

export async function setObjectRetention(
  bucket: string,
  key: string,
  mode: ObjectRetentionMode,
  retainUntil: string | null,
  versionID?: string,
): Promise<void> {
  const body: Record<string, unknown> = { key, version_id: versionID, mode };
  if (mode !== 'None' && retainUntil) body.retain_until = retainUntil;
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/object-retention`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set object retention failed');
}

export async function setObjectLegalHold(
  bucket: string,
  key: string,
  enabled: boolean,
  versionID?: string,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/object-legal-hold`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key, version_id: versionID, enabled }),
    },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'set object legal hold failed');
}

export async function deleteObject(
  bucket: string,
  key: string,
  versionID?: string,
): Promise<void> {
  // Encode each path segment but preserve '/' separators so the gateway mux
  // sees the full nested key (the trailing-wildcard {key...} pattern).
  const encodedKey = key.split('/').map(encodeURIComponent).join('/');
  const usp = new URLSearchParams();
  if (versionID) usp.set('versionId', versionID);
  const qs = usp.toString();
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/objects/${encodedKey}${qs ? `?${qs}` : ''}`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete object failed');
}

// MultipartActiveRow is one in-flight multipart upload row surfaced by the
// watchdog page (US-017). bytes_uploaded sums the parts uploaded so far;
// initiator falls back to the bucket owner since the gateway does not
// record the originating principal on the multipart row.
export interface MultipartActiveRow {
  bucket: string;
  key: string;
  upload_id: string;
  initiated_at: number;
  age_seconds: number;
  storage_class: string;
  initiator: string;
  bytes_uploaded: number;
}

export interface MultipartActiveResponse {
  uploads: MultipartActiveRow[];
  total: number;
}

export interface MultipartActiveParams {
  bucket?: string;
  minAgeHours?: number;
  initiator?: string;
  page?: number;
  pageSize?: number;
}

export async function fetchMultipartActive(
  params: MultipartActiveParams,
): Promise<MultipartActiveResponse> {
  const usp = new URLSearchParams();
  if (params.bucket) usp.set('bucket', params.bucket);
  if (params.minAgeHours != null) usp.set('min_age_hours', String(params.minAgeHours));
  if (params.initiator) usp.set('initiator', params.initiator);
  if (params.page != null) usp.set('page', String(params.page));
  if (params.pageSize != null) usp.set('page_size', String(params.pageSize));
  const qs = usp.toString();
  const resp = await fetch(`/admin/v1/multipart/active${qs ? `?${qs}` : ''}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'multipart list failed');
  const body = (await resp.json()) as MultipartActiveResponse;
  return { uploads: body.uploads ?? [], total: body.total ?? 0 };
}

export interface MultipartAbortTarget {
  bucket: string;
  upload_id: string;
}

export interface MultipartAbortResult {
  bucket: string;
  upload_id: string;
  status: 'aborted' | 'error';
  code?: string;
  message?: string;
}

export interface MultipartAbortResponse {
  results: MultipartAbortResult[];
}

export async function abortMultipartBatch(
  uploads: MultipartAbortTarget[],
): Promise<MultipartAbortResponse> {
  const resp = await fetch('/admin/v1/multipart/abort', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ uploads }),
  });
  if (!resp.ok) throw await buildAdminError(resp, 'multipart abort failed');
  return (await resp.json()) as MultipartAbortResponse;
}

// US-018: audit-log viewer.
export interface AuditRecord {
  bucket_id: string;
  bucket: string;
  event_id: string;
  time: string;
  principal: string;
  action: string;
  resource: string;
  result: string;
  request_id: string;
  source_ip: string;
  user_agent: string;
}

export interface AuditListResponse {
  records: AuditRecord[];
  next_page_token: string;
}

export interface AuditQuery {
  since?: string;
  until?: string;
  action?: string;
  principal?: string;
  bucket?: string;
  pageToken?: string;
  limit?: number;
}

function buildAuditQuery(q: AuditQuery): URLSearchParams {
  const usp = new URLSearchParams();
  if (q.since) usp.set('since', q.since);
  if (q.until) usp.set('until', q.until);
  if (q.action) usp.set('action', q.action);
  if (q.principal) usp.set('principal', q.principal);
  if (q.bucket) usp.set('bucket', q.bucket);
  if (q.pageToken) usp.set('page_token', q.pageToken);
  if (q.limit != null) usp.set('limit', String(q.limit));
  return usp;
}

export async function fetchAuditLog(q: AuditQuery): Promise<AuditListResponse> {
  const usp = buildAuditQuery(q);
  const qs = usp.toString();
  const resp = await fetch(`/admin/v1/audit${qs ? `?${qs}` : ''}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'fetch audit log failed');
  const body = (await resp.json()) as AuditListResponse;
  return {
    records: body.records ?? [],
    next_page_token: body.next_page_token ?? '',
  };
}

// auditCSVUrl returns the URL the operator's browser hits to download the
// CSV. Using window.location.href triggers the browser's normal download flow
// (Content-Disposition attachment) and lets the same session cookie auth
// the request — no extra fetch wrapper needed.
export function auditCSVUrl(q: AuditQuery): string {
  const usp = buildAuditQuery(q);
  const qs = usp.toString();
  return `/admin/v1/audit.csv${qs ? `?${qs}` : ''}`;
}

// Settings (US-019, US-021) — read-only Settings page payloads + JWT rotation.
export interface ClusterSettings {
  cluster_name: string;
  region: string;
  version: string;
  prometheus_url: string;
  heartbeat_interval_ms: number;
  jwt_secret: string;
  jwt_ephemeral: boolean;
  jwt_secret_file: string;
  console_theme_default: string;
  audit_retention_ms: number;
  meta_backend: string;
  data_backend: string;
}

export interface CassandraSettings {
  hosts: string[];
  keyspace: string;
  local_dc: string;
  replication: string;
  username?: string;
}

export interface RADOSSettings {
  config_file: string;
  user: string;
  pool: string;
  namespace?: string;
  classes?: string;
  clusters?: string;
}

export interface TiKVSettings {
  endpoints: string[];
}

export interface SettingsBackends {
  cassandra: CassandraSettings;
  rados: RADOSSettings;
  tikv: TiKVSettings;
}

export interface SettingsResponse {
  settings: ClusterSettings;
  backends: SettingsBackends;
}

export async function fetchSettings(): Promise<SettingsResponse> {
  const resp = await fetch('/admin/v1/settings', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'fetch settings failed');
  return (await resp.json()) as SettingsResponse;
}

export interface S3BackendSettings {
  kind: string;
  clusters: string;
  classes: string;
}

export async function fetchSettingsDataBackend(): Promise<S3BackendSettings> {
  const resp = await fetch('/admin/v1/settings/data-backend', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'fetch data-backend settings failed');
  return (await resp.json()) as S3BackendSettings;
}

export interface RotateJWTResponse {
  rotated_at: number;
  file: string;
}

export async function rotateJWTSecret(): Promise<RotateJWTResponse> {
  const resp = await fetch('/admin/v1/settings/jwt/rotate', {
    method: 'POST',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw await buildAdminError(resp, 'rotate JWT secret failed');
  return (await resp.json()) as RotateJWTResponse;
}

// US-005 — OTel trace ring buffer. The wire shape mirrors ringbuf.Trace
// (internal/otel/ringbuf/ringbuf.go). Spans land in the order the SDK fired
// OnEnd; the UI reorders for the waterfall (root first, depth-first).
export interface TraceSpan {
  span_id: string;
  parent?: string;
  name: string;
  start_ns: number;
  end_ns: number;
  status: 'OK' | 'Error' | 'Unset' | string;
  attributes?: Record<string, unknown>;
}

export interface TraceDoc {
  trace_id: string;
  request_id?: string;
  root?: string;
  spans: TraceSpan[];
}

// fetchTrace pulls a single trace from the in-process ring buffer. Returns
// null on 404 NotFound (request id has aged out / never seen) so the page
// can render an empty-state without forcing the caller to catch.
// AdminApiError on other 4xx/5xx (e.g. RingbufUnavailable 503) so the UI
// can surface the {code, message} pair.
export async function fetchTrace(idOrRequestID: string): Promise<TraceDoc | null> {
  const resp = await fetch(
    `/admin/v1/diagnostics/trace/${encodeURIComponent(idOrRequestID)}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (resp.status === 404) return null;
  if (!resp.ok) throw await buildAdminError(resp, 'fetch trace failed');
  const body = (await resp.json()) as TraceDoc;
  return { ...body, spans: body.spans ?? [] };
}

// US-003 — slow-queries diagnostics. The wire shape mirrors slowQueriesResponse
// in internal/adminapi/diagnostics_slow_queries.go. The handler returns rows
// sorted by latency_ms DESC and a base64 page-token continuation cursor.
export interface SlowQueryRow {
  ts: string;
  bucket: string;
  bucket_id: string;
  op: string;
  latency_ms: number;
  status: number;
  request_id: string;
  principal: string;
  source_ip: string;
  object_key: string;
}

export interface SlowQueriesResponse {
  rows: SlowQueryRow[];
  next_page_token: string;
}

export interface SlowQueriesQuery {
  since?: string;
  minMs?: number;
  pageToken?: string;
}

export async function fetchSlowQueries(
  q: SlowQueriesQuery,
): Promise<SlowQueriesResponse> {
  const usp = new URLSearchParams();
  if (q.since) usp.set('since', q.since);
  if (q.minMs != null) usp.set('min_ms', String(q.minMs));
  if (q.pageToken) usp.set('page_token', q.pageToken);
  const qs = usp.toString();
  const resp = await fetch(
    `/admin/v1/diagnostics/slow-queries${qs ? `?${qs}` : ''}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch slow queries failed');
  const body = (await resp.json()) as SlowQueriesResponse;
  return { rows: body.rows ?? [], next_page_token: body.next_page_token ?? '' };
}

// US-007 — Hot Buckets matrix. Wire shape mirrors HotBucketsResponse in
// internal/adminapi/diagnostics_hot_buckets.go. Each series is one bucket
// with a list of (timestamp, value) points across the requested range.
export interface HotBucketPoint {
  ts: string;
  value: number;
}

export interface HotBucketSeries {
  bucket: string;
  values: HotBucketPoint[];
}

export interface HotBucketsResponse {
  matrix: HotBucketSeries[];
}

export interface HotBucketsQuery {
  range: string;
  step: string;
}

// fetchHotBuckets pulls the per-bucket request-rate matrix. 503
// `MetricsUnavailable` (Prom not configured) is preserved as an
// AdminApiError so the page can render the empty-state card by branching
// on `error.code === 'MetricsUnavailable'`.
export async function fetchHotBuckets(
  q: HotBucketsQuery,
): Promise<HotBucketsResponse> {
  const usp = new URLSearchParams();
  usp.set('range', q.range);
  usp.set('step', q.step);
  const resp = await fetch(
    `/admin/v1/diagnostics/hot-buckets?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch hot buckets failed');
  const body = (await resp.json()) as HotBucketsResponse;
  return { matrix: body.matrix ?? [] };
}

// US-013 — Bucket-Shard Distribution. Wire shape mirrors
// BucketDistributionResponse in internal/adminapi/buckets_distribution.go.
// One row per shard ID 0..N-1, contiguous and zero-filled when a shard has no
// live objects — same data the bucketstats sampler emits via the
// `strata_bucket_shard_{bytes,objects}` gauges.
export interface BucketShardStat {
  shard: number;
  bytes: number;
  objects: number;
}

export interface BucketDistributionResponse {
  shards: BucketShardStat[];
}

export async function fetchBucketDistribution(
  bucket: string,
): Promise<BucketDistributionResponse> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/distribution`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch bucket distribution failed');
  const body = (await resp.json()) as BucketDistributionResponse;
  return { shards: body.shards ?? [] };
}

// US-014 — Per-bucket replication queue age time-series. Wire shape mirrors
// BucketReplicationLagResponse in internal/adminapi/buckets_replication_lag.go.
// When the bucket has no replication configuration the response is
// `{empty: true}` and the UI tab hides itself.
export interface BucketReplicationLagPoint {
  ts: string;
  value: number;
}

export interface BucketReplicationLagResponse {
  empty?: boolean;
  reason?: string;
  values?: BucketReplicationLagPoint[];
}

export async function fetchBucketReplicationLag(
  bucket: string,
  range: string,
): Promise<BucketReplicationLagResponse> {
  const usp = new URLSearchParams();
  usp.set('range', range);
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(bucket)}/replication-lag?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch replication lag failed');
  const body = (await resp.json()) as BucketReplicationLagResponse;
  return {
    empty: body.empty ?? false,
    reason: body.reason ?? '',
    values: body.values ?? [],
  };
}

// US-009/US-010 — Hot Shards matrix for a single bucket. Wire shape mirrors
// HotShardsResponse in internal/adminapi/diagnostics_hot_shards.go. When the
// data backend is `s3` (no shards) the response shape is
// `{empty: true, reason: ...}` and the UI renders an explainer card.
export interface HotShardPoint {
  ts: string;
  value: number;
}

export interface HotShardSeries {
  shard: string;
  values: HotShardPoint[];
}

export interface HotShardsResponse {
  empty?: boolean;
  reason?: string;
  matrix?: HotShardSeries[];
}

export interface HotShardsQuery {
  bucket: string;
  range: string;
  step: string;
}

export async function fetchHotShards(
  q: HotShardsQuery,
): Promise<HotShardsResponse> {
  const usp = new URLSearchParams();
  usp.set('range', q.range);
  usp.set('step', q.step);
  const resp = await fetch(
    `/admin/v1/diagnostics/hot-shards/${encodeURIComponent(q.bucket)}?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch hot shards failed');
  const body = (await resp.json()) as HotShardsResponse;
  return {
    empty: body.empty ?? false,
    reason: body.reason ?? '',
    matrix: body.matrix ?? [],
  };
}

// US-011 — per-node drilldown. Wire shape mirrors NodeDrilldownResponse in
// internal/adminapi/diagnostics_node.go. The handler issues 5 PromQL queries
// scoped by `instance="<node-address>"` and returns CPU / memory / open-FDs /
// goroutines / GC-pause sparklines plus the heartbeat row for the drawer
// header.
export interface NodeMetricPoint {
  ts: string;
  value: number;
}

export interface NodeDrilldownNode {
  id: string;
  address: string;
  version: string;
  started_at: number;
  uptime_sec: number;
  status: string;
  workers: string[];
  leader_for: string[];
  last_heartbeat: number;
}

export interface NodeDrilldownResponse {
  node: NodeDrilldownNode;
  cpu: NodeMetricPoint[];
  mem: NodeMetricPoint[];
  fds: NodeMetricPoint[];
  goroutines: NodeMetricPoint[];
  gc_pause: NodeMetricPoint[];
}

export async function fetchNodeDrilldown(
  nodeID: string,
  range: string,
): Promise<NodeDrilldownResponse> {
  const usp = new URLSearchParams();
  usp.set('range', range);
  const resp = await fetch(
    `/admin/v1/diagnostics/node/${encodeURIComponent(nodeID)}?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch node drilldown failed');
  const body = (await resp.json()) as NodeDrilldownResponse;
  return {
    node: body.node,
    cpu: body.cpu ?? [],
    mem: body.mem ?? [],
    fds: body.fds ?? [],
    goroutines: body.goroutines ?? [],
    gc_pause: body.gc_pause ?? [],
  };
}

// US-010: Bucket / user quota + usage history.
//
// All "Get" wrappers return null on 404 NoSuchBucketQuota / NoSuchUserQuota
// so the UI can render the "no quota set" empty state without try/catch
// glue at the call site. Other 4xx/5xx still throw AdminApiError.

export interface BucketQuota {
  max_bytes: number;
  max_objects: number;
  max_bytes_per_object: number;
}

export interface UserQuota {
  max_buckets: number;
  total_max_bytes: number;
}

export interface UsageRow {
  bucket?: string;
  day: string;
  storage_class: string;
  byte_seconds: number;
  object_count_avg: number;
  object_count_max: number;
}

export interface BucketUsageResponse {
  rows: UsageRow[];
}

export interface UserUsageTotals {
  byte_seconds: number;
  objects: number;
}

export interface UserUsageResponse {
  rows: UsageRow[];
  totals: UserUsageTotals;
}

export async function fetchBucketQuota(name: string): Promise<BucketQuota | null> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/quota`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (resp.status === 404) {
    const err = await buildAdminError(resp, 'fetch bucket quota failed');
    if (err.code === 'NoSuchBucketQuota') return null;
    throw err;
  }
  if (!resp.ok) throw await buildAdminError(resp, 'fetch bucket quota failed');
  return (await resp.json()) as BucketQuota;
}

export async function setBucketQuota(name: string, q: BucketQuota): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/quota`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(q),
    },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'set bucket quota failed');
}

export async function deleteBucketQuota(name: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/quota`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete bucket quota failed');
}

export async function fetchUserQuota(userName: string): Promise<UserQuota | null> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/quota`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (resp.status === 404) {
    const err = await buildAdminError(resp, 'fetch user quota failed');
    if (err.code === 'NoSuchUserQuota') return null;
    throw err;
  }
  if (!resp.ok) throw await buildAdminError(resp, 'fetch user quota failed');
  return (await resp.json()) as UserQuota;
}

export async function setUserQuota(userName: string, q: UserQuota): Promise<void> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/quota`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(q),
    },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'set user quota failed');
}

export async function deleteUserQuota(userName: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/quota`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  throw await buildAdminError(resp, 'delete user quota failed');
}

export async function fetchBucketUsage(
  name: string,
  start: string,
  end: string,
): Promise<BucketUsageResponse> {
  const usp = new URLSearchParams();
  usp.set('start', start);
  usp.set('end', end);
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/usage?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch bucket usage failed');
  const body = (await resp.json()) as BucketUsageResponse;
  return { rows: body.rows ?? [] };
}

export async function fetchUserUsage(
  userName: string,
  start: string,
  end: string,
): Promise<UserUsageResponse> {
  const usp = new URLSearchParams();
  usp.set('start', start);
  usp.set('end', end);
  const resp = await fetch(
    `/admin/v1/iam/users/${encodeURIComponent(userName)}/usage?${usp.toString()}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) throw await buildAdminError(resp, 'fetch user usage failed');
  const body = (await resp.json()) as UserUsageResponse;
  return {
    rows: body.rows ?? [],
    totals: body.totals ?? { byte_seconds: 0, objects: 0 },
  };
}
