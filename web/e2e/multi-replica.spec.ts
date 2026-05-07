import { expect, test, type Page } from '@playwright/test';

import { dockerStart, dockerStop } from './fixtures/docker';

// US-006 — multi-replica failure-scenario coverage. Boots OUTSIDE this spec
// (CI: `make up-lab-tikv && make wait-strata-lab`; local: same). This file
// is intentionally excluded from the default playwright project (see
// playwright.config.ts testIgnore) and only runs under
// playwright.multi-replica.config.ts so the memory-mode webServer doesn't
// fight the docker stack.
//
// Three flows:
//   1. cluster-overview-shows-2-nodes — login -> /console -> NodesTable shows
//      2 healthy rows.
//   2. cross-replica-put-get          — login -> create bucket via UI ->
//      upload a small file via the UploadDialog (presigned PUT, served via
//      the LB so the request can land on either replica) -> reload -> object
//      visible in the bucket.
//   3. worker-rotation                — identify the lifecycle-leader chip
//      holder via /admin/v1/cluster/nodes -> dockerStop the corresponding
//      container -> wait DEAD_GRACE -> reload -> assert OTHER replica row
//      now carries the chip. dockerStart in finally so the next test in this
//      file (or a re-run) starts from the 2-node baseline.
//
// Login creds come from STRATA_STATIC_CREDENTIALS (first comma-entry, same
// shape the smoke harness parses). The lab gateway runs with
// STRATA_AUTH_MODE=required, so unauthenticated S3 PUT/GET would 403; the
// console upload path uses presigned URLs minted by the admin API — the
// session cookie authenticates the presign call, the SigV4 query string
// authenticates the actual PUT.

const CONSOLE_HOME = /\/console\/?$/;
const CONSOLE_LOGIN = /\/console\/login\/?$/;

const NODE_ID_TO_CONTAINER: Record<string, string> = {
  'strata-a': 'strata-tikv-a',
  'strata-b': 'strata-tikv-b',
};

// Heartbeat TTL is 30 s and the lease renew runs at TTL/3 — chip rotation
// lands within ~30-35 s after the holder is killed. 40 s gives a small
// buffer for a slow scheduler tick on a busy CI runner.
const DEAD_GRACE_MS = 40_000;
const REJOIN_GRACE_MS = 30_000;

interface ClusterNode {
  id: string;
  status: string;
  leader_for: string[];
}

function staticCreds(): { ak: string; sk: string } {
  const raw = process.env.STRATA_STATIC_CREDENTIALS;
  if (!raw) {
    throw new Error(
      'STRATA_STATIC_CREDENTIALS unset — multi-replica spec needs lab-gateway creds',
    );
  }
  const first = raw.split(',')[0];
  const [ak, sk] = first.split(':');
  if (!ak || !sk) {
    throw new Error(
      `STRATA_STATIC_CREDENTIALS first entry malformed: ${first}`,
    );
  }
  return { ak, sk };
}

async function login(page: Page) {
  const { ak, sk } = staticCreds();
  await page.goto('/console/');
  await expect(page).toHaveURL(CONSOLE_LOGIN);
  await page.getByLabel('Access Key').fill(ak);
  await page.getByLabel('Secret Key').fill(sk);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(CONSOLE_HOME);
}

async function fetchNodes(page: Page): Promise<ClusterNode[]> {
  const res = await page.request.get('/admin/v1/cluster/nodes');
  expect(res.status(), 'GET /admin/v1/cluster/nodes').toBe(200);
  const body = (await res.json()) as { nodes?: ClusterNode[] };
  return body.nodes ?? [];
}

function leaderHolder(nodes: ClusterNode[], chip: string): ClusterNode | null {
  return (
    nodes.find(
      (n) =>
        n.status === 'healthy' &&
        Array.isArray(n.leader_for) &&
        n.leader_for.includes(chip),
    ) ?? null
  );
}

