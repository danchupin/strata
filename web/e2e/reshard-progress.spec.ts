import { expect, test, type Page } from '@playwright/test';

// US-006 reshard-progress — the bucket-detail Resharding panel mirrors the
// drain-progress UX: a typed-confirm Reshard action that queues an online
// shard-resize, plus a progress indicator that polls the US-005 reshard
// endpoint (GET /admin/v1/buckets/{name}/reshard) and walks the operator
// through trigger → in-progress → complete.
//
// The e2e gateway runs the memory backend (no physical reshard, supported=
// false), so every bucket-scoped endpoint this panel touches is spoofed via
// page.route() — same pattern as drain-progress.spec.ts. That lets us drive
// both the supported (Cassandra-shaped) state machine AND the range-scan
// disabled state deterministically without seeding the gateway.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

const BUCKET = 'bkt';

interface ReshardShape {
  ok: boolean;
  bucket: string;
  supported: boolean;
  state: 'idle' | 'queued' | 'running';
  source?: number;
  target?: number;
  shard_count: number;
  last_key?: string;
  started_at?: number;
}

async function login(page: Page) {
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill('test');
  await page.getByLabel('Secret Key').fill('test');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

function bucketDetail() {
  return {
    name: BUCKET,
    owner: 'alice',
    region: 'test-region',
    created_at: 1_700_000_000,
    versioning: 'Off',
    object_lock: false,
    size_bytes: 0,
    object_count: 0,
    backend_presign: false,
    shard_count: 64,
  };
}

// installBucketRoutes mocks every bucket-scoped endpoint the detail page +
// Distribution tab fetch, EXCEPT /reshard which each test wires with its own
// state machine. `reshard` is the live closure the GET handler reads.
function installBucketRoutes(page: Page, reshard: () => ReshardShape) {
  page.route(`**/admin/v1/buckets/${BUCKET}`, async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(bucketDetail()),
    });
  });
  page.route(`**/admin/v1/buckets/${BUCKET}/placement`, async (route) => {
    await route.fulfill({ status: 404, body: 'no placement' });
  });
  page.route(`**/admin/v1/buckets/${BUCKET}/objects*`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ objects: [], common_prefixes: [], is_truncated: false }),
    });
  });
  page.route(`**/admin/v1/buckets/${BUCKET}/distribution`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        shards: Array.from({ length: 4 }, (_, i) => ({
          shard: i,
          bytes: 0,
          objects: 0,
        })),
      }),
    });
  });
  page.route(`**/admin/v1/buckets/${BUCKET}/reshard`, async (route) => {
    if (route.request().method() === 'GET') {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(reshard()),
      });
      return;
    }
    // POST — the test's state machine advances on subsequent GETs; ack 202.
    const r = reshard();
    await route.fulfill({
      status: 202,
      contentType: 'application/json',
      body: JSON.stringify({ ...r, state: 'queued' }),
    });
  });
}

async function gotoDistribution(page: Page) {
  await login(page);
  await page.goto(`/console/buckets/${BUCKET}?tab=distribution`);
  await expect(page.getByTestId('reshard-panel')).toBeVisible({ timeout: 10_000 });
}

test.describe('Strata console — reshard panel (US-006)', () => {
  test('trigger → in-progress → complete on a reshardable backend', async ({
    page,
  }) => {
    // The GET state machine: a poll counter advances queued → running →
    // complete once the operator has triggered the reshard. Before the
    // trigger the panel sits idle with the action enabled (supported).
    let triggered = false;
    let pollsSinceTrigger = 0;
    const state = (): ReshardShape => {
      if (!triggered) {
        return {
          ok: true,
          bucket: BUCKET,
          supported: true,
          state: 'idle',
          shard_count: 64,
        };
      }
      pollsSinceTrigger += 1;
      if (pollsSinceTrigger <= 1) {
        return {
          ok: true,
          bucket: BUCKET,
          supported: true,
          state: 'queued',
          source: 64,
          target: 128,
          shard_count: 64,
          started_at: 1_700_000_000,
        };
      }
      if (pollsSinceTrigger <= 2) {
        return {
          ok: true,
          bucket: BUCKET,
          supported: true,
          state: 'running',
          source: 64,
          target: 128,
          shard_count: 64,
          last_key: 'photos/2026/cat.jpg',
          started_at: 1_700_000_000,
        };
      }
      return {
        ok: true,
        bucket: BUCKET,
        supported: true,
        state: 'idle',
        shard_count: 128,
      };
    };

    installBucketRoutes(page, state);
    await gotoDistribution(page);

    // Idle + enabled trigger, no progress bar yet.
    const trigger = page.getByTestId('reshard-trigger');
    await expect(trigger).toBeEnabled();
    await expect(page.getByTestId('reshard-progress')).toHaveCount(0);
    await expect(page.getByTestId('reshard-shard-count')).toHaveText('64');

    // Trigger: typed-confirm dialog.
    await trigger.click();
    await expect(page.getByTestId('reshard-target')).toHaveText('128');
    const submit = page.getByTestId('reshard-confirm-submit');
    await expect(submit).toBeDisabled();
    await page.getByTestId('reshard-confirm-input').fill(BUCKET);
    await expect(submit).toBeEnabled();
    // Flip the machine on submit, then ack.
    await submit.click();
    triggered = true;

    // In-progress: state walks queued → running, cursor surfaces.
    await expect(page.getByTestId('reshard-progress')).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.getByTestId('reshard-state')).toHaveText('Migrating rows', {
      timeout: 10_000,
    });
    await expect(page.getByTestId('reshard-cursor')).toContainText(
      'photos/2026/cat.jpg',
    );

    // Complete: idle-after-active renders the green completion affordance with
    // the new shard count.
    await expect(page.getByTestId('reshard-complete')).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.getByTestId('reshard-complete')).toContainText('128');
  });

  test('disabled with tooltip on a range-scan (TiKV) backend', async ({
    page,
  }) => {
    installBucketRoutes(page, () => ({
      ok: true,
      bucket: BUCKET,
      supported: false,
      state: 'idle',
      shard_count: 64,
    }));
    await gotoDistribution(page);

    const trigger = page.getByTestId('reshard-trigger');
    await expect(trigger).toBeDisabled();
    await expect(page.getByTestId('reshard-trigger-wrap')).toHaveAttribute(
      'title',
      'range-scan backend needs no resharding',
    );
    await expect(page.getByTestId('reshard-noop-note')).toContainText(
      'range-scan backend needs no resharding',
    );
  });
});
