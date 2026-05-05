import { expect, test, type Page } from '@playwright/test';

// Phase 3 debug coverage (US-015). Five flows, each independent so a failure
// in one does not bleed selector state into the next:
//   1. audit-tail        — login → AuditTail → trigger PutObject via fetch →
//                          assert event appears in the live tail.
//   2. slow-queries      — set min_ms=0 → assert recent rows render.
//   3. trace-browser     — trigger request → grab `X-Request-Id` from response
//                          header → paste → assert ≥1 span renders.
//   4. hot-buckets-empty — Prom unset (default in playwright.config.ts) →
//                          MetricsUnavailable empty-state card renders.
//   5. hot-shards-s3     — mocked endpoint returns `{empty:true}` so the
//                          s3-explainer card renders without booting an s3
//                          backend (the e2e gateway runs memory backend).
//
// All flows run against `make run-memory`-shape gateway booted by
// playwright.config.ts: STRATA_AUTH_MODE=off, STRATA_META_BACKEND=memory,
// STRATA_DATA_BACKEND=memory, ringbuf=on (otel default), Prom URL unset.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

test.describe('Strata debug console — Phase 3 critical paths', () => {
  test('audit-tail: PUT object → row appears in live tail', async ({
    page,
    request,
  }) => {
    const bucket = `e2e-tail-${Date.now()}`;

    // Seed bucket up front; bucket-create rows would also appear in the tail
    // but the spec focuses on the post-mount object PUT to keep the assertion
    // unambiguous.
    const seed = await request.put(`/${bucket}`);
    expect([200, 409]).toContain(seed.status());

    await login(page);
    await page.goto('/console/diagnostics/audit-tail');
    await expect(
      page.getByRole('heading', { name: 'Audit tail' }),
    ).toBeVisible();
    // Wait for the SSE EventSource to flip to "connected" so the
    // broadcaster has a subscriber before we drive traffic.
    await expect(page.getByText('connected')).toBeVisible({
      timeout: 15_000,
    });

    // Drive a state-changing request via plain HTTP — STRATA_AUTH_MODE=off so
    // SigV4 is not required. AuditMiddleware fan-outs the row to the SSE
    // broadcaster after Enqueue.
    const put = await request.put(`/${bucket}/key.txt`, {
      data: 'hello',
    });
    expect(put.status(), 'object PUT should succeed').toBe(200);

    // The row lands in the virtualised list keyed by data-testid. The action
    // column shows `PutObject` for object-PUT — assert ≥1 row shows up.
    const list = page.getByTestId('audit-tail-list');
    await expect(list.getByText('PutObject').first()).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByText(/[1-9]\d* events streamed/)).toBeVisible({
      timeout: 5_000,
    });
  });

  test('slow-queries: min_ms=0 → recent rows render', async ({
    page,
    request,
  }) => {
    const bucket = `e2e-slow-${Date.now()}`;

    // Seed traffic before opening the page so the audit table already
    // contains rows. Memory backend stamps total_time_ms on every audited
    // request; min_ms=0 + since=15m surfaces them all.
    const seed = await request.put(`/${bucket}`);
    expect([200, 409]).toContain(seed.status());
    const obj = await request.put(`/${bucket}/k`, { data: 'x' });
    expect(obj.status()).toBe(200);

    await login(page);
    await page.goto('/console/diagnostics/slow-queries');
    await expect(
      page.getByRole('heading', { name: 'Slow queries' }),
    ).toBeVisible();

    // Drop the latency filter so even microsecond-fast memory-backend rows
    // qualify. The Min latency input is a plain number Input — fill('0').
    const minMs = page.getByLabel('Min latency (ms)');
    await minMs.fill('0');
    // Filter is debounced, so wait for the table to repopulate.
    await expect(page.getByText(/Page \d+ · [1-9]\d* (row|rows)/)).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByRole('cell', { name: 'PutObject' }).first()).toBeVisible();
  });

  test('trace-browser: paste request-id → spans render', async ({
    page,
    request,
  }) => {
    const bucket = `e2e-trace-${Date.now()}`;

    // Trigger a request whose trace will land in the in-process ring buffer
    // (default STRATA_OTEL_RINGBUF=on). Capture X-Request-Id from the
    // response header — the ring buffer indexes traces by request id.
    const put = await request.put(`/${bucket}`);
    expect([200, 409]).toContain(put.status());
    const requestID = put.headers()['x-request-id'];
    expect(requestID, 'gateway should emit X-Request-Id').toBeTruthy();

    await login(page);
    await page.goto('/console/diagnostics/trace');
    await expect(
      page.getByRole('heading', { name: 'Trace browser' }),
    ).toBeVisible();

    await page.getByLabel('Paste a request-id').fill(requestID);
    await page.getByRole('button', { name: 'Look up' }).click();

    // Resolution navigates to /diagnostics/trace/<requestID>. The waterfall
    // is a list with role="list" + aria-label="Trace waterfall"; assert at
    // least one span row.
    await expect(page).toHaveURL(
      new RegExp(`/console/diagnostics/trace/${requestID}/?$`),
      { timeout: 10_000 },
    );
    const waterfall = page.getByRole('list', { name: 'Trace waterfall' });
    await expect(waterfall).toBeVisible({ timeout: 10_000 });
    await expect(waterfall.locator('[role="listitem"]').first()).toBeVisible();
  });

  test('hot-buckets-empty: Prom unset → MetricsUnavailable card renders', async ({
    page,
  }) => {
    await login(page);
    await page.goto('/console/diagnostics/hot-buckets');
    await expect(
      page.getByRole('heading', { name: 'Hot buckets' }),
    ).toBeVisible();
    // Prom is unset in the e2e webServer config → 503 MetricsUnavailable →
    // amber empty-state card renders.
    await expect(page.getByText('Metrics unavailable')).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByText(/STRATA_PROMETHEUS_URL/)).toBeVisible();
  });

  test('hot-shards-s3: empty:true response → s3-explainer card renders', async ({
    page,
    request,
  }) => {
    const bucket = `e2e-shards-${Date.now()}`;
    const seed = await request.put(`/${bucket}`);
    expect([200, 409]).toContain(seed.status());

    await login(page);

    // The e2e gateway runs the memory data backend, not s3 — but the AC
    // requires the s3-explainer empty-state to be exercised. Mock the
    // hot-shards endpoint to return the s3-backend short-circuit shape.
    await page.route(
      new RegExp(`/admin/v1/diagnostics/hot-shards/${bucket}(\\?.*)?$`),
      (route) =>
        route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            empty: true,
            reason: 's3-over-s3 stores objects 1:1, no shards',
          }),
        }),
    );

    await page.goto(`/console/buckets/${bucket}`);
    await page.getByRole('tab', { name: 'Hot Shards' }).click();
    await expect(
      page.getByText('Shard heatmap is not applicable'),
    ).toBeVisible({ timeout: 10_000 });
    await expect(
      page.getByText(/RADOS-backed/, { exact: false }),
    ).toBeVisible();
  });
});
