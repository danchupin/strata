import { expect, test, type Page, type Route } from '@playwright/test';

// US-006 placement-ui — full operator flow against the in-memory gateway
// (single binary on :9999 via playwright.config.ts webServer,
// STRATA_META_BACKEND=memory + STRATA_DATA_BACKEND=memory + auth off).
//
// Memory mode has no registered clusters and no rebalance worker, so the
// spec installs page.route() handlers up-front that:
//   - return a synthetic two-cluster topology (`cepha`, `cephb`) on
//     `GET /admin/v1/clusters`,
//   - flip a closure-state row to "draining" on
//     `POST /admin/v1/clusters/{id}/drain` and clear it on `.../undrain`,
//   - return a `DataHealthReport{backend=rados, pools=[...]}` from
//     `GET /admin/v1/storage/data` so `<ClustersSubsection>` mounts
//     (it short-circuits on memory),
//   - persist the per-bucket placement policy round-trip via
//     `GET|PUT|DELETE /admin/v1/buckets/{name}/placement`,
//   - return a metrics-unavailable rebalance-progress payload.
//
// This mirrors `storage.spec.ts`'s precedent of spoofing via the network
// layer rather than seeding the gateway via env — keeps the shared
// webServer block clean.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

interface ClusterRow {
  id: string;
  state: 'live' | 'draining' | 'removed';
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

async function createBucketViaUI(page: Page, name: string) {
  await page.goto('/console/buckets');
  await page.getByRole('button', { name: /^Create$/ }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel(/^Name$/i).fill(name);
  await dialog.getByRole('button', { name: 'Create bucket' }).click();
  await expect(dialog).toBeHidden({ timeout: 10_000 });
  await expect(
    page.getByRole('link', { name, exact: true }),
  ).toBeVisible({ timeout: 10_000 });
}

test.describe('Strata console — placement + cluster surfacing (US-006)', () => {
  test('placement-flow: clusters → slider save → drain → banner → undrain → reset', async ({
    page,
  }) => {
    // Closure state — handlers mutate; the next refetch surfaces the
    // change. Two clusters so the Drain/Undrain pair has a remaining
    // live cluster to keep new PUTs routable.
    const clusters: ClusterRow[] = [
      { id: 'cepha', state: 'live', backend: 'rados' },
      { id: 'cephb', state: 'live', backend: 'rados' },
    ];
    let placement: Record<string, number> | null = null;

    // ── /admin/v1/clusters ───────────────────────────────────────────
    await page.route('**/admin/v1/clusters', async (route) => {
      if (route.request().method() !== 'GET') return route.fallback();
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ clusters }),
      });
    });

    // ── /admin/v1/clusters/{id}/drain | undrain ──────────────────────
    await page.route('**/admin/v1/clusters/*/drain', async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      const m = route.request().url().match(/\/clusters\/([^/]+)\/drain/);
      const id = m?.[1];
      const row = clusters.find((c) => c.id === id);
      if (row) row.state = 'draining';
      await route.fulfill({ status: 204, body: '' });
    });
    await page.route('**/admin/v1/clusters/*/undrain', async (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      const m = route.request().url().match(/\/clusters\/([^/]+)\/undrain/);
      const id = m?.[1];
      const row = clusters.find((c) => c.id === id);
      if (row) row.state = 'live';
      await route.fulfill({ status: 204, body: '' });
    });

    // ── /admin/v1/clusters/{id}/rebalance-progress ───────────────────
    // Chip handles metrics_available=false explicitly with
    // "(metrics unavailable)" — exercise that branch so the test
    // doesn't depend on a Prometheus wired into the e2e gateway.
    await page.route(
      '**/admin/v1/clusters/*/rebalance-progress',
      async (route) => {
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
      },
    );

