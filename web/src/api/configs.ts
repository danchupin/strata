// Read-only env-resolved tunable snapshots returned by the gateway
// (US-001 drain-rebalance-transparency). Both are env-static at boot,
// so callers set `staleTime: Infinity` on the TanStack query — a
// gateway restart is the only event that can shift the values.

export interface GCConfig {
  grace_seconds: number;
  interval_seconds: number;
  batch_size: number;
  concurrency: number;
  shards: number;
}

export interface RebalanceConfig {
  interval_seconds: number;
  rate_mb_s: number;
  inflight: number;
  shards: number;
  replicas_count: number;
}

function num(v: unknown, fallback = 0): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : fallback;
}

export async function fetchGCConfig(): Promise<GCConfig> {
  const resp = await fetch('/admin/v1/gc-config', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) {
    throw new Error(`gc-config: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as Partial<GCConfig>;
  return {
    grace_seconds: num(body.grace_seconds),
    interval_seconds: num(body.interval_seconds),
    batch_size: num(body.batch_size),
    concurrency: num(body.concurrency),
    shards: num(body.shards),
  };
}

export async function fetchRebalanceConfig(): Promise<RebalanceConfig> {
  const resp = await fetch('/admin/v1/rebalance-config', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) {
    throw new Error(`rebalance-config: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as Partial<RebalanceConfig>;
  return {
    interval_seconds: num(body.interval_seconds),
    rate_mb_s: num(body.rate_mb_s),
    inflight: num(body.inflight),
    shards: num(body.shards),
    replicas_count: num(body.replicas_count),
  };
}
