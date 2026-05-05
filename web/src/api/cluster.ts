// Cluster status + nodes fetch wrappers for /admin/v1/cluster/*. US-008
// will swap the bare fetch + setInterval pattern for TanStack Query polling.

export interface ClusterStatus {
  status: 'healthy' | 'degraded' | 'unhealthy' | string;
  version: string;
  started_at: number;
  uptime_sec: number;
  cluster_name: string;
  region: string;
  node_count: number;
  node_count_healthy: number;
  meta_backend: string;
  data_backend: string;
  // otel_endpoint mirrors OTEL_EXPORTER_OTLP_ENDPOINT — present (non-empty)
  // when an OTLP collector is wired on the gateway. The trace browser uses
  // it to render the "Open in Jaeger" deep link only when set.
  otel_endpoint?: string;
}

export interface ClusterNode {
  id: string;
  address: string;
  version: string;
  started_at: number;
  uptime_sec: number;
  status: 'healthy' | 'degraded' | 'unhealthy' | string;
  workers: string[];
  leader_for: string[];
  last_heartbeat: number;
}

export interface ClusterNodesResponse {
  nodes: ClusterNode[];
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

export async function fetchClusterNodes(): Promise<ClusterNode[]> {
  const resp = await fetch('/admin/v1/cluster/nodes', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) {
    throw new Error(`cluster/nodes: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as ClusterNodesResponse;
  return body.nodes ?? [];
}
