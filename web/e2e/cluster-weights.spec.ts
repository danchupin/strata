import { expect, test, type Page, type Route } from '@playwright/test';

// US-005 cluster-weights — UI half of the operator journey defined by
// scripts/smoke-cluster-weights.sh. Three scenarios drive the
// ActivateClusterModal (typed-confirm + initial-weight slider) and the
// LiveClusterWeightSlider (debounced PUT + 4xx revert + weight=0 chip).
//
// The memory-mode webServer has no registered RADOS clusters and never
// boots the boot-time reconcile against real metadata. Every admin
// endpoint touched by this spec is therefore spoofed via page.route() —
// same pattern as drain-transparency.spec.ts and drain-lifecycle.spec.ts.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

type ClusterState =
  | 'pending'
  | 'live'
  | 'draining_readonly'
  | 'evacuating'
  | 'removed';
type ClusterMode = '' | 'readonly' | 'evacuate';

interface ClusterRow {
  id: string;
  state: ClusterState;
  mode: ClusterMode;
  weight: number;
  backend: 'rados' | 's3' | 'memory';
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
  weightPuts: { id: string; weight: number; ts: number }[];
  // When set, weight PUTs return this HTTP code instead of 200. The handler
  // resets it back to 0 after one rejection so subsequent PUTs succeed.
  rejectWeightPutsWith: number;
}

function makeSpoof(clusters: ClusterRow[]): SpoofState {
  return {
    clusters,
    weightPuts: [],
    rejectWeightPutsWith: 0,
  };
}

function installRoutes(page: Page, state: SpoofState) {
  // /admin/v1/clusters
  page.route('**/admin/v1/clusters', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ clusters: state.clusters }),
    });
  });

  // /admin/v1/clusters/{id}/activate
  page.route('**/admin/v1/clusters/*/activate', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    const m = route.request().url().match(/\/clusters\/([^/]+)\/activate/);
    const id = m?.[1] ?? '';
    const body = JSON.parse(route.request().postData() || '{}') as {
      weight?: number;
    };
    const row = state.clusters.find((c) => c.id === id);
    if (!row) {
      await route.fulfill({
        status: 404,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'NoSuchCluster' }),
      });
      return;
    }
    if (row.state !== 'pending') {
      await route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({
          code: 'InvalidTransition',
          message: `cluster ${id} not in pending state`,
        }),
      });
      return;
    }
    if (
      typeof body.weight !== 'number' ||
      body.weight < 0 ||
      body.weight > 100
    ) {
      await route.fulfill({
        status: 400,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'BadRequest' }),
      });
      return;
    }
    row.state = 'live';
    row.weight = Math.round(body.weight);
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ state: row.state, weight: row.weight }),
    });
  });

  // /admin/v1/clusters/{id}/weight
  page.route('**/admin/v1/clusters/*/weight', async (route) => {
    if (route.request().method() !== 'PUT') return route.fallback();
    const m = route.request().url().match(/\/clusters\/([^/]+)\/weight/);
    const id = m?.[1] ?? '';
    const body = JSON.parse(route.request().postData() || '{}') as {
      weight?: number;
    };
    state.weightPuts.push({
      id,
      weight: typeof body.weight === 'number' ? body.weight : -1,
      ts: Date.now(),
    });
    if (state.rejectWeightPutsWith > 0) {
      const code = state.rejectWeightPutsWith;
      state.rejectWeightPutsWith = 0;
      await route.fulfill({
        status: code,
        contentType: 'application/json',
        body: JSON.stringify({
          code: code === 409 ? 'InvalidTransition' : 'BadRequest',
          message: `weight rejected with ${code}`,
        }),
      });
      return;
    }
    const row = state.clusters.find((c) => c.id === id);
    if (row && row.state === 'live' && typeof body.weight === 'number') {
      row.weight = Math.round(body.weight);
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ weight: row?.weight ?? 0 }),
    });
  });

  // /admin/v1/clusters/{id}/rebalance-progress — quiet
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

  // /admin/v1/clusters/{id}/drain-progress — quiet (no draining clusters)
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

  // /admin/v1/storage/data — minimal pool shape so ClustersSubsection mounts
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
}

async function gotoStorageData(page: Page, cephbVisible = true) {
  await page.getByRole('link', { name: 'Storage', exact: true }).click();
  await page.getByRole('tab', { name: 'Data' }).click();
  if (cephbVisible) {
    await expect(page.locator('span[title="cephb"]').first()).toBeVisible({
      timeout: 10_000,
    });
  }
}

