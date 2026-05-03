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
