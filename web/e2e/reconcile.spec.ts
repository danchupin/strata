import { expect, test, type Page } from '@playwright/test';

// US-006 reconcile — the Reconcile diagnostics page mirrors the drain /
// reshard-progress UX: a form that queues a reconcile pass (orphan or dangling,
// with a pass-scoped policy picker), a progress block that polls
// GET /admin/v1/reconcile/{id} and walks the operator through trigger →
// in-progress → complete, and a CLI-only rebuild-index card that links to the
// runbook instead of exposing a one-click destructive rebuild.
//
// The e2e gateway runs the memory backend with no reconcile worker running, so
// the job would sit queued forever — every reconcile endpoint is spoofed via
// page.route() (same pattern as reshard-progress.spec.ts) to drive the
// queued → running → done state machine deterministically.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

const JOB_ID = 'rec-00000000-0000-0000-0000-000000000001';

interface ReconcileShape {
  ok: boolean;
  id: string;
  cluster?: string;
  pool?: string;
  bucket?: string;
  policy: string;
  state: 'queued' | 'running' | 'done' | 'error';
  cursor?: string;
  scanned: number;
  orphans_found: number;
  orphans_gc: number;
  orphans_report: number;
  absent_backref: number;
  manifests_scanned: number;
  healthy: number;
  dangling_found: number;
  dangling_quarantine: number;
  dangling_report: number;
  errors: number;
  message?: string;
  started_at?: number;
}

function zeroCounters() {
  return {
    scanned: 0,
    orphans_found: 0,
    orphans_gc: 0,
    orphans_report: 0,
    absent_backref: 0,
    manifests_scanned: 0,
    healthy: 0,
    dangling_found: 0,
    dangling_quarantine: 0,
    dangling_report: 0,
    errors: 0,
  };
}

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

// installReconcileRoutes wires POST (ack 202 queued) + GET (state machine the
// closure returns) for /admin/v1/reconcile.
function installReconcileRoutes(page: Page, status: () => ReconcileShape) {
  page.route('**/admin/v1/reconcile', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    await route.fulfill({
      status: 202,
      contentType: 'application/json',
      body: JSON.stringify({
        ok: true,
        id: JOB_ID,
        cluster: 'ceph-a',
        pool: 'strata-data',
        policy: 'report',
        state: 'queued',
        ...zeroCounters(),
      }),
    });
  });
  page.route(`**/admin/v1/reconcile/${JOB_ID}`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(status()),
    });
  });
}

async function gotoReconcile(page: Page) {
  await login(page);
  await page.goto('/console/diagnostics/reconcile');
  await expect(page.getByTestId('reconcile-page')).toBeVisible({
    timeout: 10_000,
  });
}

test.describe('Strata console — reconcile page (US-006)', () => {
  test('orphan pass: trigger → in-progress → complete with summary', async ({
    page,
  }) => {
    // GET state machine: queued → running (counters climb) → done.
    let pollsSinceTrigger = 0;
    let triggered = false;
    const status = (): ReconcileShape => {
      pollsSinceTrigger += triggered ? 1 : 0;
      const base = {
        ok: true,
        id: JOB_ID,
        cluster: 'ceph-a',
        pool: 'strata-data',
        policy: 'report',
        started_at: 1_700_000_000,
        ...zeroCounters(),
      };
      if (pollsSinceTrigger <= 1) {
        return { ...base, state: 'queued' };
      }
      // Hold running for several polls so the in-progress assertions are
      // stable against the 2s poll cadence (the real worker dwells here too).
      if (pollsSinceTrigger <= 6) {
        return {
          ...base,
          state: 'running',
          scanned: 1280,
          orphans_found: 3,
          orphans_report: 3,
          cursor: 'pg-hash:0x3f',
        };
      }
      return {
        ...base,
        state: 'done',
        scanned: 4096,
        orphans_found: 5,
        orphans_report: 5,
        absent_backref: 2,
        message: 'orphan pass complete: 5 orphans reported, 2 without back-reference',
      };
    };

    installReconcileRoutes(page, status);
    await gotoReconcile(page);

    // Orphan is the default pass; fill cluster + pool and submit.
    await page.getByTestId('reconcile-cluster').fill('ceph-a');
    await page.getByTestId('reconcile-pool').fill('strata-data');
    const submit = page.getByTestId('reconcile-submit');
    await expect(submit).toBeEnabled();
    await submit.click();
    triggered = true;

    // In-progress: status card appears, state running, orphan counters surface.
    await expect(page.getByTestId('reconcile-status-card')).toBeVisible({
      timeout: 10_000,
    });
    // CSS `capitalize` is visual-only — the DOM text stays lowercase.
    await expect(page.getByTestId('reconcile-state')).toContainText(/running/, {
      timeout: 10_000,
    });
    await expect(page.getByTestId('reconcile-orphan-counters')).toContainText(
      'Chunks scanned',
    );
    await expect(page.getByTestId('reconcile-cursor')).toContainText(
      'pg-hash:0x3f',
    );

    // Complete: done state, completion affordance + post-run summary message.
    await expect(page.getByTestId('reconcile-complete')).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.getByTestId('reconcile-message')).toContainText(
      'orphan pass complete',
    );
  });

  test('dangling pass shows manifest counters and report/quarantine policy', async ({
    page,
  }) => {
    installReconcileRoutes(page, () => ({
      ok: true,
      id: JOB_ID,
      bucket: '11111111-1111-1111-1111-111111111111',
      policy: 'quarantine',
      state: 'done',
      ...zeroCounters(),
      manifests_scanned: 200,
      healthy: 198,
      dangling_found: 2,
      dangling_quarantine: 2,
      message: 'dangling pass complete: 2 quarantined',
    }));
    await gotoReconcile(page);

    // Flip to the dangling pass — the bucket input replaces cluster/pool.
    await page.getByTestId('reconcile-pass').click();
    await page.getByRole('option', { name: /Dangling manifests/ }).click();
    await expect(page.getByTestId('reconcile-bucket')).toBeVisible();
    await expect(page.getByTestId('reconcile-cluster')).toHaveCount(0);

    // The policy picker now offers quarantine (a dangling-only policy).
    await page.getByTestId('reconcile-policy').click();
    await expect(
      page.getByRole('option', { name: /quarantine/ }),
    ).toBeVisible();
    await page.getByRole('option', { name: /quarantine/ }).click();

    await page.getByTestId('reconcile-bucket').fill('my-bucket');
    await page.getByTestId('reconcile-submit').click();

    await expect(page.getByTestId('reconcile-dangling-counters')).toContainText(
      'Manifests scanned',
    );
    await expect(page.getByTestId('reconcile-message')).toContainText(
      'dangling pass complete',
    );
  });

  test('rebuild-index is CLI-only — links to the runbook, no one-click button', async ({
    page,
  }) => {
    installReconcileRoutes(page, () => ({
      ok: true,
      id: JOB_ID,
      policy: 'report',
      state: 'queued',
      ...zeroCounters(),
    }));
    await gotoReconcile(page);

    const card = page.getByTestId('rebuild-index-card');
    await expect(card).toBeVisible();
    await expect(card).toContainText('strata admin rebuild-index');
    await expect(card).toContainText('CLI only');
    const link = page.getByTestId('rebuild-runbook-link');
    await expect(link).toHaveAttribute(
      'href',
      /operate\/metadata-data-reconcile/,
    );
  });
});
