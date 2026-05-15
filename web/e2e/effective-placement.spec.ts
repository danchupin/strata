import { expect, test, type Page, type Route } from '@playwright/test';

// US-006 effective-placement — UI half of the operator journey defined by
// scripts/smoke-effective-placement.sh. Four scenarios drive the
// BucketDetail Placement-tab Strict toggle (US-004), the BucketDetail
// header "strict" badge (US-004), the BulkPlacementFixDialog strict-only
// filter + 'Flip to weighted' shortcut (US-005), and the ConfirmDrainModal
// compliance-locked stuck-row copy update (US-005).
//
// Memory-mode gateway has no registered RADOS clusters and no rebalance
// worker — every admin endpoint touched here is spoofed via page.route()
// (same pattern as drain-transparency.spec.ts and cluster-weights.spec.ts).

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

type ClusterState = 'live' | 'draining_readonly' | 'evacuating' | 'pending' | 'removed';
type ClusterMode = '' | 'readonly' | 'evacuate';
type PlacementMode = 'weighted' | 'strict' | '';

interface ClusterRow {
  id: string;
  state: ClusterState;
  mode: ClusterMode;
  weight: number;
  backend: 'rados' | 's3' | 'memory';
}

interface SuggestedPolicy {
  label: string;
  policy: Record<string, number>;
  placement_mode_override?: PlacementMode;
}

interface BucketImpactEntry {
  name: string;
  current_policy: Record<string, number> | null;
  category: 'stuck_single_policy' | 'stuck_no_policy' | 'migratable';
  chunk_count: number;
  bytes_used: number;
  placement_mode: 'weighted' | 'strict';
  suggested_policies: SuggestedPolicy[];
}

interface PlacementRow {
  placement: Record<string, number>;
  mode: PlacementMode;
}

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

interface SpoofState {
  clusters: ClusterRow[];
  stuckBuckets: BucketImpactEntry[];
  migratableChunks: number;
  stuckSingleChunks: number;
  stuckNoPolicyChunks: number;
  // placements is keyed by bucket name — supports the BucketDetail
  // Placement tab + the header badge live update via TanStack invalidation.
  placements: Record<string, PlacementRow | null>;
  // placementPuts captures every PUT body so tests can assert the wire
  // shape carried `mode` (audit semantics).
  placementPuts: { name: string; placement: Record<string, number>; mode?: PlacementMode }[];
}

