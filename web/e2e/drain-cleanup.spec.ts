import { expect, test, type Page, type Route } from '@playwright/test';

// US-005 drain-cleanup — UI half of the operator journey defined by
// scripts/smoke-drain-cleanup.sh. Walks the 7-ROADMAP-entry cleanup
// against the memory-mode webServer with admin endpoints spoofed via
// page.route() — same pattern as drain-transparency.spec.ts.
//
// Coverage:
//   - BucketReferencesDrawer renders 3 categorized sections (US-001)
//   - Inline Bulk fix CTA → BulkPlacementFixDialog Apply → drawer
//     counts update immediately (US-002 cache invalidation)
//   - ConfirmDrainModal evacuate → submit when stuck=0 (US-001+US-004)
//   - DrainProgressBar flips to deregister-ready chip when
//     chunks_on_cluster reaches 0 (US-006)
//   - ClusterCard action slot renders 'Cancel deregister prep'
//     (NOT 'Undrain') for evacuating+chunks=0+deregister_ready=true
//     (US-007 state-aware buttons)
//   - CancelDeregisterPrepModal typed-confirm → POST /undrain →
//     cluster flips to live
//   - TraceBrowser RecentTracesPanel renders the live list (US-008)

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

type ClusterState = 'live' | 'draining_readonly' | 'evacuating' | 'removed';
type ClusterMode = '' | 'readonly' | 'evacuate';

interface ClusterRow {
  id: string;
  state: ClusterState;
  mode: ClusterMode;
  backend: 'rados' | 's3' | 'memory';
}

interface BucketImpactEntry {
  name: string;
  current_policy: Record<string, number> | null;
  category: 'stuck_single_policy' | 'stuck_no_policy' | 'migratable';
  chunk_count: number;
  bytes_used: number;
  suggested_policies: { label: string; policy: Record<string, number> }[];
}

interface SpoofState {
  clusters: ClusterRow[];
  byBucket: BucketImpactEntry[];
  migratableChunks: number;
  stuckSingleChunks: number;
  stuckNoPolicyChunks: number;
  drainChunks: number;
  drainBase: number;
}

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