test.describe('Strata console — multi-replica lab (lab-tikv)', () => {
  test('cluster-overview-shows-2-nodes: nodes table reports both replicas healthy', async ({
    page,
  }) => {
    await login(page);
    await expect(
      page.getByRole('heading', { level: 1, name: 'Cluster Overview' }),
    ).toBeVisible();
    // Hero card surfaces the count once the heartbeat goroutine has written
    // both rows. Allow ~15 s slack for the second replica's first tick.
    await expect(
      page.getByText(/2 of 2 nodes? healthy/i),
    ).toBeVisible({ timeout: 15_000 });

    // Nodes table — both replica IDs present as cells.
    await expect(page.getByRole('cell', { name: 'strata-a' })).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByRole('cell', { name: 'strata-b' })).toBeVisible({
      timeout: 15_000,
    });
  });

  test('cross-replica-put-get: console upload survives a reload via the LB', async ({
    page,
  }) => {
    const bucket = `e2e-mr-${Date.now()}`;
    const key = 'cross-replica.txt';
    const payload = Buffer.from('multi-replica e2e payload\n');

    await login(page);

    // Create bucket via the buckets-list dialog (same shape as admin.spec.ts).
    await page.goto('/console/buckets');
    await page.getByRole('button', { name: /^Create$/ }).first().click();
    const createDialog = page.getByRole('dialog');
    await createDialog.getByLabel(/^Name$/i).fill(bucket);
    await createDialog
      .getByRole('button', { name: 'Create bucket' })
      .click();
    await expect(createDialog).toBeHidden({ timeout: 10_000 });

    // Drill into the bucket and open the Upload dialog.
    await page
      .getByRole('link', { name: bucket, exact: true })
      .click();
    await expect(page).toHaveURL(new RegExp(`/console/buckets/${bucket}/?$`));
    await page.getByRole('button', { name: /^Upload$/ }).first().click();
    const uploadDialog = page.getByRole('dialog');
    await expect(
      uploadDialog.getByText(/Upload objects/),
    ).toBeVisible();

    // Drop a small payload through the file picker — single-PUT path
    // (size < 5 MiB) means one presigned URL + one Web Worker round-trip.
    await uploadDialog.locator('#file-picker').setInputFiles({
      name: key,
      mimeType: 'text/plain',
      buffer: payload,
    });
    await uploadDialog
      .getByRole('button', { name: /^Upload 1 file$/ })
      .click();
    // Per-file ✓ marker means the worker reported done. Footer flips to
    // "Close" once allDone, so use that as the upload-complete signal.
    await expect(
      uploadDialog.getByRole('button', { name: 'Close' }),
    ).toBeVisible({ timeout: 30_000 });
    // Close + reload to force a fresh fetch (likely lands on a different
    // replica through the LB's least_conn upstream).
    await uploadDialog.getByRole('button', { name: 'Close' }).click();
    await page.reload();

    await expect(
      page.getByRole('button', { name: key }).first(),
    ).toBeVisible({ timeout: 15_000 });
  });

  test('worker-rotation: kill lifecycle-leader holder, chip rotates to survivor', async ({
    page,
  }) => {
    test.setTimeout(120_000);
    await login(page);

    const nodes = await fetchNodes(page);
    const holder = leaderHolder(nodes, 'lifecycle-leader');
    expect(holder, 'lifecycle-leader chip must have a holder at baseline')
      .not.toBeNull();
    const holderID = holder!.id;
    const otherID = holderID === 'strata-a' ? 'strata-b' : 'strata-a';
    const holderContainer = NODE_ID_TO_CONTAINER[holderID];
    expect(holderContainer, `unknown node id: ${holderID}`).toBeDefined();

    try {
      await dockerStop(holderContainer);
      // Wait the heartbeat-TTL grace + lease-renew tick for the surviving
      // replica to pick up lifecycle-leader.
      await page.waitForTimeout(DEAD_GRACE_MS);

      const after = await fetchNodes(page);
      const newHolder = leaderHolder(after, 'lifecycle-leader');
      expect(
        newHolder?.id,
        `lifecycle-leader expected to rotate to ${otherID}`,
      ).toBe(otherID);

      // UI side: reload Cluster Overview and assert the chip lives on the
      // OTHER replica's row.
      await page.goto('/console/');
      await expect(
        page.getByRole('heading', { level: 1, name: 'Cluster Overview' }),
      ).toBeVisible();
      const otherRow = page.getByRole('row', { name: new RegExp(otherID) });
      await expect(otherRow).toBeVisible({ timeout: 15_000 });
      await expect(
        otherRow.getByText('lifecycle-leader'),
      ).toBeVisible({ timeout: 15_000 });
    } finally {
      await dockerStart(holderContainer).catch(() => undefined);
      // Give the rejoined replica time to /readyz + register heartbeats so
      // the next test (or a re-run) starts from a clean 2-node baseline.
      await page.waitForTimeout(REJOIN_GRACE_MS);
    }
  });
});
