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
}

export async function fetchClusters(): Promise<ClusterStateEntry[]> {
  const resp = await fetch('/admin/v1/clusters', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) {
    throw new Error(`clusters: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as ClustersListResponse;
  return body.clusters ?? [];
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
