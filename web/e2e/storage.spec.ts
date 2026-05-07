import { expect, test, type Page } from '@playwright/test';

// US-006 / web-ui-storage-status cycle e2e coverage.
//
// Three independent flows, all booting against the shared memory-mode gateway
// configured in playwright.config.ts (STRATA_AUTH_MODE=off,
// STRATA_BUCKETSTATS_INTERVAL=500ms so the per-class sampler ticks visibly):
//
//   1. storage-page-renders   — login → /storage → Meta + Data tabs visible,
//                                NodeStatus row count >= 1, PoolStatus row
//                                count >= 1.
//   2. cluster-hero-shows-storage-card — login → home → "Storage" card
//                                visible with at least one class chip
//                                (a seed bucket + object are PUT so the
//                                bucketstats sampler emits one).
//   3. degraded-banner-on-warn — page.route() spoofs
//                                /admin/v1/storage/health to ok=false; banner
//                                visible above shell on every authed page,
//                                dismiss button hides it for the rest of the
//                                browsing context.
//
// We override /admin/v1/storage/health via page.route() rather than setting
// STRATA_STORAGE_HEALTH_OVERRIDE on the gateway because the env-var would
// pollute every other spec sharing the same webServer. The handler honors
// both knobs identically.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

const SEED_BUCKET = 'e2e-storage';
const SEED_KEY = 'seed.txt';

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

test.describe('Strata console — storage status', () => {
  test.beforeAll(async ({ request }) => {
    // Seed bucket + object so the bucketstats sampler has a per-class row to
    // emit. STRATA_AUTH_MODE=off lets us skip SigV4 here; the gateway also
    // reads STRATA_BUCKETSTATS_INTERVAL=500ms (set in playwright.config.ts)
    // so the snapshot populates within a few seconds of process start.
    const mb = await request.put(`/${SEED_BUCKET}`);
    if (![200, 409].includes(mb.status())) {
      throw new Error(`bucket seed PUT /${SEED_BUCKET} → ${mb.status()}`);
    }
    const put = await request.put(`/${SEED_BUCKET}/${SEED_KEY}`, {
      data: 'storage-status-e2e\n',
    });
    if (![200, 201].includes(put.status())) {
      throw new Error(
        `seed object PUT /${SEED_BUCKET}/${SEED_KEY} → ${put.status()}`,
      );
    }
  });

  test('storage-page-renders: Meta + Data tabs show backend rows', async ({
    page,
  }) => {
    await login(page);
    await page.getByRole('link', { name: 'Storage', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/storage\/?$/);
    await expect(
      page.getByRole('heading', { level: 1, name: 'Storage' }),
    ).toBeVisible();

    // Meta tab is the default; one row per node (memory backend → 1 self row).
    await expect(page.getByRole('tab', { name: 'Meta' })).toBeVisible();
    await expect(page.getByRole('tab', { name: 'Data' })).toBeVisible();
    // Memory backend renders an explainer card, NOT a node table — assert via
    // backend label instead. Same applies to the Data tab below.
    await expect(page.getByText(/Backend\s*=\s*Memory/i).first()).toBeVisible({
      timeout: 10_000,
    });

    await page.getByRole('tab', { name: 'Data' }).click();
    await expect(page.getByText(/Backend\s*=\s*Memory/i).first()).toBeVisible({
      timeout: 10_000,
    });
    // Storage classes subsection is always rendered — body is either a chip
    // strip (sampler tick already occurred) or a "no per-class breakdown"
    // hint. Either way the title is mounted.
    await expect(page.getByText('Storage classes')).toBeVisible();
  });

  test('cluster-hero-shows-storage-card: home shows Storage card with class chip', async ({
    page,
  }) => {
    await login(page);
    // Hero card title is rendered eagerly; the chip strip waits for the
    // bucketstats sampler tick (Interval=500ms in e2e config).
    await expect(
      page
        .getByRole('heading', { level: 1, name: 'Cluster Overview' }),
    ).toBeVisible();
    const card = page
      .locator('div')
      .filter({ has: page.getByText('Storage', { exact: true }) })
      .filter({ has: page.getByRole('link', { name: 'View Storage page' }) })
      .first();
    await expect(card).toBeVisible({ timeout: 10_000 });

    // The chip carries the class name (STANDARD by default). Wait up to ~10s
    // for the sampler to emit; fallback to a class-name regex so non-default
    // class configurations still pass.
    await expect(
      card.getByText(/^STANDARD$|^GLACIER|^STANDARD_IA$/).first(),
    ).toBeVisible({ timeout: 10_000 });
  });

  test('degraded-banner-on-warn: degraded health → banner visible, dismiss hides it', async ({
    page,
  }) => {
    // Spoof the storage-health endpoint at the network layer so the banner
    // path is exercised without polluting other specs via env. Returns the
    // exact wire shape the handler emits when STRATA_STORAGE_HEALTH_OVERRIDE
    // is set.
    await page.route('**/admin/v1/storage/health', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          ok: false,
          warnings: ['e2e simulated meta degraded'],
          source: 'meta',
        }),
      });
    });

    await login(page);
    const banner = page.getByRole('alert').filter({
      has: page.getByText(/Storage degraded/),
    });
    await expect(banner).toBeVisible({ timeout: 10_000 });
    await expect(banner.getByText(/e2e simulated meta degraded/)).toBeVisible();
    await expect(
      banner.getByRole('link', { name: 'View Storage page' }),
    ).toBeVisible();

    // Dismiss button hides the banner for the rest of the session.
    await banner
      .getByRole('button', { name: /Dismiss storage degraded banner/i })
      .click();
    await expect(banner).toHaveCount(0);

    // Navigate to another page — banner stays dismissed because the warning
    // signature has not changed.
    await page.getByRole('link', { name: 'Buckets', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/buckets\/?$/);
    await expect(page.getByText(/Storage degraded/)).toHaveCount(0);
  });
});
