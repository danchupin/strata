import { expect, test, type Page } from '@playwright/test';

// US-002 drain-progress-physical — exercises the <DrainProgressBar>
// 3-state machine added by surfacing physical pool state behind the
// admin /drain-progress probe (US-001). Three scenarios cover the
// states the operator sees during the drain lifecycle:
//
//   1. Migrating  — physical > 0 && manifest > 0 → "Migrating: X chunks remaining"
//   2. Awaiting GC — physical > 0 && manifest == 0 → amber chip + tooltip
//   3. Ready      — physical == 0 && manifest == 0 + deregister_ready
//
// Plus a fourth back-compat scenario where physical_chunks_on_cluster
// is null (S3 / memory backend) — manifest count is primary and the
// "(physical count unavailable on this backend)" tooltip renders.
//
// The memory-mode webServer has no rebalance worker, so every admin
// endpoint touched by this spec is spoofed via page.route() — same
// pattern as drain-transparency.spec.ts.

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

interface DrainProgressShape {
  state: ClusterState;
  mode: ClusterMode;
  chunks_on_cluster: number | null;
  bytes_on_cluster: number | null;
  base_chunks_at_start: number | null;
  last_scan_at: string | null;
  eta_seconds: number | null;
  deregister_ready: boolean | null;
  not_ready_reasons?: string[];
  warnings: string[];
  migratable_chunks: number | null;
  stuck_single_policy_chunks: number | null;
  stuck_no_policy_chunks: number | null;
  physical_chunks_on_cluster: number | null;
  physical_bytes_on_cluster: number | null;
  gc_queue_pending: number;
  by_bucket: {
    name: string;
    category: string;
    chunk_count: number;
    bytes_used: number;
  }[];
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
  progress: DrainProgressShape;
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