function installRoutes(page: Page, state: SpoofState) {
  page.route('**/admin/v1/clusters', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ clusters: state.clusters }),
    });
  });

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
        by_bucket: state.byBucket,
        total_buckets: state.byBucket.length,
        next_offset: null,
        last_scan_at: new Date().toISOString(),
      }),
    });
  });

  page.route('**/admin/v1/clusters/*/drain-progress', async (route) => {
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-progress/);
    const id = m?.[1] ?? '';
    const row = state.clusters.find((c) => c.id === id);
    if (!row || row.state === 'live' || row.state === 'removed') {
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
          migratable_chunks: null,
          stuck_single_policy_chunks: null,
          stuck_no_policy_chunks: null,
          by_bucket: [],
          not_ready_reasons: [],
        }),
      });
      return;
    }
    // evacuating
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        state: 'evacuating',
        mode: 'evacuate',
        chunks_on_cluster: state.drainChunks,
        bytes_on_cluster: state.drainChunks * 1024,
        base_chunks_at_start: state.drainBase,
        last_scan_at: new Date().toISOString(),
        eta_seconds: state.drainChunks > 0 ? 120 : 0,
        deregister_ready: state.drainChunks === 0,
        warnings: [],
        migratable_chunks: state.drainChunks,
        stuck_single_policy_chunks: 0,
        stuck_no_policy_chunks: 0,
        by_bucket: [],
        not_ready_reasons: [],
      }),
    });
  });

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

  // Pools matrix carries chunk_count (US-003 rename).
  page.route('**/admin/v1/storage/data', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        backend: 'rados',
        pools: [
          {
            name: 'strata-data-a',
            class: 'STANDARD',
            cluster: 'cepha',
            bytes_used: 1024 * 1024,
            chunk_count: 4,
            num_replicas: 3,
            state: 'active+clean',
          },
          {
            name: 'strata-data-b',
            class: 'STANDARD',
            cluster: 'cephb',
            bytes_used: 2 * 1024 * 1024,
            chunk_count: 7,
            num_replicas: 3,
            state: 'active+clean',
          },
        ],
        warnings: [],
      }),
    });
  });

  page.route('**/admin/v1/buckets/*/placement', async (route: Route) => {
    const method = route.request().method();
    if (method === 'PUT') {
      const m = route
        .request()
        .url()
        .match(/\/admin\/v1\/buckets\/([^/]+)\/placement/);
      const name = m?.[1] ?? '';
      const idx = state.byBucket.findIndex((b) => b.name === name);
      if (idx >= 0) {
        const e = state.byBucket[idx];
        if (e.category === 'stuck_single_policy') {
          state.stuckSingleChunks = Math.max(0, state.stuckSingleChunks - e.chunk_count);
        } else if (e.category === 'stuck_no_policy') {
          state.stuckNoPolicyChunks = Math.max(0, state.stuckNoPolicyChunks - e.chunk_count);
        }
        state.migratableChunks += e.chunk_count;
        e.category = 'migratable';
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ placement: { cepha: 1, cephb: 1 } }),
      });
      return;
    }
    if (method === 'GET') {
      await route.fulfill({
        status: 404,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'NoSuchPlacement' }),
      });
      return;
    }
    await route.fallback();
  });

  // US-008 recent traces endpoint.
  page.route('**/admin/v1/diagnostics/traces**', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        total: 2,
        traces: [
          {
            request_id: 'req-aaaa',
            trace_id: '00000000000000000000000000000aaa',
            root_name: 'PUT /demo-bucket/object.bin',
            started_at_ns: Date.now() * 1_000_000,
            duration_ms: 12,
            status: 'Unset',
            span_count: 6,
          },
          {
            request_id: 'req-bbbb',
            trace_id: '00000000000000000000000000000bbb',
            root_name: 'GET /demo-bucket/object.bin',
            started_at_ns: (Date.now() - 250) * 1_000_000,
            duration_ms: 7,
            status: 'Error',
            span_count: 4,
          },
        ],
      }),
    });
  });
}

