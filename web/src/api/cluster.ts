// Cluster status fetch wrapper for /admin/v1/cluster/status. Phase 1
// returns the minimum subset that drives the top bar (cluster_name +
// version); US-006 fills the remaining derived fields.

export interface ClusterStatus {
  status: string;
  version: string;
  started_at: number;
  cluster_name: string;
  uptime_sec?: number;
  node_count?: number;
  node_count_healthy?: number;
  meta_backend?: string;
  data_backend?: string;
}

export async function fetchClusterStatus(): Promise<ClusterStatus> {
  const resp = await fetch('/admin/v1/cluster/status', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) {
    throw new Error(`cluster/status: ${resp.status} ${resp.statusText}`);
  }
  return (await resp.json()) as ClusterStatus;
}
