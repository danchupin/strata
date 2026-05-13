// Cluster topology wrappers for /admin/v1/clusters and the drain/undrain
// flips landed by the placement-rebalance cycle (US-006). Used by the
// Storage page Clusters subsection (US-002 placement-ui) and the AppShell
// drain banner (US-004). The wire shape mirrors
// adminapi.ClusterStateEntry in internal/adminapi/clusters_drain.go.

export interface ClusterStateEntry {
  id: string;
  state: 'live' | 'draining' | 'removed' | string;
  backend: 'rados' | 's3' | string;
}

export interface ClustersListResponse {
  clusters: ClusterStateEntry[];
  drain_strict: boolean;
}

export interface ClustersList {
  clusters: ClusterStateEntry[];
  drainStrict: boolean;
}

export async function fetchClusters(): Promise<ClustersList> {
  const resp = await fetch('/admin/v1/clusters', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) {
    throw new Error(`clusters: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as ClustersListResponse;
  return {
    clusters: body.clusters ?? [],
    drainStrict: Boolean(body.drain_strict),
  };
}

async function postFlip(id: string, action: 'drain' | 'undrain'): Promise<void> {
  const resp = await fetch(
    `/admin/v1/clusters/${encodeURIComponent(id)}/${action}`,
    { method: 'POST', credentials: 'same-origin' },
  );
  if (!resp.ok) {
    let detail = '';
    try {
      const body = (await resp.json()) as { message?: string; code?: string };
      detail = body.message ? `: ${body.message}` : '';
    } catch {
      // ignore JSON parse failure — fall back to status text below
    }
    throw new Error(`${action} ${id}: ${resp.status} ${resp.statusText}${detail}`);
  }
}

export function drainCluster(id: string): Promise<void> {
  return postFlip(id, 'drain');
}

export function undrainCluster(id: string): Promise<void> {
  return postFlip(id, 'undrain');
}

// ClusterRebalanceProgress is the wire shape returned by
// GET /admin/v1/clusters/{id}/rebalance-progress. Series points are
// [epoch_ms, rate(chunks moved into cluster per second)] over the last
// hour at 1-minute resolution. metrics_available=false means the chip
// should render "(metrics unavailable)" instead of zeros.
export interface ClusterRebalanceProgress {
  metrics_available: boolean;
  moved_total: number;
  refused_total: number;
  series: Array<[number, number]>;
}

// ClusterDrainProgress is the wire shape returned by
// GET /admin/v1/clusters/{id}/drain-progress (US-003 drain-lifecycle).
// Numeric fields go null when no value applies (live state, or before
// the rebalance worker commits its first scan). The UI uses
// base_chunks_at_start as the denominator for the progress bar — when
// null the card falls back to a plain text "<N> remaining" readout.
export interface ClusterDrainProgress {
  state: 'live' | 'draining' | 'removed' | string;
  chunks_on_cluster: number | null;
  bytes_on_cluster: number | null;
  base_chunks_at_start: number | null;
  last_scan_at: string | null;
  eta_seconds: number | null;
  deregister_ready: boolean | null;
  warnings?: string[];
}

export async function fetchClusterDrainProgress(
  id: string,
): Promise<ClusterDrainProgress> {
  const resp = await fetch(
    `/admin/v1/clusters/${encodeURIComponent(id)}/drain-progress`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) {
    throw new Error(
      `cluster ${id} drain-progress: ${resp.status} ${resp.statusText}`,
    );
  }
  const body = (await resp.json()) as ClusterDrainProgress;
  return body;
}

// BucketReferenceEntry is one row in the bucket-references list returned by
// GET /admin/v1/clusters/{id}/bucket-references (US-006 drain-lifecycle).
// chunk_count + bytes_used come from the live bucket_stats counter (not a
// manifest walk), so they reflect logical objects rather than chunk-on-disk
// distribution — the drain progress endpoint surfaces the latter.
export interface BucketReferenceEntry {
  name: string;
  weight: number;
  chunk_count: number;
  bytes_used: number;
}

export interface BucketReferencesResponse {
  buckets: BucketReferenceEntry[];
  total_buckets: number;
  next_offset: number | null;
}

export async function fetchClusterBucketReferences(
  id: string,
  limit = 100,
  offset = 0,
): Promise<BucketReferencesResponse> {
  const params = new URLSearchParams();
  if (limit !== 100) params.set('limit', String(limit));
  if (offset > 0) params.set('offset', String(offset));
  const qs = params.toString();
  const resp = await fetch(
    `/admin/v1/clusters/${encodeURIComponent(id)}/bucket-references${qs ? `?${qs}` : ''}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) {
    throw new Error(
      `cluster ${id} bucket-references: ${resp.status} ${resp.statusText}`,
    );
  }
  const body = (await resp.json()) as BucketReferencesResponse;
  return {
    buckets: Array.isArray(body.buckets) ? body.buckets : [],
    total_buckets: Number.isFinite(body.total_buckets) ? body.total_buckets : 0,
    next_offset:
      body.next_offset == null
        ? null
        : Number.isFinite(body.next_offset)
          ? body.next_offset
          : null,
  };
}

export async function fetchClusterRebalanceProgress(
  id: string,
): Promise<ClusterRebalanceProgress> {
  const resp = await fetch(
    `/admin/v1/clusters/${encodeURIComponent(id)}/rebalance-progress`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) {
    throw new Error(
      `cluster ${id} rebalance-progress: ${resp.status} ${resp.statusText}`,
    );
  }
  const body = (await resp.json()) as ClusterRebalanceProgress;
  return {
    metrics_available: Boolean(body.metrics_available),
    moved_total: Number.isFinite(body.moved_total) ? body.moved_total : 0,
    refused_total: Number.isFinite(body.refused_total) ? body.refused_total : 0,
    series: Array.isArray(body.series) ? body.series : [],
  };
}
