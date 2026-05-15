import { expect, test, type Page } from '@playwright/test';

// US-003 drain-followup — cluster-card UI clarity after full evacuation.
//
// Asserts the renamed action button label, the green "Ready to deregister"
// chip tooltip text, the outline button variant, and the chip-above-button
// vertical layout (chip's bounding-box top is strictly above the button's
// bounding-box top). Spoofs admin routes via page.route() so the spec
// runs against the memory-mode webServer without needing the multi-cluster
// lab.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

interface ClusterRow {
  id: string;
  state: 'live' | 'draining_readonly' | 'evacuating' | 'removed';
  mode: '' | 'readonly' | 'evacuate';
  backend: 'rados';
}

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

function installRoutes(page: Page, clusters: ClusterRow[]) {
  page.route('**/admin/v1/clusters', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ clusters }),
    });
  });

  page.route('**/admin/v1/clusters/*/drain-progress', async (route) => {
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-progress/);
    const id = m?.[1] ?? '';
    const row = clusters.find((c) => c.id === id);
    if (!row || row.state !== 'evacuating') {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          state: row?.state ?? 'live',
          mode: row?.mode ?? '',
          chunks_on_cluster: null,
          deregister_ready: null,
          warnings: [],
          not_ready_reasons: [],
        }),
      });
      return;
    }
    // Fully evacuated, deregister_ready=true.
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        state: 'evacuating',
        mode: 'evacuate',
        chunks_on_cluster: 0,
        bytes_on_cluster: 0,
        base_chunks_at_start: 100,
        last_scan_at: new Date().toISOString(),
        eta_seconds: 0,
        deregister_ready: true,
        warnings: [],
        migratable_chunks: 0,
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

  page.route('**/admin/v1/clusters/*/drain-impact**', async (route) => {
    const m = route.request().url().match(/\/clusters\/([^/]+)\/drain-impact/);
    const id = m?.[1] ?? '';
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        cluster_id: id,
        current_state: 'evacuating',
        migratable_chunks: 0,
        stuck_single_policy_chunks: 0,
        stuck_no_policy_chunks: 0,
        total_chunks: 0,
        by_bucket: [],
        total_buckets: 0,
        next_offset: null,
        last_scan_at: new Date().toISOString(),
      }),
    });
  });

  page.route('**/admin/v1/storage/data', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        backend: 'rados',
        pools: [
          {
            name: 'strata-data-b',
            class: 'STANDARD',
            cluster: 'cephb',
            bytes_used: 0,
            chunk_count: 0,
            num_replicas: 3,
            state: 'active+clean',
          },
        ],
        warnings: [],
      }),
    });
  });
}

test.describe('Strata console — drain followup (US-003)', () => {
  test('evacuating + chunks=0 + deregister_ready cell: renamed button + chip tooltip + chip-above-button layout', async ({
    page,
  }) => {
    const clusters: ClusterRow[] = [
      { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
    ];
    installRoutes(page, clusters);

    await login(page);

    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/storage\/?$/);
    await page.getByRole('tab', { name: 'Data' }).click();

    const cephbCard = page
      .locator('[class*="relative"]')
      .filter({ has: page.locator('span[title="cephb"]') })
      .first();
    await expect(cephbCard).toBeVisible({ timeout: 10_000 });

    // 1) Renamed button label + outline variant.
    const button = cephbCard.getByTestId('cluster-card-cancel-deregister-prep');
    await expect(button).toBeVisible({ timeout: 10_000 });
    await expect(button).toHaveText(/Restore to live \(cancel evacuation\)/);
    // outline variant has a transparent/background-less surface; assert the
    // class list contains the outline marker rather than destructive.
    const className = (await button.getAttribute('class')) ?? '';
    expect(className).not.toMatch(/destructive/);
    expect(className).toMatch(/border-input|border/);

    // 2) Green chip tooltip text matches the PRD copy.
    const chip = cephbCard.getByTestId('dp-dereg-ready');
    await expect(chip).toBeVisible();
    await expect(chip).toHaveAttribute(
      'title',
      'Edit STRATA_RADOS_CLUSTERS env to remove this cluster, then rolling restart. See operator runbook for deregister procedure.',
    );

    // 3) Chip-above-button layout — chip's top edge strictly above button's top edge.
    const chipBox = await chip.boundingBox();
    const buttonBox = await button.boundingBox();
    expect(chipBox).not.toBeNull();
    expect(buttonBox).not.toBeNull();
    if (chipBox && buttonBox) {
      expect(chipBox.y).toBeLessThan(buttonBox.y);
    }
  });
});