function installRoutes(page: Page, state: SpoofState) {
  // ── /admin/v1/clusters ───────────────────────────────────────────────
  page.route('**/admin/v1/clusters', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ clusters: state.clusters }),
    });
  });

  // ── /admin/v1/clusters/{id}/drain | undrain ─────────────────────────
  page.route('**/admin/v1/clusters/*/drain', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain/);
    const id = m?.[1];
    const row = state.clusters.find((c) => c.id === id);
    const body = JSON.parse(route.request().postData() || '{}') as {
      mode?: ClusterMode;
    };
    if (row && body.mode === 'evacuate') {
      row.state = 'evacuating';
      row.mode = 'evacuate';
    } else if (row && body.mode === 'readonly') {
      row.state = 'draining_readonly';
      row.mode = 'readonly';
    }
    await route.fulfill({ status: 204, body: '' });
  });
  page.route('**/admin/v1/clusters/*/undrain', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    const m = route.request().url().match(/\/clusters\/([^/]+)\/undrain/);
    const id = m?.[1];
    const row = state.clusters.find((c) => c.id === id);
    if (row) {
      row.state = 'live';
      row.mode = '';
    }
    await route.fulfill({ status: 204, body: '' });
  });

  // ── /admin/v1/clusters/{id}/drain-impact ────────────────────────────
  page.route('**/admin/v1/clusters/*/drain-impact**', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-impact/);
    const id = m?.[1] ?? '';
    const row = state.clusters.find((c) => c.id === id);
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        cluster_id: id,
        current_state: row?.state ?? 'live',
        migratable_chunks: state.migratableChunks,
        stuck_single_policy_chunks: state.stuckSingleChunks,
        stuck_no_policy_chunks: state.stuckNoPolicyChunks,
        total_chunks:
          state.migratableChunks + state.stuckSingleChunks + state.stuckNoPolicyChunks,
        by_bucket: state.stuckBuckets,
        total_buckets: state.stuckBuckets.length,
        next_offset: null,
        last_scan_at: new Date().toISOString(),
      }),
    });
  });

  // ── /admin/v1/clusters/{id}/drain-progress ──────────────────────────
  page.route('**/admin/v1/clusters/*/drain-progress', async (route) => {
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-progress/);
    const id = m?.[1] ?? '';
    const row = state.clusters.find((c) => c.id === id);
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        state: row?.state ?? 'live',
        mode: row?.mode ?? '',
        chunks_on_cluster: null,
        bytes_on_cluster: null,
        base_chunks_at_start: null,
        last_scan_at: null,
        eta_seconds: null,
        deregister_ready: null,
        warnings: [],
        weight: row?.weight ?? 0,
      }),
    });
  });

  // ── /admin/v1/clusters/{id}/rebalance-progress ──────────────────────
  page.route('**/admin/v1/clusters/*/rebalance-progress', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        metrics_available: false,
        moved_total: 0,
        refused_total: 0,
        series: [],
      }),
    });
  });

  // ── /admin/v1/storage/data ──────────────────────────────────────────
  page.route('**/admin/v1/storage/data', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        backend: 'rados',
        pools: state.clusters.map((c) => ({
          name: `strata-data-${c.id}`,
          class: 'STANDARD',
          cluster: c.id,
          bytes_used: 1024,
          chunk_count: 1,
          num_replicas: 3,
          state: 'active+clean',
        })),
        warnings: [],
      }),
    });
  });

  // ── /admin/v1/buckets/{name}/placement (GET/PUT) ───────────────────
  page.route('**/admin/v1/buckets/*/placement', async (route: Route) => {
    const m = route.request().url().match(/\/admin\/v1\/buckets\/([^/]+)\/placement/);
    const name = m?.[1] ?? '';
    const method = route.request().method();
    if (method === 'GET') {
      const row = state.placements[name];
      if (!row) {
        await route.fulfill({
          status: 404,
          contentType: 'application/json',
          body: JSON.stringify({ code: 'NoSuchPlacement' }),
        });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          placement: row.placement,
          mode: row.mode === '' ? 'weighted' : row.mode,
        }),
      });
      return;
    }
    if (method === 'PUT') {
      const body = JSON.parse(route.request().postData() || '{}') as {
        placement?: Record<string, number>;
        mode?: PlacementMode;
      };
      state.placementPuts.push({
        name,
        placement: body.placement ?? {},
        mode: body.mode,
      });
      state.placements[name] = {
        placement: body.placement ?? {},
        mode: (body.mode ?? 'weighted') as PlacementMode,
      };
      // Mirror the server's drain-impact cache invalidation — drop any
      // stuck row whose name matches so the parent modal's refetch sees
      // the bucket as resolved.
      const idx = state.stuckBuckets.findIndex((b) => b.name === name);
      if (idx >= 0) {
        state.stuckSingleChunks = Math.max(
          0,
          state.stuckSingleChunks - state.stuckBuckets[idx].chunk_count,
        );
        state.stuckBuckets.splice(idx, 1);
      }
      await route.fulfill({ status: 204, body: '' });
      return;
    }
    await route.fallback();
  });

  // ── /admin/v1/buckets/{name} (GET) ─────────────────────────────────
  page.route('**/admin/v1/buckets/*', async (route: Route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const m = route.request().url().match(/\/admin\/v1\/buckets\/([^/?]+)(?:\?.*)?$/);
    const name = m?.[1] ?? '';
    if (!name) return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        name,
        owner: 'owner',
        region: 'strata-local',
        created_at: Math.floor(Date.now() / 1000),
        versioning: 'Off',
        object_lock: false,
        size_bytes: 0,
        object_count: 0,
        backend_presign: false,
        shard_count: 64,
      }),
    });
  });

  // ── /admin/v1/buckets/{name}/objects (empty page) ─────────────────
  page.route('**/admin/v1/buckets/*/objects**', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        objects: [],
        common_prefixes: [],
        next_marker: null,
      }),
    });
  });
}

function makeStrictBucket(name: string, chunkCount: number): BucketImpactEntry {
  return {
    name,
    current_policy: { cephb: 1 },
    category: 'stuck_single_policy',
    chunk_count: chunkCount,
    bytes_used: chunkCount * 1024,
    placement_mode: 'strict',
    suggested_policies: [
      {
        label: 'Flip to weighted (auto-fallback to cluster weights)',
        policy: { cephb: 1 },
        placement_mode_override: 'weighted',
      },
      {
        label: 'Replace draining cluster with cepha (strict)',
        policy: { cepha: 1 },
        placement_mode_override: 'strict',
      },
    ],
  };
}

function makeWeightedBucket(name: string, chunkCount: number): BucketImpactEntry {
  return {
    name,
    current_policy: { cephb: 1 },
    category: 'stuck_single_policy',
    chunk_count: chunkCount,
    bytes_used: chunkCount * 1024,
    placement_mode: 'weighted',
    suggested_policies: [
      {
        label: 'Replace draining cluster with cepha',
        policy: { cepha: 1 },
      },
    ],
  };
}

