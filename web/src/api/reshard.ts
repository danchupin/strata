// Per-bucket online-reshard wrappers for /admin/v1/buckets/{name}/reshard
// (US-006). Wire shape mirrors adminapi.BucketReshardJSON in
// internal/adminapi/buckets_reshard.go.
//
// The console mirrors the drain-progress UX: POST queues a job (202), the
// leader-elected `reshard` worker migrates rows out of band, and the GET
// endpoint is polled to watch the job converge (queued → running → idle).
//
// `supported` reflects whether the active meta backend physically moves rows.
// Only Cassandra implements meta.ReshardMigrator; on TiKV / memory a reshard
// is an immediate-complete no-op (range-scan / flat-map layouts carry no
// per-key shard). The UI disables the Reshard action when supported=false.

export type ReshardState = 'idle' | 'queued' | 'running';

export interface BucketReshard {
  ok: boolean;
  bucket: string;
  supported: boolean;
  state: ReshardState;
  source?: number;
  target?: number;
  shard_count: number;
  last_key?: string;
  started_at?: number;
  updated_at?: number;
}

// fetchBucketReshard reads the reshard job state for `name`. The endpoint
// always returns 200 (idle when no job is in flight) for an existing bucket,
// so callers don't special-case "not configured" the way placement does.
export async function fetchBucketReshard(name: string): Promise<BucketReshard> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/reshard`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (!resp.ok) {
    throw new Error(`reshard: ${resp.status} ${resp.statusText}`);
  }
  return (await resp.json()) as BucketReshard;
}

// startBucketReshard queues an online shard-resize to `target` (a positive
// power of two strictly greater than the current shard count). Returns the
// queued job descriptor (202). Throws on 4xx/5xx with the server message so
// the dialog can render the inline error (e.g. 409 OperationAborted when a
// reshard is already in flight).
export async function startBucketReshard(
  name: string,
  target: number,
): Promise<BucketReshard> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/reshard`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ target }),
    },
  );
  if (!resp.ok) {
    let detail = '';
    try {
      const j = (await resp.json()) as { message?: string; code?: string };
      detail = j.message ? `: ${j.message}` : '';
    } catch {
      // ignore JSON parse failure
    }
    throw new Error(`start reshard: ${resp.status} ${resp.statusText}${detail}`);
  }
  return (await resp.json()) as BucketReshard;
}

// nextPowerOfTwo returns the smallest power of two strictly greater than n.
// The Reshard action defaults the target to this — reshard only doubles up.
export function nextPowerOfTwo(n: number): number {
  if (!Number.isFinite(n) || n < 1) return 1;
  let p = 1;
  while (p <= n) p *= 2;
  return p;
}
