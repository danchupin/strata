import { expect, test, type Page, type Route } from '@playwright/test';

// US-008 drain-transparency — UI half of the operator journey defined by
// scripts/smoke-drain-transparency.sh. Three scenarios drive the
// ConfirmDrainModal mode picker, /drain-impact analysis, the
// <BulkPlacementFixDialog>, and the redesigned <DrainProgressBar>.
//
// The memory-mode webServer has no registered RADOS clusters and no
// rebalance worker, so every admin endpoint touched by this spec is
// spoofed via page.route() — same pattern as placement.spec.ts and
// drain-lifecycle.spec.ts.

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

interface DrainProgressShape {
  state: ClusterState;
  mode: ClusterMode;
  chunks_on_cluster: number | null;
  bytes_on_cluster: number | null;
  base_chunks_at_start: number | null;
  last_scan_at: string | null;
  eta_seconds: number | null;
  deregister_ready: boolean | null;
  warnings: string[];
  migratable_chunks: number | null;
  stuck_single_policy_chunks: number | null;
  stuck_no_policy_chunks: number | null;
  by_bucket: { name: string; category: string; chunk_count: number; bytes_used: number }[];
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
  drainChunks: number;
  drainBase: number;
}

function installRoutes(page: Page, state: SpoofState) {
  // ── /admin/v1/clusters ────────────────────────────────────────────
  page.route('**/admin/v1/clusters', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ clusters: state.clusters }),
    });
  });

  // ── /admin/v1/clusters/{id}/drain | undrain ──────────────────────
  page.route('**/admin/v1/clusters/*/drain', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain/);
    const id = m?.[1];
    const row = state.clusters.find((c) => c.id === id);
    const body = JSON.parse(route.request().postData() || '{}') as {
      mode?: ClusterMode;
    };
    if (!body.mode) {
      await route.fulfill({
        status: 400,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'BadRequest', message: 'mode required' }),
      });
      return;
    }
    if (row) {
      if (body.mode === 'readonly') {
        row.state = 'draining_readonly';
        row.mode = 'readonly';
      } else if (body.mode === 'evacuate') {
        row.state = 'evacuating';
        row.mode = 'evacuate';
      }
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

  // ── /admin/v1/clusters/{id}/drain-impact ─────────────────────────
  page.route('**/admin/v1/clusters/*/drain-impact**', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-impact/);
    const id = m?.[1] ?? '';
    const row = state.clusters.find((c) => c.id === id);
    if (row && (row.state === 'evacuating' || row.state === 'removed')) {
      await route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({
          code: 'InvalidTransition',
          message: 'drain-impact unavailable for evacuating/removed',
          current_state: row.state,
        }),
      });
      return;
    }
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

  // ── /admin/v1/clusters/{id}/drain-progress ───────────────────────
  page.route('**/admin/v1/clusters/*/drain-progress', async (route) => {
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-progress/);
    const id = m?.[1] ?? '';
    const row = state.clusters.find((c) => c.id === id);
    const draining =
      row?.state === 'draining_readonly' || row?.state === 'evacuating';
    if (!draining) {
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
        } as DrainProgressShape),
      });
      return;
    }
    if (row?.state === 'draining_readonly') {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          state: 'draining_readonly',
          mode: 'readonly',
          chunks_on_cluster: null,
          bytes_on_cluster: null,
          base_chunks_at_start: null,
          last_scan_at: null,
          eta_seconds: null,
          deregister_ready: null,
          warnings: ['stop-writes mode — migration scan skipped'],
          migratable_chunks: null,
          stuck_single_policy_chunks: null,
          stuck_no_policy_chunks: null,
          by_bucket: [],
        } as DrainProgressShape),
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
      } as DrainProgressShape),
    });
  });

  // ── /admin/v1/clusters/{id}/rebalance-progress ───────────────────
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

  // ── /admin/v1/storage/data (rados shape so ClustersSubsection mounts) ─
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
            object_count: 4,
            num_replicas: 3,
            state: 'active+clean',
          },
          {
            name: 'strata-data-b',
            class: 'STANDARD',
            cluster: 'cephb',
            bytes_used: 2 * 1024 * 1024,
            object_count: 7,
            num_replicas: 3,
            state: 'active+clean',
          },
        ],
        warnings: [],
      }),
    });
  });

  // ── /admin/v1/buckets/*/placement ────────────────────────────────
  page.route('**/admin/v1/buckets/*/placement', async (route: Route) => {
    const method = route.request().method();
    if (method === 'PUT') {
      const body = JSON.parse(route.request().postData() || '{}') as {
        placement?: Record<string, number>;
      };
      // Treat any PUT against a stuck bucket as a successful fix —
      // collapse stuck_single counters so the modal refreshes to stuck=0
      // on its next refetch.
      const m = route
        .request()
        .url()
        .match(/\/admin\/v1\/buckets\/([^/]+)\/placement/);
      const name = m?.[1] ?? '';
      const idx = state.stuckBuckets.findIndex((b) => b.name === name);
      if (idx >= 0) {
        state.stuckSingleChunks = Math.max(
          0,
          state.stuckSingleChunks - state.stuckBuckets[idx].chunk_count,
        );
        state.stuckBuckets.splice(idx, 1);
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ placement: body.placement ?? {} }),
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
}

test.describe('Strata console — drain transparency (US-008)', () => {
  test('scenario-A: stop-writes drain → mode picker readonly → state flips', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', backend: 'rados' },
        { id: 'cephb', state: 'live', mode: '', backend: 'rados' },
      ],
      stuckBuckets: [],
      migratableChunks: 0,
      stuckSingleChunks: 0,
      stuckNoPolicyChunks: 0,
      drainChunks: 0,
      drainBase: 0,
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
    const dialog = page.getByRole('dialog').filter({ hasText: /Drain cluster/ });
    await expect(dialog).toBeVisible();

    // Readonly is the default; submit disabled until typed-confirm matches.
    await expect(dialog.getByTestId('cd-mode-readonly')).toBeChecked();
    const submit = dialog.getByTestId('cd-submit');
    await expect(submit).toBeDisabled();
    await dialog.getByLabel('Cluster id').fill('cephb');
    await expect(submit).toBeEnabled();
    await submit.click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    // Card flips to readonly drain — DrainProgressBar renders the readonly chip.
    await expect(page.getByTestId('dp-readonly').first()).toBeVisible({
      timeout: 10_000,
    });
  });

  test('scenario-B: evacuate flow — impact analysis blocks submit until bulk fix runs', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', backend: 'rados' },
        { id: 'cephb', state: 'live', mode: '', backend: 'rados' },
      ],
      stuckBuckets: [
        {
          name: 'tx-stuck',
          current_policy: { cephb: 1 },
          category: 'stuck_single_policy',
          chunk_count: 6,
          bytes_used: 6 * 1024,
          suggested_policies: [
            {
              label: 'Add all live clusters (uniform)',
              policy: { cepha: 1, cephb: 0 },
            },
            { label: 'Replace draining with cepha', policy: { cepha: 1 } },
          ],
        },
      ],
      migratableChunks: 12,
      stuckSingleChunks: 6,
      stuckNoPolicyChunks: 0,
      drainChunks: 12,
      drainBase: 12,
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
    const dialog = page.getByRole('dialog').filter({ hasText: /Drain cluster/ });
    await expect(dialog).toBeVisible();

    // Flip to evacuate; impact analysis fires.
    await dialog.getByTestId('cd-mode-evacuate').click();
    await expect(dialog.getByTestId('cd-impact')).toBeVisible({
      timeout: 10_000,
    });

    // Stuck warning panel + bulk-fix CTA render.
    await expect(dialog.getByTestId('cd-stuck-warning')).toBeVisible();
    const submit = dialog.getByTestId('cd-submit');
    // Even with typed-confirm matching, stuck>0 keeps submit disabled.
    await dialog.getByLabel('Cluster id').fill('cephb');
    await expect(submit).toBeDisabled();

    // Click bulk-fix → BulkPlacementFixDialog opens overlay-on-overlay.
    await dialog.getByTestId('cd-bulk-fix').click();
    const bulkDialog = page
      .getByRole('dialog')
      .filter({ hasText: /Fix bucket placement policies/ });
    await expect(bulkDialog).toBeVisible({ timeout: 10_000 });
    await expect(bulkDialog.getByTestId('bpf-apply')).toBeEnabled();
    await bulkDialog.getByTestId('bpf-apply').click();
    // Bulk dialog closes on forward progress.
    await expect(bulkDialog).toBeHidden({ timeout: 10_000 });

    // Parent modal refetches impact via TanStack invalidation — stuck=0,
    // submit text flips, button enables.
    await expect(dialog.getByTestId('cd-stuck-warning')).toBeHidden({
      timeout: 15_000,
    });
    await expect(submit).toBeEnabled({ timeout: 10_000 });

    // Submit evacuate → modal closes, cluster card flips to evacuating
    // (DrainProgressBar's evacuate render shape mounts).
    await submit.click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });
    await expect(page.getByTestId('dp-evacuate').first()).toBeVisible({
      timeout: 15_000,
    });

    // Drop drainChunks to 0 → deregister-ready chip renders.
    state.drainChunks = 0;
    await page.reload();
    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(page.getByTestId('dp-dereg-ready').first()).toBeVisible({
      timeout: 15_000,
    });
  });

  test('scenario-C: upgrade readonly → evacuate hides the readonly radio', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cepha', state: 'live', mode: '', backend: 'rados' },
        {
          id: 'cephb',
          state: 'draining_readonly',
          mode: 'readonly',
          backend: 'rados',
        },
      ],
      stuckBuckets: [],
      migratableChunks: 8,
      stuckSingleChunks: 0,
      stuckNoPolicyChunks: 0,
      drainChunks: 8,
      drainBase: 8,
    };
    installRoutes(page, state);

    await login(page);
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(page.locator('span[title="cephb"]').first()).toBeVisible({
      timeout: 10_000,
    });

    // The cephb card is already in readonly drain; DrainProgressBar renders
    // the readonly chip with an "Upgrade to evacuate" button (testid dp-upgrade).
    const upgradeBtn = page.getByTestId('dp-upgrade').first();
    await expect(upgradeBtn).toBeVisible({ timeout: 10_000 });
    await upgradeBtn.click();

    const dialog = page
      .getByRole('dialog')
      .filter({ hasText: /Upgrade to evacuate/ });
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    // Readonly radio is hidden — only evacuate option exists.
    await expect(dialog.getByTestId('cd-mode-readonly')).toHaveCount(0);
    await expect(dialog.getByTestId('cd-mode-evacuate')).toBeChecked();

    // Impact analysis fires (stuck=0 in this scenario).
    await expect(dialog.getByTestId('cd-impact')).toBeVisible({
      timeout: 10_000,
    });
    const submit = dialog.getByTestId('cd-submit');
    await dialog.getByLabel('Cluster id').fill('cephb');
    await expect(submit).toBeEnabled();
    await submit.click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });
    await expect(page.getByTestId('dp-evacuate').first()).toBeVisible({
      timeout: 15_000,
    });
  });
});
