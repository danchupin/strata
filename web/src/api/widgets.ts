// Top-N widget fetch wrappers for the Cluster Overview page (US-007).
// Both endpoints carry a `metrics_available` flag — false when the gateway
// has no STRATA_PROMETHEUS_URL configured (or the upstream PromQL failed).
// The UI surfaces a "Metrics unavailable" warning instead of '—' rows.

export type BucketsTopBy = 'size' | 'requests';
export type ConsumersTopBy = 'requests' | 'bytes';

export interface BucketTop {
  name: string;
  size_bytes: number;
  object_count: number;
  request_count_24h: number;
}

export interface BucketsTopResponse {
  buckets: BucketTop[];
  metrics_available: boolean;
}

export interface ConsumerTop {
  access_key: string;
  user: string;
  request_count_24h: number;
  bytes_24h: number;
}

export interface ConsumersTopResponse {
  consumers: ConsumerTop[];
  metrics_available: boolean;
}

export async function fetchTopBuckets(by: BucketsTopBy, limit = 10): Promise<BucketsTopResponse> {
  const url = `/admin/v1/buckets/top?by=${encodeURIComponent(by)}&limit=${limit}`;
  const resp = await fetch(url, { method: 'GET', credentials: 'same-origin' });
  if (!resp.ok) {
    throw new Error(`buckets/top: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as BucketsTopResponse;
  return {
    buckets: body.buckets ?? [],
    metrics_available: Boolean(body.metrics_available),
  };
}

export async function fetchTopConsumers(by: ConsumersTopBy, limit = 10): Promise<ConsumersTopResponse> {
  const url = `/admin/v1/consumers/top?by=${encodeURIComponent(by)}&limit=${limit}`;
  const resp = await fetch(url, { method: 'GET', credentials: 'same-origin' });
  if (!resp.ok) {
    throw new Error(`consumers/top: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as ConsumersTopResponse;
  return {
    consumers: body.consumers ?? [],
    metrics_available: Boolean(body.metrics_available),
  };
}
