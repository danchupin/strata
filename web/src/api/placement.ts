// Per-bucket placement-policy wrappers for /admin/v1/buckets/{name}/placement
// landed by the placement-rebalance cycle (US-001 server-side). Used by the
// BucketDetail Placement tab (US-003 placement-ui). Wire shape mirrors
// adminapi.BucketPlacementJSON in internal/adminapi/buckets_placement.go.
//
// PlacementMode (US-001 effective-placement) is the bucket's mode override:
// "weighted" (default) → EffectivePolicy falls back to cluster-weights when
// the bucket's Placement points only at draining clusters; "strict" disables
// that fallback (compliance / data-sovereignty pin). GET coerces the legacy
// empty-string value to "weighted" so the client never sees `""`.

export type PlacementMode = 'weighted' | 'strict';

export interface BucketPlacementJSON {
  placement: Record<string, number>;
  // mode is optional on the wire (server omits empty/legacy) but always
  // populated by GET — the server coerces "" → "weighted" per US-001.
  mode?: PlacementMode;
}

export interface BucketPlacement {
  placement: Record<string, number>;
  mode: PlacementMode;
}

// fetchBucketPlacement returns the configured placement map + mode for
// `name`. Returns null when the server responds 404 NoSuchPlacement so the
// editor can render the "Default routing" empty state without a try/catch.
export async function fetchBucketPlacement(
  name: string,
): Promise<BucketPlacement | null> {
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/placement`,
    { method: 'GET', credentials: 'same-origin' },
  );
  if (resp.status === 404) return null;
  if (!resp.ok) {
    throw new Error(`placement: ${resp.status} ${resp.statusText}`);
  }
  const body = (await resp.json()) as BucketPlacementJSON;
  return {
    placement: body.placement ?? {},
    mode: normalizeMode(body.mode),
  };
}

// setBucketPlacement persists the weights and (optionally) the mode flag.
// When `mode` is supplied the server stamps the audit action
// `admin:UpdateBucketPlacementMode`; otherwise the legacy
// `admin:PutBucketPlacement` action is used.
export async function setBucketPlacement(
  name: string,
  placement: Record<string, number>,
  mode?: PlacementMode,
): Promise<void> {
  const body: BucketPlacementJSON = { placement };
  if (mode !== undefined) body.mode = mode;
  const resp = await fetch(
    `/admin/v1/buckets/${encodeURIComponent(name)}/placement`,
    {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
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

// normalizeMode mirrors meta.NormalizePlacementMode on the server. Defends
// against future wire-shape drift: any unknown / empty value falls back to
// "weighted" so the UI never renders a meaningless mode chip.
export function normalizeMode(m: string | undefined): PlacementMode {
  return m === 'strict' ? 'strict' : 'weighted';
}
