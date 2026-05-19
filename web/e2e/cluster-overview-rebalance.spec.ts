import { expect, test, type Page } from '@playwright/test';

// US-003 drain-rebalance-transparency — exercises the
// <RebalanceConfigCard> mounted on the Cluster Overview page. Verifies:
//   1. Card renders four config rows + tune link + live bandwidth row
//      when both /admin/v1/rebalance-config and /admin/v1/rebalance-
//      bandwidth resolve with metrics_available=true.
//   2. Live bandwidth row hidden when /admin/v1/rebalance-bandwidth
//      returns metrics_available=false (Prom unset upstream).
//   3. Entire card hidden when /admin/v1/rebalance-config 404s
//      (legacy gateway compatibility — no error toast).
//
// The memory-mode webServer has no rebalance worker, so every admin
// endpoint touched by this spec is spoofed via page.route() — same
// pattern as drain-progress.spec.ts.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

interface RebalanceConfigShape {
  interval_seconds: number;
  rate_mb_s: number;
  inflight: number;
  shards: number;
  replicas_count: number;
}

interface RebalanceBandwidthShape {
  metrics_available: boolean;
  bytes_per_sec: number;
  chunks_per_sec: number;
}

interface SpoofState {
  rebalanceConfig?: RebalanceConfigShape | { status: number };
  rebalanceBandwidth?: RebalanceBandwidthShape;
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

  page.route('**/admin/v1/rebalance-bandwidth', async (route) => {
    const payload =
      state.rebalanceBandwidth ?? {
        metrics_available: false,
        bytes_per_sec: 0,
        chunks_per_sec: 0,
      };
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(payload),
    });
  });
}

test.describe('Strata console — Cluster Overview rebalance card (US-003)', () => {
  test('config + live bandwidth: renders four rows + tune link + observed row', async ({
    page,
  }) => {
    const state: SpoofState = {
      rebalanceConfig: {
        interval_seconds: 30,
        rate_mb_s: 100,
        inflight: 4,
        shards: 1,
        replicas_count: 2,
      },
      rebalanceBandwidth: {
        metrics_available: true,
        // 50 MiB/s observed across all destination clusters
        bytes_per_sec: 50 * 1024 * 1024,
        chunks_per_sec: 12.5,
      },
    };
    installRoutes(page, state);
    await login(page);

    const card = page.getByTestId('rebalance-config-card');
    await expect(card).toBeVisible({ timeout: 10_000 });

    await expect(card.getByTestId('rebalance-per-replica')).toHaveText(
      '100 MB/s',
    );
    await expect(card.getByTestId('rebalance-aggregate-label')).toContainText(
      'Aggregate (× 2 replicas)',
    );
    await expect(card.getByTestId('rebalance-aggregate')).toHaveText(
      '~200 MB/s',
    );
    await expect(card.getByTestId('rebalance-effective')).toHaveText(
      '~100 MB/s',
    );
    await expect(card.getByTestId('rebalance-cadence')).toContainText(
      'every 30s · Inflight: 4 · Shards: 1',
    );
    // chunkSize × 2 tokens math tooltip on Aggregate + Effective rows.
    await expect(card.getByTestId('rebalance-aggregate')).toHaveAttribute(
      'title',
      /chunkSize × 2 tokens/,
    );
    await expect(card.getByTestId('rebalance-effective')).toHaveAttribute(
      'title',
      /chunkSize × 2 tokens/,
    );
    // Tune link → US-004 docs anchor.
    await expect(card.getByTestId('rebalance-tune-link')).toHaveAttribute(
      'href',
      /placement-rebalance.*#bandwidth-tuning/,
    );
    await expect(card.getByTestId('rebalance-tune-link')).toHaveAttribute(
      'target',
      '_blank',
    );
    // Live observed row when metrics_available=true.
    await expect(card.getByTestId('rebalance-observed-row')).toBeVisible();
    await expect(card.getByTestId('rebalance-observed-mbs')).toHaveText(
      '~50 MB/s',
    );
    await expect(card.getByTestId('rebalance-observed-chunks')).toHaveText(
      '~12.5 chunks/sec',
    );
  });

  test('observed row hidden when bandwidth endpoint reports metrics_available=false', async ({
    page,
  }) => {
    const state: SpoofState = {
      rebalanceConfig: {
        interval_seconds: 30,
        rate_mb_s: 100,
        inflight: 4,
        shards: 1,
        replicas_count: 2,
      },
      rebalanceBandwidth: {
        metrics_available: false,
        bytes_per_sec: 0,
        chunks_per_sec: 0,
      },
    };
    installRoutes(page, state);
    await login(page);

    const card = page.getByTestId('rebalance-config-card');
    await expect(card).toBeVisible({ timeout: 10_000 });
    await expect(card.getByTestId('rebalance-observed-row')).toHaveCount(0);
  });

  test('card hidden when rebalance-config 404s (legacy gateway)', async ({
    page,
  }) => {
    // rebalanceConfig omitted → 404 → entire card must not render and no
    // error toast surfaces.
    installRoutes(page, {});
    await login(page);

    await expect(page.getByTestId('rebalance-config-card')).toHaveCount(0);
  });
});
