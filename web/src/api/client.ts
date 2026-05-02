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

export async function fetchBucket(name: string): Promise<BucketDetail> {
  const resp = await fetch(`/admin/v1/buckets/${encodeURIComponent(name)}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) throw new Error(`buckets/${name}: ${resp.status} ${resp.statusText}`);
  return (await resp.json()) as BucketDetail;
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
