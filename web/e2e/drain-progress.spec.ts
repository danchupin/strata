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

interface GCConfigShape {
  grace_seconds: number;
  interval_seconds: number;
  batch_size: number;
  concurrency: number;
  shards: number;
}

interface RebalanceConfigShape {
  interval_seconds: number;
  rate_mb_s: number;
  inflight: number;
  shards: number;
  replicas_count: number;
}

interface RebalanceProgressShape {
  metrics_available: boolean;
  moved_total: number;
  refused_total: number;
  observed_bytes_per_sec: number;
  series: Array<[number, number]>;
}

interface SpoofState {
  clusters: ClusterRow[];
  progress: DrainProgressShape;
  // Optional config + rebalance-progress overrides — when set the
  // /admin/v1/gc-config + /admin/v1/rebalance-config endpoints serve
  // these payloads instead of 404. US-002 drain-rebalance-transparency.
  gcConfig?: GCConfigShape | { status: number };
  rebalanceConfig?: RebalanceConfigShape | { status: number };
  rebalanceProgress?: RebalanceProgressShape;
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
    const payload = state.rebalanceProgress ?? {
      metrics_available: false,
      moved_total: 0,
      refused_total: 0,
      observed_bytes_per_sec: 0,
      series: [],
    };
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(payload),
    });
  });

  page.route('**/admin/v1/gc-config', async (route) => {
    if (state.gcConfig == null) {
      await route.fulfill({ status: 404, body: 'not found' });
      return;
    }
    if ('status' in state.gcConfig) {
      await route.fulfill({ status: state.gcConfig.status, body: 'err' });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(state.gcConfig),
    });
  });

  page.route('**/admin/v1/rebalance-config', async (route) => {
    if (state.rebalanceConfig == null) {
      await route.fulfill({ status: 404, body: 'not found' });
      return;
    }
    if ('status' in state.rebalanceConfig) {
      await route.fulfill({ status: state.rebalanceConfig.status, body: 'err' });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(state.rebalanceConfig),
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

// US-002 drain-rebalance-transparency — exercises the live ETA on the
// Awaiting GC chip + live bandwidth / formula ETA on the Migrating chip
// once /admin/v1/{gc,rebalance}-config + /admin/v1/clusters/<id>/
// rebalance-progress.observed_bytes_per_sec ride along.
test.describe('Strata console — drain-progress live ETA + bandwidth (US-002)', () => {
  test('awaiting-gc chip: ETA suffix + dynamic tooltip from gc-config', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: awaitingGCProgress(),
      gcConfig: {
        grace_seconds: 300,
        interval_seconds: 60,
        batch_size: 100,
        concurrency: 4,
        shards: 1,
      },
      rebalanceConfig: {
        interval_seconds: 30,
        rate_mb_s: 100,
        inflight: 4,
        shards: 1,
        replicas_count: 2,
      },
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const bar = page.getByTestId('dp-evacuate').first();
    const chip = bar.getByTestId('dp-awaiting-gc');
    await expect(chip).toBeVisible({ timeout: 10_000 });
    // grace 300s → 5m; queue 80 / (100×1) = 0.8 ticks × (60/60 = 1m) → 1m
    // total ≈ 5 + 1 = 6m ETA
    await expect(chip).toContainText(/~6m ETA/);
    await expect(chip).toHaveAttribute(
      'title',
      /ETA computed from current GC queue depth \(80 chunks\), STRATA_GC_GRACE \(300s\), STRATA_GC_INTERVAL \(60s\), STRATA_GC_BATCH_SIZE \(100\), STRATA_GC_SHARDS \(1\)/,
    );
  });

  test('awaiting-gc chip: gc-config 404 → static fallback tooltip', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: awaitingGCProgress(),
      // gcConfig omitted → endpoint returns 404 → useQuery is in error
      // state and the chip falls back to the pre-cycle static tooltip.
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const bar = page.getByTestId('dp-evacuate').first();
    const chip = bar.getByTestId('dp-awaiting-gc');
    await expect(chip).toBeVisible({ timeout: 10_000 });
    await expect(chip).toHaveAttribute('title', /STRATA_GC_GRACE/);
    // No ETA suffix in fallback mode.
    await expect(chip).not.toContainText(/ETA\)/);
  });

  test('migrating chip: live bandwidth + formula ETA', async ({ page }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: migratingProgress(),
      gcConfig: {
        grace_seconds: 300,
        interval_seconds: 60,
        batch_size: 100,
        concurrency: 4,
        shards: 1,
      },
      rebalanceConfig: {
        interval_seconds: 30,
        rate_mb_s: 100,
        inflight: 4,
        shards: 1,
        replicas_count: 2,
      },
      rebalanceProgress: {
        metrics_available: true,
        moved_total: 50,
        refused_total: 0,
        // 50 MB/s — 200 chunks × 4 MiB / 50 MB/s ≈ 16.7s → rounds to 0m / 1m
        observed_bytes_per_sec: 50 * 1024 * 1024,
        series: [],
      },
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const bar = page.getByTestId('dp-evacuate').first();
    const summary = bar.getByTestId('dp-summary');
    await expect(summary.getByTestId('dp-observed-mbs')).toHaveText(
      '~50 MB/s observed',
    );
    await expect(summary.getByTestId('dp-eta-formula')).toContainText('ETA');
    await expect(summary).toHaveAttribute(
      'title',
      /ETA from observed bandwidth \(50 MB\/s on cluster cephb\) over remaining manifest chunks \(200\)\. Configured rate cap per replica: 100 MB\/s\./,
    );
  });

  test('migrating chip: PromQL cold start (observed=0) → fallback ETA + cold-start label', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: migratingProgress(),
      gcConfig: {
        grace_seconds: 300,
        interval_seconds: 60,
        batch_size: 100,
        concurrency: 4,
        shards: 1,
      },
      rebalanceConfig: {
        interval_seconds: 30,
        rate_mb_s: 100,
        inflight: 4,
        shards: 1,
        replicas_count: 2,
      },
      rebalanceProgress: {
        metrics_available: true,
        moved_total: 0,
        refused_total: 0,
        observed_bytes_per_sec: 0,
        series: [],
      },
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const summary = page.getByTestId('dp-evacuate').first().getByTestId('dp-summary');
    await expect(summary.getByTestId('dp-observed-mbs')).toHaveText(
      '~0 MB/s observed (cold start)',
    );
    // Fallback denominator: 100 MB/s × 2 / 2 = 100 MB/s effective.
    // 200 chunks × 4 MiB / 100 MB/s ≈ 8.4s → rounds to 0m.
    await expect(summary.getByTestId('dp-eta-formula')).toContainText('ETA');
  });

  test('migrating chip: rate_mb_s=0 → ~24h+ ETA cap', async ({ page }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: migratingProgress(),
      gcConfig: {
        grace_seconds: 300,
        interval_seconds: 60,
        batch_size: 100,
        concurrency: 4,
        shards: 1,
      },
      rebalanceConfig: {
        interval_seconds: 30,
        rate_mb_s: 0,
        inflight: 4,
        shards: 1,
        replicas_count: 2,
      },
      rebalanceProgress: {
        metrics_available: true,
        moved_total: 0,
        refused_total: 0,
        observed_bytes_per_sec: 0,
        series: [],
      },
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const summary = page.getByTestId('dp-evacuate').first().getByTestId('dp-summary');
    await expect(summary.getByTestId('dp-eta-formula')).toHaveText('~24h+ ETA');
  });

  test('migrating chip: rebalance-config 404 → pre-cycle fallback (no live row)', async ({
    page,
  }) => {
    const state: SpoofState = {
      clusters: [
        { id: 'cephb', state: 'evacuating', mode: 'evacuate', backend: 'rados' },
      ],
      progress: migratingProgress(),
      // rebalanceConfig omitted → 404 → chip falls back to fallbackEtaSeconds.
    };
    installRoutes(page, state);
    await gotoStorageData(page);

    const summary = page.getByTestId('dp-evacuate').first().getByTestId('dp-summary');
    await expect(summary.getByTestId('dp-observed-mbs')).toHaveCount(0);
    await expect(summary.getByTestId('dp-eta-formula')).toHaveCount(0);
    // Pre-cycle behavior: ~2m fallback from migratingProgress().eta_seconds=120
    await expect(summary).toContainText(/~2m/);
  });
});
