// Cluster topology wrappers for /admin/v1/clusters and the drain/undrain
// flips landed by the placement-rebalance cycle (US-006). Used by the
// Storage page Clusters subsection (US-002 placement-ui) and the AppShell
// drain banner (US-004). The wire shape mirrors
// adminapi.ClusterStateEntry in internal/adminapi/clusters_drain.go.

// ClusterState is the 4-state machine introduced in US-001 drain-transparency.
// The legacy "draining" value is still accepted from the wire for
// compatibility with backends mid-migration; the server normalizes it on
// read so this branch should be exercised only by very stale UI fetches.
export type ClusterState =
  | 'live'
  | 'draining_readonly'
  | 'evacuating'
  | 'removed'
  | 'draining';

export type ClusterMode = '' | 'readonly' | 'evacuate';

export interface ClusterStateEntry {
  id: string;
  state: ClusterState | string;
  mode: ClusterMode | string;
  backend: 'rados' | 's3' | string;
}

// isDrainingState collapses the 4-state machine into a boolean "stop-
// writes" predicate so existing call sites that asked `state==='draining'`
// keep working through US-006 redesign of the UI.
export function isDrainingState(state: string | undefined): boolean {
  return (
    state === 'draining_readonly' ||
    state === 'evacuating' ||
    state === 'draining'
  );
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

async function postFlip(
  id: string,
  action: 'drain' | 'undrain',
  body: unknown,
): Promise<void> {
  const init: RequestInit = { method: 'POST', credentials: 'same-origin' };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    init.headers = { 'Content-Type': 'application/json' };
  }
  const resp = await fetch(
    `/admin/v1/clusters/${encodeURIComponent(id)}/${action}`,
    init,
  );
  if (!resp.ok) {
    let detail = '';
    try {
      const j = (await resp.json()) as { message?: string; code?: string };
      detail = j.message ? `: ${j.message}` : '';
    } catch {
      // ignore JSON parse failure — fall back to status text below
    }
    throw new Error(`${action} ${id}: ${resp.status} ${resp.statusText}${detail}`);
  }
}

// drainCluster posts {mode} to /clusters/{id}/drain. Mode is required
// (no default) per US-001 drain-transparency. Pass 'readonly' for the
// stop-writes-only maintenance mode or 'evacuate' for the full
// decommission mode that also migrates chunks.
export function drainCluster(
  id: string,
  mode: 'readonly' | 'evacuate' = 'evacuate',
): Promise<void> {
  return postFlip(id, 'drain', { mode });
}

export function undrainCluster(id: string): Promise<void> {
  return postFlip(id, 'undrain', undefined);
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
  state: ClusterState | string;
  mode: ClusterMode | string;
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

// SuggestedPolicy is one operator-facing remediation option returned per
// affected bucket by GET /admin/v1/clusters/{id}/drain-impact (US-003
// drain-transparency). Label is the human-readable text rendered in the
// BulkPlacementFixDialog dropdown; Policy is the {clusterID: weight}
// body the operator would PUT to /admin/v1/buckets/{name}/placement.
export interface SuggestedPolicy {
  label: string;
  policy: Record<string, number>;
}

// BucketImpactEntry mirrors adminapi.BucketImpactEntry from
// internal/adminapi/clusters_drain_impact.go. current_policy is null when
// the bucket has no Placement (stuck_no_policy category).
export interface BucketImpactEntry {
  name: string;
  current_policy: Record<string, number> | null;
  category: 'migratable' | 'stuck_single_policy' | 'stuck_no_policy' | string;
  chunk_count: number;
  bytes_used: number;
  suggested_policies: SuggestedPolicy[] | null;
}

// ClusterDrainImpactResponse mirrors adminapi.ClusterDrainImpactResponse.
// next_offset is null when the by_bucket slice is the final page.
export interface ClusterDrainImpactResponse {
  cluster_id: string;
  current_state: string;
  migratable_chunks: number;
  stuck_single_policy_chunks: number;
  stuck_no_policy_chunks: number;
  total_chunks: number;
  by_bucket: BucketImpactEntry[];
  total_buckets: number;
  next_offset: number | null;
  last_scan_at: string | null;
}

export async function fetchClusterDrainImpact(
  id: string,
  limit = 100,
  offset = 0,
): Promise<ClusterDrainImpactResponse> {
  const params = new URLSearchParams();
  if (limit !== 100) params.set('limit', String(limit));
  if (offset > 0) params.set('offset', String(offset));
  const qs = params.toString();
  const resp = await fetch(
    `/admin/v1/clusters/${encodeURIComponent(id)}/drain-impact${qs ? `?${qs}` : ''}`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) {
    let detail = '';
    try {
      const j = (await resp.json()) as { message?: string };
      detail = j.message ? `: ${j.message}` : '';
    } catch {
      // ignore
    }
    throw new Error(
      `cluster ${id} drain-impact: ${resp.status} ${resp.statusText}${detail}`,
    );
  }
  const body = (await resp.json()) as ClusterDrainImpactResponse;
  return {
    cluster_id: body.cluster_id ?? id,
    current_state: body.current_state ?? '',
    migratable_chunks: Number.isFinite(body.migratable_chunks)
      ? body.migratable_chunks
      : 0,
    stuck_single_policy_chunks: Number.isFinite(body.stuck_single_policy_chunks)
      ? body.stuck_single_policy_chunks
      : 0,
    stuck_no_policy_chunks: Number.isFinite(body.stuck_no_policy_chunks)
      ? body.stuck_no_policy_chunks
      : 0,
    total_chunks: Number.isFinite(body.total_chunks) ? body.total_chunks : 0,
    by_bucket: Array.isArray(body.by_bucket) ? body.by_bucket : [],
    total_buckets: Number.isFinite(body.total_buckets) ? body.total_buckets : 0,
    next_offset:
      body.next_offset == null
        ? null
        : Number.isFinite(body.next_offset)
          ? body.next_offset
          : null,
    last_scan_at: body.last_scan_at ?? null,
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