  page.route('**/admin/v1/clusters/*/drain-progress', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(state.progress),
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
}

function migratingProgress(): DrainProgressShape {
  return {
    state: 'evacuating',
    mode: 'evacuate',
    chunks_on_cluster: 200,
    bytes_on_cluster: 200 * 1024 * 1024,
    base_chunks_at_start: 300,
    last_scan_at: new Date().toISOString(),
    eta_seconds: 120,
    deregister_ready: false,
    not_ready_reasons: ['chunks_remaining'],
    warnings: [],
    migratable_chunks: 200,
    stuck_single_policy_chunks: 0,
    stuck_no_policy_chunks: 0,
    physical_chunks_on_cluster: 250,
    physical_bytes_on_cluster: 250 * 1024 * 1024,
    gc_queue_pending: 50,
    by_bucket: [],
  };
}

function awaitingGCProgress(): DrainProgressShape {
  return {
    state: 'evacuating',
    mode: 'evacuate',
    chunks_on_cluster: 0,
    bytes_on_cluster: 0,
    base_chunks_at_start: 300,
    last_scan_at: new Date().toISOString(),
    eta_seconds: null,
    deregister_ready: false,
    not_ready_reasons: ['gc_queue_pending'],
    warnings: [],
    migratable_chunks: 0,
    stuck_single_policy_chunks: 0,
    stuck_no_policy_chunks: 0,
    physical_chunks_on_cluster: 80,
    physical_bytes_on_cluster: 80 * 1024 * 1024,
    gc_queue_pending: 80,
    by_bucket: [],
  };
}

function readyProgress(): DrainProgressShape {
  return {
    state: 'evacuating',
    mode: 'evacuate',
    chunks_on_cluster: 0,
    bytes_on_cluster: 0,
    base_chunks_at_start: 300,
    last_scan_at: new Date().toISOString(),
    eta_seconds: 0,
    deregister_ready: true,
    not_ready_reasons: [],
    warnings: [],
    migratable_chunks: 0,
    stuck_single_policy_chunks: 0,
    stuck_no_policy_chunks: 0,
    physical_chunks_on_cluster: 0,
    physical_bytes_on_cluster: 0,
    gc_queue_pending: 0,
    by_bucket: [],
  };
}

function nullPhysicalProgress(): DrainProgressShape {
  return {
    state: 'evacuating',
    mode: 'evacuate',
    chunks_on_cluster: 150,
    bytes_on_cluster: 150 * 1024 * 1024,
    base_chunks_at_start: 300,
    last_scan_at: new Date().toISOString(),
    eta_seconds: 240,
    deregister_ready: false,
    not_ready_reasons: ['chunks_remaining'],
    warnings: [],
    migratable_chunks: 150,
    stuck_single_policy_chunks: 0,
    stuck_no_policy_chunks: 0,
    physical_chunks_on_cluster: null,
    physical_bytes_on_cluster: null,
    gc_queue_pending: 0,
    by_bucket: [],
  };
}

async function gotoStorageData(page: Page) {
  await login(page);
  await page.getByRole('link', { name: 'Storage', exact: true }).click();
  await page.getByRole('tab', { name: 'Data' }).click();
  await expect(page.locator('span[title="cephb"]').first()).toBeVisible({
    timeout: 10_000,
  });
}

test.describe('Strata console — drain-progress 3-state machine (US-002)', () => {
  test('scenario-A: Migrating — physical primary + chunks remaining headline', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: migratingProgress(),
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const bar = page.getByTestId('dp-evacuate').first();
    await expect(bar).toBeVisible({ timeout: 10_000 });
    await expect(bar.getByTestId('dp-primary-count')).toHaveText('250');
    await expect(bar.getByTestId('dp-summary')).toContainText(
      'Migrating: 250 chunks remaining',
    );
    // Collapsible detail mounts collapsed with manifest/gc/bytes rows.
    await bar.getByTestId('dp-detail').locator('summary').click();
    await expect(bar.getByTestId('dp-detail-manifest')).toHaveText('200');
    await expect(bar.getByTestId('dp-detail-gc')).toHaveText('50');
    await expect(bar.getByTestId('dp-detail-bytes')).toContainText('MiB');
  });

  test('scenario-B: Awaiting GC — amber chip + tooltip', async ({ page }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: awaitingGCProgress(),
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const bar = page.getByTestId('dp-evacuate').first();
    const chip = bar.getByTestId('dp-awaiting-gc');
    await expect(chip).toBeVisible({ timeout: 10_000 });
    await expect(chip).toContainText(
      'Awaiting GC cleanup: 80 chunks awaiting physical delete',
    );
    await expect(chip).toHaveAttribute('title', /STRATA_GC_GRACE/);
    // Detail block exposes 0 manifest, 80 gc, formatted bytes.
    await bar.getByTestId('dp-detail').locator('summary').click();
    await expect(bar.getByTestId('dp-detail-manifest')).toHaveText('0');
    await expect(bar.getByTestId('dp-detail-gc')).toHaveText('80');
  });

  test('scenario-C: Ready to deregister — green chip', async ({ page }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: readyProgress(),
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    await expect(page.getByTestId('dp-dereg-ready').first()).toBeVisible({
      timeout: 10_000,
    });
  });

  test('scenario-D: null physical → manifest primary + tooltip', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 's3' },
      ],
      progress: nullPhysicalProgress(),
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const bar = page.getByTestId('dp-evacuate').first();
    await expect(bar.getByTestId('dp-primary-count')).toHaveText('150');
    await expect(bar.getByTestId('dp-physical-unavailable')).toBeVisible();
    await expect(bar.getByTestId('dp-physical-unavailable')).toHaveAttribute(
      'title',
      /physical count unavailable/,
    );
    // Detail block surfaces "unavailable" for the physical-bytes row.
    await bar.getByTestId('dp-detail').locator('summary').click();
    await expect(bar.getByTestId('dp-detail-bytes')).toContainText(
      'unavailable',
    );
  });
});
