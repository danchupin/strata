import { expect, test, type Page, type Route } from '@playwright/test';

// US-007 drain-lifecycle — UI half of the operator journey from
// tasks/prd-drain-lifecycle.md. Mirrors web/e2e/placement.spec.ts's pattern
// of spoofing the gateway via page.route() so the spec runs against the
// shared memory-mode webServer without needing a multi-cluster RADOS lab.
//
// Covers: cluster strict chip (US-004) → bucket-references drawer (US-006) →
// drain modal with "<N> buckets reference" info row (US-006) → progress bar
// (US-004) → deregister-ready chip (US-004) → policy-drain-warning chip on
// the Bucket Placement tab (US-006).

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

interface ClusterRow {
  id: string;
  state: 'live' | 'draining' | 'removed';
  backend: 'rados' | 's3' | 'memory';
}

interface BucketRef {
  name: string;
  weight: number;
  chunk_count: number;
  bytes_used: number;
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

test.describe('Strata console — drain lifecycle (US-007)', () => {
  test('drain walkthrough: refs drawer → confirm modal → progress bar → deregister chip → policy-drain warning', async ({
    page,
  }) => {
    // Closure state — handlers mutate; refetch surfaces the change.
    const clusters: ClusterRow[] = [
      { id: 'cepha', state: 'live', backend: 'rados' },
      { id: 'cephb', state: 'live', backend: 'rados' },
    ];
    const drainStrict = true;
    let placement: Record<string, number> | null = null;
    // Progress shape per cluster. Start "draining" with 5 chunks; after the
    // operator clicks Drain we'll flip to draining + populate a non-zero
    // chunks_on_cluster; after a refetch we drop to 0 to exercise the
    // deregister-ready chip.
    let progressChunks = 5;
    let progressBase = 5;

    const refsByCluster: Record<string, BucketRef[]> = {
      cepha: [
        {
          name: 'demo-split',
          weight: 1,
          chunk_count: 12,
          bytes_used: 256 * 1024,
        },
      ],
      cephb: [
        {
          name: 'demo-split',
          weight: 1,
          chunk_count: 12,
          bytes_used: 256 * 1024,
        },
        {
          name: 'demo-cephb-only',
          weight: 1,
          chunk_count: 4,
          bytes_used: 64 * 1024,
        },
      ],
    };

    // ── /admin/v1/clusters ───────────────────────────────────────────
    await page.route('**/admin/v1/clusters', async (route) => {
      if (route.request().method() !== 'GET') return route.fallback();
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ clusters, drain_strict: drainStrict }),
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

    // ── /admin/v1/clusters/{id}/drain-progress ───────────────────────
    await page.route(
      '**/admin/v1/clusters/*/drain-progress',
      async (route) => {
        const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-progress/);
        const id = m?.[1] ?? '';
        const row = clusters.find((c) => c.id === id);
        if (!row || row.state !== 'draining') {
          await route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({
              state: row?.state ?? 'live',
              chunks_on_cluster: null,
              bytes_on_cluster: null,
              base_chunks_at_start: null,
              last_scan_at: null,
              eta_seconds: null,
              deregister_ready: null,
              warnings: [],
            }),
          });
          return;
        }
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            state: 'draining',
            chunks_on_cluster: progressChunks,
            bytes_on_cluster: progressChunks * 1024,
            base_chunks_at_start: progressBase,
            last_scan_at: new Date().toISOString(),
            eta_seconds: progressChunks > 0 ? 120 : 0,
            deregister_ready: progressChunks === 0,
            warnings: [],
          }),
        });
      },
    );

    // ── /admin/v1/clusters/{id}/bucket-references ───────────────────
    await page.route(
      '**/admin/v1/clusters/*/bucket-references**',
      async (route) => {
        const url = new URL(route.request().url());
        const m = url.pathname.match(/\/clusters\/([^/]+)\/bucket-references/);
        const id = m?.[1] ?? '';
        const list = refsByCluster[id] ?? [];
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            buckets: list,
            total_buckets: list.length,
            next_offset: null,
          }),
        });
      },
    );

    // ── /admin/v1/storage/data ──────────────────────────────────────
    await page.route('**/admin/v1/storage/data', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          backend: 'rados',
          pools: [
            {
              name: 'strata-hot',
              class: 'STANDARD',
              cluster: 'cepha',
              bytes_used: 1024 * 1024,
              object_count: 4,
              num_replicas: 3,
              state: 'active+clean',
            },
            {
              name: 'strata-hot',
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

    // ── /admin/v1/buckets/{name}/placement ──────────────────────────
    await page.route('**/admin/v1/buckets/*/placement', async (route: Route) => {
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
    });

    // ── Flow ─────────────────────────────────────────────────────────
    await login(page);

    // (1) Storage page — strict chip rendered alongside state badge.
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/storage\/?$/);
    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(page.locator('span[title="cephb"]').first()).toBeVisible({
      timeout: 10_000,
    });
    // strict-mode badge is global (US-004).
    await expect(page.getByText('strict').first()).toBeVisible({
      timeout: 10_000,
    });

    // (2) "Show affected buckets" link opens the drawer — drawer enumerates
    // demo-split + demo-cephb-only for cephb.
    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();
    await cephbCard.getByRole('button', { name: 'Show affected buckets' }).click();
    const drawer = page.getByRole('dialog').filter({
      hasText: /Buckets referencing/i,
    });
    await expect(drawer).toBeVisible({ timeout: 10_000 });
    await expect(drawer.getByText('demo-split')).toBeVisible();
    await expect(drawer.getByText('demo-cephb-only')).toBeVisible();
    await drawer.press('Escape');
    await expect(drawer).toBeHidden({ timeout: 10_000 });

    // (3) Drain cephb via ConfirmDrainModal — the modal's info row should
    // surface "<N> buckets reference this cluster".
    await cephbCard.getByRole('button', { name: 'Drain' }).click();
    const drainDialog = page.getByRole('dialog').filter({
      hasText: /Drain cluster/,
    });
    await expect(drainDialog).toBeVisible();
    await expect(
      drainDialog.getByText(/buckets reference/i),
    ).toBeVisible({ timeout: 10_000 });
    // Typed-confirmation flow.
    const drainSubmit = drainDialog.getByRole('button', { name: 'Drain' });
    await expect(drainSubmit).toBeDisabled();
    await drainDialog.getByLabel('Cluster id').fill('cephb');
    await expect(drainSubmit).toBeEnabled();
    await drainSubmit.click();
    await expect(drainDialog).toBeHidden({ timeout: 10_000 });

    // (4) Card flips to draining state; progress bar shows "chunks remaining".
    await expect(
      page.getByText(/chunks remaining/).first(),
    ).toBeVisible({ timeout: 15_000 });

    // (5) Drop progress to zero — refetch surfaces deregister-ready chip.
    progressChunks = 0;
    // Force a refetch by re-opening the page (TanStack staleTime keeps the
    // value; navigation cold-starts the query).
    await page.reload();
    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(
      page.getByText(/Ready to deregister/).first(),
    ).toBeVisible({ timeout: 15_000 });

    // (6) Bucket Placement tab — saving a policy that ONLY references the
    // draining cephb cluster surfaces the policy-drain-warning chip.
    const bucket = `e2e-dl-${Date.now()}`;
    await createBucketViaUI(page, bucket);
    await page.getByRole('link', { name: bucket, exact: true }).click();
    await page.getByRole('tab', { name: 'Placement' }).click();
    // cepha is still live; bring it to 0 and cephb to 100.
    const cephbSlider = page.getByLabel('weight for cephb');
    await cephbSlider.fill('100');
    await page.getByRole('button', { name: 'Save placement' }).click();
    await expect(
      page.getByText('Placement policy updated').first(),
    ).toBeVisible({ timeout: 10_000 });
    // policy-drain-warning chip renders from the saved policy + draining set.
    await expect(
      page.getByTestId('policy-drain-warning'),
    ).toBeVisible({ timeout: 10_000 });
  });
});