test.describe('Strata console — cluster weights (US-005)', () => {
  test('scenario-A: pending card → Activate modal → typed-confirm + slider=25 → flips to live', async ({
    page,
  }) => {
    const state = makeSpoof([
      { id: 'cepha', state: 'live', mode: '', weight: 100, backend: 'rados' },
      { id: 'cephb', state: 'pending', mode: '', weight: 0, backend: 'rados' },
    ]);
    installRoutes(page, state);

    await login(page);
    await gotoStorageData(page);

    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();

    // Pending-state badge label.
    await expect(
      cephbCard.getByText(/Pending — not receiving writes/i),
    ).toBeVisible();

    // Activate button replaces the Drain CTA on pending cards.
    const activateBtn = cephbCard.getByTestId('cluster-card-activate');
    await expect(activateBtn).toBeVisible();
    await activateBtn.click();

    const dialog = page
      .getByRole('dialog')
      .filter({ hasText: /Activate cluster/ });
    await expect(dialog).toBeVisible({ timeout: 10_000 });

    // Drag slider to 25 via the numeric input (slider is paired two-way).
    const numericInput = dialog.getByTestId('ac-weight-input');
    await numericInput.fill('25');
    await numericInput.blur();

    // Submit disabled until typed-confirm matches.
    const submit = dialog.getByTestId('ac-submit');
    await expect(submit).toBeDisabled();
    await dialog.getByTestId('ac-confirm-input').fill('cephb');
    await expect(submit).toBeEnabled();
    // Button label bakes the chosen weight in.
    await expect(submit).toHaveText(/Activate \(weight 25\)/);
    await submit.click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    // Card flips to live; weight slider (US-004 component) mounts inline.
    await expect(
      cephbCard.getByTestId('cluster-card-weight-slider'),
    ).toBeVisible({ timeout: 10_000 });
    // The Drain CTA replaces the Activate CTA on live cards.
    await expect(cephbCard.getByRole('button', { name: 'Drain' })).toBeVisible();
    await expect(
      cephbCard.getByTestId('cluster-card-activate'),
    ).toHaveCount(0);
    // The slider position reflects the chosen weight=25.
    const slider = cephbCard.getByTestId('cluster-card-weight-slider');
    await expect(slider).toHaveValue('25');
  });

  test('scenario-B: live card inline slider — drag debounces 500ms then PUTs final value, weight=0 chip renders', async ({
    page,
  }) => {
    const state = makeSpoof([
      { id: 'cepha', state: 'live', mode: '', weight: 100, backend: 'rados' },
      { id: 'cephb', state: 'live', mode: '', weight: 50, backend: 'rados' },
    ]);
    installRoutes(page, state);

    await login(page);
    await gotoStorageData(page);

    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();

    const slider = cephbCard.getByTestId('cluster-card-weight-slider');
    const numericInput = cephbCard.getByTestId('cluster-card-weight-input');
    await expect(slider).toBeVisible();

    // Rapid drags via numeric input: 60 → 70 → 80 must coalesce into one
    // PUT after 500ms with the final value 80.
    await numericInput.fill('60');
    await numericInput.fill('70');
    await numericInput.fill('80');

    // Wait past the debounce + grace.
    await page.waitForTimeout(900);

    // Exactly one PUT recorded for cephb in the burst window, value 80.
    const cephbPuts = state.weightPuts.filter((p) => p.id === 'cephb');
    expect(cephbPuts.length).toBeGreaterThanOrEqual(1);
    expect(cephbPuts[cephbPuts.length - 1]?.weight).toBe(80);

    // Now drag to 0 → weight=0 chip mounts.
    await numericInput.fill('0');
    await page.waitForTimeout(900);
    await expect(
      cephbCard.getByTestId('cluster-card-weight-zero-chip'),
    ).toBeVisible({ timeout: 5_000 });
  });

  test('scenario-C: 409 from weight PUT reverts slider to last-accepted value', async ({
    page,
  }) => {
    const state = makeSpoof([
      { id: 'cepha', state: 'live', mode: '', weight: 100, backend: 'rados' },
      { id: 'cephb', state: 'live', mode: '', weight: 25, backend: 'rados' },
    ]);
    installRoutes(page, state);

    await login(page);
    await gotoStorageData(page);

    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();

    // Arm the next PUT to fail with 409 (cluster drained mid-edit).
    state.rejectWeightPutsWith = 409;

    const numericInput = cephbCard.getByTestId('cluster-card-weight-input');
    await numericInput.fill('77');
    await page.waitForTimeout(900);

    // PUT was attempted with 77 and rejected → input reverts to 25
    // (the last server-accepted value).
    const cephbPuts = state.weightPuts.filter((p) => p.id === 'cephb');
    expect(cephbPuts[cephbPuts.length - 1]?.weight).toBe(77);
    await expect(numericInput).toHaveValue('25', { timeout: 5_000 });
  });
});