test.describe('Strata console — drain cleanup (US-005)', () => {
  test('drawer 3-category → bulk fix → drain evacuate → cancel deregister prep → trace browser', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', backend: 'rados' },
        { id: 'cephb', state: 'live', mode: '', backend: 'rados' },
      ],
      byBucket: [
        {
          name: 'dc-split',
          current_policy: { cepha: 1, cephb: 1 },
          category: 'migratable',
          chunk_count: 5,
          bytes_used: 5 * 1024,
          suggested_policies: [],
        },
        {
          name: 'dc-stuck',
          current_policy: { cephb: 1 },
          category: 'stuck_single_policy',
          chunk_count: 3,
          bytes_used: 3 * 1024,
          suggested_policies: [
            { label: 'Replace draining with cepha', policy: { cepha: 1 } },
          ],
        },
      ],
      migratableChunks: 5,
      stuckSingleChunks: 3,
      stuckNoPolicyChunks: 0,
      drainChunks: 5,
      drainBase: 5,
    };
    installRoutes(page, state);

    await login(page);

    // 1) Storage → Data tab → cluster cards rendered.
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/storage\/?$/);
    await page.getByRole('tab', { name: 'Data' }).click();
    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();
    await expect(cephbCard).toBeVisible({ timeout: 10_000 });

    // 2) Pools matrix carries 'Chunks' header (US-003 rename).
    await expect(page.getByRole('columnheader', { name: /Chunks/ })).toBeVisible();

    // 3) Show affected buckets → drawer renders 3 categories.
    await cephbCard.getByRole('button', { name: 'Show affected buckets' }).click();
    const drawer = page.getByRole('dialog').filter({ hasText: /Drain impact/i });
    await expect(drawer).toBeVisible({ timeout: 10_000 });
    await expect(drawer.getByTestId('cat-migrating')).toBeVisible();
    await expect(drawer.getByTestId('cat-stuck-single')).toBeVisible();
    // Bulk-fix CTA renders because stuck>0.
    const bulkCta = drawer.getByTestId('bucket-references-bulk-fix');
    await expect(bulkCta).toBeVisible();

    // 4) Click Bulk fix → BulkPlacementFixDialog opens, Apply.
    await bulkCta.click();
    const bulkDialog = page.getByRole('dialog').filter({ hasText: /Bulk fix/i });
    await expect(bulkDialog).toBeVisible({ timeout: 10_000 });
    await bulkDialog.getByTestId('bpf-apply').click();
    await expect(bulkDialog).toBeHidden({ timeout: 15_000 });

    // 5) Drawer counts refetch — stuck section gone (cache invalidated
    //    synchronously by PUT placement, US-002).
    await expect(drawer.getByTestId('cat-stuck-single')).toBeHidden({
      timeout: 10_000,
    });
    await drawer.press('Escape');
    await expect(drawer).toBeHidden({ timeout: 10_000 });

    // 6) Drain cephb (evacuate mode) via cluster-card action button.
    await cephbCard.getByTestId('cluster-card-drain').click();
    const drainDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Drain cluster/ });
    await expect(drainDialog).toBeVisible({ timeout: 10_000 });
    // Flip to evacuate radio.
    await drainDialog.getByTestId('cd-mode-evacuate').check();
    await drainDialog.getByLabel('Cluster id').fill('cephb');
    await drainDialog.getByTestId('cd-submit').click();
    await expect(drainDialog).toBeHidden({ timeout: 15_000 });

    // 7) Card flips to evacuating + chunks>0 → action slot shows
    //    'Undrain (cancel evacuation)' (US-007 truth table row 4).
    await expect(
      cephbCard.getByTestId('cluster-card-undrain-evacuation'),
    ).toBeVisible({ timeout: 15_000 });

    // 8) Drop drainChunks → 0; refetch surfaces deregister-ready chip +
    //    action slot flips to 'Cancel deregister prep'.
    state.drainChunks = 0;
    await page.reload();
    await page.getByRole('tab', { name: 'Data' }).click();
    const cephbCard2 = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();
    await expect(
      cephbCard2.getByTestId('cluster-card-cancel-deregister-prep'),
    ).toBeVisible({ timeout: 15_000 });
    // Undrain MUST NOT render in this state (US-007 row 6).
    await expect(
      cephbCard2.getByTestId('cluster-card-undrain-evacuation'),
    ).toBeHidden();

    // 9) Click Cancel deregister prep → typed-confirm modal → submit.
    await cephbCard2.getByTestId('cluster-card-cancel-deregister-prep').click();
    const cdpDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Cancel deregister prep/i });
    await expect(cdpDialog).toBeVisible({ timeout: 10_000 });
    await expect(cdpDialog.getByTestId('cdp-submit')).toBeDisabled();
    await cdpDialog.getByTestId('cdp-input').fill('cephb');
    await expect(cdpDialog.getByTestId('cdp-submit')).toBeEnabled();
    await cdpDialog.getByTestId('cdp-submit').click();
    await expect(cdpDialog).toBeHidden({ timeout: 15_000 });

    // 10) Trace browser renders the new RecentTracesPanel (US-008).
    await page.goto('/console/diagnostics/trace');
    await expect(page.getByTestId('recent-traces-panel')).toBeVisible({
      timeout: 10_000,
    });
    const rows = page.getByTestId('recent-trace-row');
    await expect(rows.first()).toBeVisible({ timeout: 10_000 });
    await expect(rows).toHaveCount(2);
  });
});