    // ── /admin/v1/storage/data (rados shape so ClustersSubsection mounts) ─
    await page.route('**/admin/v1/storage/data', async (route) => {
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

    // ── /admin/v1/buckets/{name}/placement (GET/PUT/DELETE) ──────────
    await page.route(
      '**/admin/v1/buckets/*/placement',
      async (route: Route) => {
        const method = route.request().method();
        if (method === 'GET') {
          if (!placement) {
            await route.fulfill({
              status: 404,
              contentType: 'application/json',
              body: JSON.stringify({
                code: 'NoSuchPlacement',
                message: 'placement policy not configured',
              }),
            });
            return;
          }
          await route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({ placement }),
          });
          return;
        }
        if (method === 'PUT') {
          const body = JSON.parse(route.request().postData() || '{}') as {
            placement?: Record<string, number>;
          };
          placement = body.placement ?? {};
          await route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({ placement }),
          });
          return;
        }
        if (method === 'DELETE') {
          placement = null;
          await route.fulfill({ status: 204, body: '' });
          return;
        }
        await route.fallback();
      },
    );

    // ── Flow ─────────────────────────────────────────────────────────
    await login(page);

    // (1) Storage page renders ≥1 cluster card from the spoofed list.
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/storage\/?$/);
    await page.getByRole('tab', { name: 'Data' }).click();
    // CardTitle renders as <div> not a heading — match on the literal text.
    await expect(
      page.getByText('Clusters', { exact: true }).first(),
    ).toBeVisible({ timeout: 10_000 });
    await expect(page.locator('span[title="cepha"]').first()).toBeVisible();
    await expect(page.locator('span[title="cephb"]').first()).toBeVisible();
    // Rebalance chip falls back to "(metrics unavailable)" copy per
    // graceful-degrade branch.
    await expect(
      page.getByText(/metrics unavailable/i).first(),
    ).toBeVisible({ timeout: 10_000 });

    // (2) Create a bucket via UI + open Placement tab + drag cephb to 100.
    const bucket = `e2e-pl-${Date.now()}`;
    await createBucketViaUI(page, bucket);
    await page.getByRole('link', { name: bucket, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/console/buckets/${bucket}/?$`));
    await page.getByRole('tab', { name: 'Placement' }).click();
    await expect(
      page.getByText(/Default routing \(no per-bucket policy\)/i),
    ).toBeVisible({ timeout: 10_000 });

    const cephbSlider = page.getByLabel('weight for cephb');
    await expect(cephbSlider).toBeVisible({ timeout: 10_000 });
    await cephbSlider.fill('100');
    // Numeric pair sticks to the slider value.
    await expect(page.getByLabel('weight input for cephb')).toHaveValue('100');

    await page.getByRole('button', { name: 'Save placement' }).click();
    await expect(
      page.getByText('Placement policy updated').first(),
    ).toBeVisible({ timeout: 10_000 });
    // Round-trip the closure state through the PUT handler.
    expect(placement).toEqual({ cephb: 100 });

    // (3) Storage → Drain cepha via typed-confirmation modal.
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await page.getByRole('tab', { name: 'Data' }).click();
    // Card with cepha id — the Drain button shares a card with the id label.
    const cephaCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cepha"]') })
      .first();
    await cephaCard.getByRole('button', { name: 'Drain' }).click();
    const drainDialog = page.getByRole('dialog');
    await expect(drainDialog).toBeVisible();
    const drainSubmit = drainDialog.getByRole('button', { name: 'Drain' });
    await expect(drainSubmit).toBeDisabled();
    // Mistype: button stays disabled.
    await drainDialog.getByLabel('Cluster id').fill('cephx');
    await expect(drainSubmit).toBeDisabled();
    // Correct id arms the button.
    await drainDialog.getByLabel('Cluster id').fill('cepha');
    await expect(drainSubmit).toBeEnabled();
    await drainSubmit.click();
    await expect(drainDialog).toBeHidden({ timeout: 10_000 });

    // (4) Banner appears (PlacementDrainBanner mounted in AppShell).
    const banner = page.getByRole('alert').filter({
      has: page.getByText(/Draining cluster\(s\): cepha\./),
    });
    await expect(banner).toBeVisible({ timeout: 10_000 });

    // (5) Undrain — banner gone after refetch.
    const cephaCardAfter = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cepha"]') })
      .first();
    await cephaCardAfter
      .getByRole('button', { name: 'Undrain' })
      .click();
    await expect(
      page.getByText(/Cluster cepha undrained/),
    ).toBeVisible({ timeout: 10_000 });
    await expect(banner).toHaveCount(0, { timeout: 10_000 });

    // (6) Reset to default on the Placement tab.
    await page.goto(`/console/buckets/${bucket}`);
    await page.getByRole('tab', { name: 'Placement' }).click();
    // Slider for cephb still shows the saved value before reset.
    await expect(page.getByLabel('weight input for cephb')).toHaveValue('100', {
      timeout: 10_000,
    });
    await page.getByRole('button', { name: /Reset to default/ }).click();
    const resetDialog = page.getByRole('dialog');
    await expect(resetDialog).toBeVisible();
    await resetDialog
      .getByRole('button', { name: 'Reset', exact: true })
      .click();
    await expect(resetDialog).toBeHidden({ timeout: 10_000 });
    expect(placement).toBeNull();
    await expect(
      page.getByText(/Default routing \(no per-bucket policy\)/i),
    ).toBeVisible({ timeout: 10_000 });
    await expect(page.getByLabel('weight input for cephb')).toHaveValue('0');
  });
});
