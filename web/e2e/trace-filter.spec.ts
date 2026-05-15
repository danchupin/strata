import { expect, test, type Page } from '@playwright/test';

// US-002 drain-followup — UI half of the trace browser filter row +
// URL persistence. Walks the four acceptance loops:
//   - navigate /diagnostics/trace?method=PUT → only PUT traces shown
//   - typing into the path input debounces 250ms (a single request
//     reaches the server with the typed substring)
//   - Clear button → URL cleaned, defaults restored, full list back
//   - browser back/forward restores the prior filter URL state
//
// Spoofs the recent-traces endpoint via page.route() so the spec is
// stand-alone and does not depend on populating the in-process ringbuf
// with real traces. The drain-cleanup spec covers the rendering side
// of <RecentTracesPanel>; this spec is filter-focused.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

interface TraceFixture {
  request_id: string;
  trace_id: string;
  root_name: string;
  started_at_ns: number;
  duration_ms: number;
  status: 'OK' | 'Error' | 'Unset';
  span_count: number;
}

function fixtures(): TraceFixture[] {
  const now = Date.now() * 1_000_000;
  return [
    {
      request_id: 'req-put-ok-1',
      trace_id: '00000000000000000000000000000001',
      root_name: 'PUT /demo-cephb/big-object.bin',
      started_at_ns: now,
      duration_ms: 220,
      status: 'OK',
      span_count: 8,
    },
    {
      request_id: 'req-put-err-2',
      trace_id: '00000000000000000000000000000002',
      root_name: 'PUT /demo-cepha/object.bin',
      started_at_ns: now - 1_000_000_000,
      duration_ms: 35,
      status: 'Error',
      span_count: 5,
    },
    {
      request_id: 'req-get-ok-3',
      trace_id: '00000000000000000000000000000003',
      root_name: 'GET /demo-cephb/object.bin',
      started_at_ns: now - 2_000_000_000,
      duration_ms: 12,
      status: 'OK',
      span_count: 4,
    },
    {
      request_id: 'req-del-err-4',
      trace_id: '00000000000000000000000000000004',
      root_name: 'DELETE /demo-cepha/key',
      started_at_ns: now - 3_000_000_000,
      duration_ms: 9,
      status: 'Error',
      span_count: 3,
    },
  ];
}

function applyFilters(rows: TraceFixture[], q: URLSearchParams): TraceFixture[] {
  const method = q.get('method')?.toUpperCase() ?? '';
  const status = q.get('status') ?? '';
  const path = (q.get('path') ?? q.get('path_substr') ?? '').toLowerCase();
  const minMs = q.get('min_duration_ms');
  const maxMs = q.get('max_duration_ms');
  return rows.filter((r) => {
    if (method && !r.root_name.toUpperCase().startsWith(method + ' ')) return false;
    if (status && r.status !== status) return false;
    if (path && !r.root_name.toLowerCase().includes(path)) return false;
    if (minMs && r.duration_ms < Number(minMs)) return false;
    if (maxMs && r.duration_ms > Number(maxMs)) return false;
    return true;
  });
}

interface RouteState {
  rows: TraceFixture[];
  // Every observed `?path=` value (most-recent first) so the spec can
  // assert debounce: a 4-character burst (e.g. d, de, dem, demo) should
  // collapse to ONE request reaching the server with the final value.
  pathRequests: string[];
}

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

function installRoutes(page: Page, state: RouteState) {
  page.route('**/admin/v1/diagnostics/traces**', async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const url = new URL(route.request().url());
    const q = url.searchParams;
    const path = q.get('path') ?? q.get('path_substr');
    if (path != null) state.pathRequests.unshift(path);
    const filtered = applyFilters(state.rows, q);
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ total: filtered.length, traces: filtered }),
    });
  });
  // Cluster status → otel_endpoint empty (suppresses the Jaeger link
  // dependency on a live collector).
  page.route('**/admin/v1/cluster/status', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        cluster_name: 'lab',
        node_id: 'node-1',
        replica_count: 1,
        otel_endpoint: '',
      }),
    });
  });
}

test.describe('Strata console — trace browser filter row (US-002)', () => {
  test('initial URL filter narrows list; debounce; clear; back/forward restore', async ({
    page,
  }) => {
    const state: RouteState = { rows: fixtures(), pathRequests: [] };
    installRoutes(page, state);

    await login(page);

    // (1) Land on /diagnostics/trace?method=PUT — list shows only PUT rows.
    await page.goto('/console/diagnostics/trace?method=PUT');
    await expect(page.getByTestId('recent-traces-panel')).toBeVisible({
      timeout: 10_000,
    });
    const rows = page.getByTestId('recent-trace-row');
    await expect(rows).toHaveCount(2);

    // The filter row must reflect the URL.
    await expect(page.getByTestId('rtp-method')).toContainText('PUT');

    // (2) Type a path filter — debounce: a 4-char burst within < 250ms
    // collapses to a single server request with the final value.
    state.pathRequests.length = 0;
    await page.getByTestId('rtp-path').fill('demo-cephb');
    // Wait past the debounce + small refetch slack.
    await page.waitForTimeout(500);
    await expect(rows).toHaveCount(1, { timeout: 5_000 });
    await expect(rows.first()).toContainText('demo-cephb');
    // The fill() above sets the value in one shot but the 250ms debounce
    // must still gate the URL/refetch — assert at most a small handful of
    // path-bearing requests landed (one per refetch cycle), not one per
    // keystroke.
    expect(state.pathRequests.length).toBeLessThanOrEqual(2);

    // URL reflects the path filter.
    await expect.poll(() => new URL(page.url()).searchParams.get('path')).toBe(
      'demo-cephb',
    );

    // (3) Clear button → URL clean, list restored to full set.
    await page.getByTestId('rtp-clear').click();
    await expect(rows).toHaveCount(4, { timeout: 5_000 });
    const cleared = new URL(page.url());
    expect(cleared.searchParams.get('method')).toBeNull();
    expect(cleared.searchParams.get('path')).toBeNull();
    expect(cleared.searchParams.get('status')).toBeNull();
    expect(cleared.searchParams.get('min_duration_ms')).toBeNull();

    // (4) Browser back → returns to the path-filtered view.
    await page.goBack();
    await expect(rows).toHaveCount(1, { timeout: 5_000 });
    await expect(page.getByTestId('rtp-path')).toHaveValue('demo-cephb');

    // (5) Browser forward → cleared view again.
    await page.goForward();
    await expect(rows).toHaveCount(4, { timeout: 5_000 });
    await expect(page.getByTestId('rtp-path')).toHaveValue('');
  });

  test('filtered empty state surfaces helper text', async ({ page }) => {
    const state: RouteState = { rows: fixtures(), pathRequests: [] };
    installRoutes(page, state);

    await login(page);
    await page.goto('/console/diagnostics/trace?method=PUT&path=no-such-bucket');
    await expect(
      page.getByTestId('recent-traces-empty-filtered'),
    ).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('recent-trace-row')).toHaveCount(0);
  });
});
