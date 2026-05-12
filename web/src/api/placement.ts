// Per-bucket placement-policy wrappers for /admin/v1/buckets/{name}/placement
// landed by the placement-rebalance cycle (US-001 server-side). Used by the
// BucketDetail Placement tab (US-003 placement-ui). Wire shape mirrors
// adminapi.BucketPlacementJSON in internal/adminapi/buckets_placement.go.

export interface BucketPlacementJSON {
  placement: Record<string, number>;
}

// fetchBucketPlacement returns the configured placement map for `name`.
// Returns null when the server responds 404 NoSuchPlacement so the editor
// can render the "Default routing" empty state without a try/catch.
export async function fetchBucketPlacement(
  name: string,
): Promise<Record<string, number> | null> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/placement`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (resp.status === 404) return null;
  if (!resp.ok) {
    throw new Error(`placement: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as BucketPlacementJSON;
  return body.placement ?? {};
}

export async function setBucketPlacement(
  name: string,
  placement: Record<string, number>,
): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/placement`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ placement } satisfies BucketPlacementJSON),
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
    throw new Error(`set placement: ${resp.status} ${resp.statusText}${detail}`);
  }
}

export async function deleteBucketPlacement(name: string): Promise<void> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/placement`,
    { method: 'DELETE', credentials: 'same-origin' },
  );
  if (resp.status === 204) return;
  if (!resp.ok) {
    throw new Error(`delete placement: ${resp.status} ${resp.statusText}`);
  }
}