test.describe('Strata console — effective-placement (US-006)', () => {
  test('scenario-A: BucketDetail Placement tab Strict toggle — off→on opens confirm, on→off relaxes one-click', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', weight: 100, backend: 'rados' },
        { id: 'cephb', state: 'live', mode: '', weight: 100, backend: 'rados' },
      ],
      stuckBuckets: [],
      migratableChunks: 0,
      stuckSingleChunks: 0,
      stuckNoPolicyChunks: 0,
      placements: {
        'bkt-strict-toggle': { placement: { cephb: 100 }, mode: 'weighted' },
      },
      placementPuts: [],
    };
    installRoutes(page, state);

    await login(page);
    await page.goto('/console/buckets/bkt-strict-toggle');
    await page.getByRole('tab', { name: 'Placement' }).click();

    // Header badge should NOT render — bucket is weighted.
    await expect(page.getByText(/^strict$/).first()).toHaveCount(0);

    // Switch in Placement tab is off (weighted is the default).
    const strictSwitch = page.getByRole('switch', {
      name: 'Strict placement mode',
    });
    await expect(strictSwitch).toBeVisible({ timeout: 10_000 });
    await expect(strictSwitch).toHaveAttribute('aria-checked', 'false');

    // Flip ON → confirmation Dialog appears.
    await strictSwitch.click();
    const confirm = page
      .getByRole('dialog')
      .filter({ hasText: /Enable strict placement\?/ });
    await expect(confirm).toBeVisible({ timeout: 5_000 });
    await confirm.getByRole('button', { name: 'Enable strict' }).click();
    await expect(confirm).toBeHidden({ timeout: 5_000 });
    await expect(strictSwitch).toHaveAttribute('aria-checked', 'true');

    // Save lands a PUT with mode=strict in the body.
    await page.getByRole('button', { name: 'Save placement' }).click();
    await expect.poll(() => state.placementPuts.length).toBeGreaterThan(0);
    const lastPut = state.placementPuts[state.placementPuts.length - 1];
    expect(lastPut.mode).toBe('strict');
    expect(lastPut.placement).toEqual({ cephb: 100 });

    // Header strict badge mounts after invalidation.
    await expect(page.getByText(/^strict$/).first()).toBeVisible({
      timeout: 10_000,
    });

    // Flip OFF → no confirmation (relaxing direction).
    await strictSwitch.click();
    await expect(strictSwitch).toHaveAttribute('aria-checked', 'false');
    // No confirm dialog appeared this time.
    await expect(
      page.getByRole('dialog').filter({ hasText: /Enable strict placement\?/ }),
    ).toHaveCount(0);
  });

  test('scenario-B: BulkPlacementFixDialog filters to strict-only — weighted stuck rows are hidden', async ({
    page,
  }) => {
    const strict = makeStrictBucket('bkt-compliance', 6);
    const weighted = makeWeightedBucket('bkt-weighted-stuck', 4);
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', weight: 100, backend: 'rados' },
        { id: 'cephb', state: 'live', mode: '', weight: 100, backend: 'rados' },
      ],
      stuckBuckets: [strict, weighted],
      migratableChunks: 12,
      stuckSingleChunks: strict.chunk_count + weighted.chunk_count,
      stuckNoPolicyChunks: 0,
      placements: {
        'bkt-compliance': { placement: { cephb: 1 }, mode: 'strict' },
        'bkt-weighted-stuck': { placement: { cephb: 1 }, mode: 'weighted' },
      },
      placementPuts: [],
    };
    installRoutes(page, state);

    await login(page);
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(page.locator('span[title="cephb"]').first()).toBeVisible({
      timeout: 10_000,
    });

    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();
    await cephbCard.getByRole('button', { name: 'Drain' }).click();
    const drainDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Drain cluster/ });
    await expect(drainDialog).toBeVisible();

    // Flip to evacuate; impact analysis fires.
    await drainDialog.getByTestId('cd-mode-evacuate').click();
    await expect(drainDialog.getByTestId('cd-impact')).toBeVisible({
      timeout: 10_000,
    });

    // Compliance-locked label — only the strict bucket counts toward the
    // amber warning (weighted stuck buckets auto-resolve via cluster
    // weights post US-003 and don't gate Submit).
    const stuckWarning = drainDialog.getByTestId('cd-stuck-warning');
    await expect(stuckWarning).toBeVisible({ timeout: 10_000 });
    await expect(stuckWarning).toContainText(/1 compliance-locked bucket/);

    // Bulk-fix CTA carries the compliance-locked count, not raw stuck.
    const bulkBtn = drainDialog.getByTestId('cd-bulk-fix');
    await expect(bulkBtn).toContainText(/Fix 1 compliance-locked bucket/);
    await bulkBtn.click();

    const bulkDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Fix compliance-locked buckets/ });
    await expect(bulkDialog).toBeVisible({ timeout: 10_000 });

    // Only the strict bucket row renders; the weighted stuck bucket is
    // filtered out by strictOnly even though the modal handed both rows.
    await expect(bulkDialog.getByTestId('bpf-row-bkt-compliance')).toBeVisible();
    await expect(bulkDialog.getByTestId('bpf-row-bkt-weighted-stuck')).toHaveCount(0);
  });

  test('scenario-C: BulkPlacementFixDialog "Flip to weighted" submits placement_mode_override=weighted', async ({
    page,
  }) => {
    const strict = makeStrictBucket('bkt-flip-target', 5);
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', weight: 100, backend: 'rados' },
        { id: 'cephb', state: 'live', mode: '', weight: 100, backend: 'rados' },
      ],
      stuckBuckets: [strict],
      migratableChunks: 8,
      stuckSingleChunks: strict.chunk_count,
      stuckNoPolicyChunks: 0,
      placements: {
        'bkt-flip-target': { placement: { cephb: 1 }, mode: 'strict' },
      },
      placementPuts: [],
    };
    installRoutes(page, state);

    await login(page);
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(page.locator('span[title="cephb"]').first()).toBeVisible({
      timeout: 10_000,
    });

    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();
    await cephbCard.getByRole('button', { name: 'Drain' }).click();
    const drainDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Drain cluster/ });
    await expect(drainDialog).toBeVisible();
    await drainDialog.getByTestId('cd-mode-evacuate').click();
    await expect(drainDialog.getByTestId('cd-bulk-fix')).toBeVisible({
      timeout: 10_000,
    });
    await drainDialog.getByTestId('cd-bulk-fix').click();

    const bulkDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Fix compliance-locked buckets/ });
    await expect(bulkDialog).toBeVisible({ timeout: 10_000 });
    // Default per-bucket suggestion index 0 is the Flip-to-weighted
    // shortcut emitted by the server for strict-stuck buckets — Apply
    // submits placement_mode_override=weighted.
    await bulkDialog.getByTestId('bpf-apply').click();
    await expect(bulkDialog).toBeHidden({ timeout: 15_000 });

    // PUT body asserts: mode field = "weighted" (the audit-stamped flip).
    await expect.poll(() => state.placementPuts.length).toBeGreaterThan(0);
    const last = state.placementPuts[state.placementPuts.length - 1];
    expect(last.name).toBe('bkt-flip-target');
    expect(last.mode).toBe('weighted');

    // Parent modal refetches /drain-impact — stuck=0, Submit enabled.
    await expect(drainDialog.getByTestId('cd-stuck-warning')).toBeHidden({
      timeout: 15_000,
    });
    const submit = drainDialog.getByTestId('cd-submit');
    await drainDialog.getByLabel('Cluster id').fill('cephb');
    await expect(submit).toBeEnabled({ timeout: 10_000 });
  });

  test('scenario-D: ConfirmDrainModal amber row reads "compliance-locked" + Submit blocks while compliance-locked > 0', async ({
    page,
  }) => {
    const strict = makeStrictBucket('bkt-compliance-row', 3);
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', weight: 100, backend: 'rados' },
        { id: 'cephb', state: 'live', mode: '', weight: 100, backend: 'rados' },
      ],
      stuckBuckets: [strict],
      migratableChunks: 4,
      stuckSingleChunks: strict.chunk_count,
      stuckNoPolicyChunks: 0,
      placements: {},
      placementPuts: [],
    };
    installRoutes(page, state);

    await login(page);
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(page.locator('span[title="cephb"]').first()).toBeVisible({
      timeout: 10_000,
    });
    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();
    await cephbCard.getByRole('button', { name: 'Drain' }).click();
    const drainDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Drain cluster/ });
    await expect(drainDialog).toBeVisible();
    await drainDialog.getByTestId('cd-mode-evacuate').click();
    const stuckWarning = drainDialog.getByTestId('cd-stuck-warning');
    await expect(stuckWarning).toBeVisible({ timeout: 10_000 });
    // New copy from US-005 — "compliance-locked" replaces the old generic
    // "stuck" wording everywhere on this row.
    await expect(stuckWarning).toContainText(/compliance-locked/);

    // Typed-confirm matches but Submit stays disabled because the
    // compliance-locked count is > 0 (US-005 Submit gating).
    const submit = drainDialog.getByTestId('cd-submit');
    await drainDialog.getByLabel('Cluster id').fill('cephb');
    await expect(submit).toBeDisabled();
  });
});
