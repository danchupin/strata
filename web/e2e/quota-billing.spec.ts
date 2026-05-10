import { expect, test, type Page } from '@playwright/test';

// US-010 quota + billing UI coverage.
//
// Two flows, each independent so a failure in one does not leak selector
// state into the next:
//   1. bucket-quota — login → CreateBucket → Usage tab → Edit Quota → save →
//      reload → assert quota persists → PUT object that exceeds → assert
//      403 QuotaExceeded over the S3 path.
//   2. user-billing — login → CreateUser → User detail → Billing button →
//      Edit user quota → save → reload → assert quota persists.

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

async function createBucketViaUI(page: Page, name: string) {
  await page.goto('/console/buckets');
  await page.getByRole('button', { name: /^Create$/ }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel(/^Name$/i).fill(name);
  await dialog.getByRole('button', { name: 'Create bucket' }).click();
  await expect(dialog).toBeHidden({ timeout: 10_000 });
  await expect(
    page.getByRole('link', { name, exact: true }),
  ).toBeVisible({ timeout: 10_000 });
}

test.describe('Strata admin console — quota + billing (US-010)', () => {
  test('bucket-quota: set via UI → reload → persists → exceeding PUT returns 403', async ({
    page,
    request,
  }) => {
    const bucket = `e2e-quota-${Date.now()}`;

    await login(page);
    await createBucketViaUI(page, bucket);

    // Drive the Usage tab.
    await page.goto(`/console/buckets/${bucket}`);
    await page.getByRole('tab', { name: 'Usage' }).click();
    await expect(
      page.getByTestId('bucket-usage-bar'),
    ).toBeVisible({ timeout: 10_000 });

    // Open Edit quota dialog and set caps. 1024 byte cap ⇒ a 2 KiB PUT
    // should hit the 403.
    await page.getByRole('button', { name: 'Edit quota' }).click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel('Max bytes').fill('1024');
    await dialog.getByLabel('Max objects').fill('5');
    await dialog.getByLabel('Max bytes per object').fill('1024');
    await dialog.getByRole('button', { name: 'Save quota' }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    // Reload + assert the quota persisted via the admin API (source of truth).
    await page.reload();
    const q = await page.request.get(`/admin/v1/buckets/${bucket}/quota`);
    expect(q.status()).toBe(200);
    const body = await q.json();
    expect(body.max_bytes).toBe(1024);
    expect(body.max_objects).toBe(5);
    expect(body.max_bytes_per_object).toBe(1024);

    // PUT that exceeds the per-object cap should be rejected with 403
    // QuotaExceeded over the S3 surface.
    const big = Buffer.alloc(2 * 1024, 0xab);
    const put = await request.put(`/${bucket}/too-big.bin`, { data: big });
    expect(put.status(), 'oversize PUT must be 403').toBe(403);
    const respBody = await put.text();
    expect(respBody).toContain('QuotaExceeded');
  });

  test('user-billing: set user quota via UI → reload → persists', async ({
    page,
  }) => {
    const userName = `e2e-user-billing-${Date.now()}`;

    await login(page);
    await page.goto('/console/iam');
    await page
      .getByRole('button', { name: 'Create user', exact: true })
      .click();
    const createUserDialog = page.getByRole('dialog');
    await createUserDialog.getByLabel('User name').fill(userName);
    await createUserDialog.getByRole('button', { name: 'Create user' }).click();
    await expect(createUserDialog).toBeHidden({ timeout: 10_000 });

    // User detail → Billing button → Edit user quota.
    await page.getByRole('link', { name: userName, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/console/iam/users/${userName}/?$`));
    await page.getByRole('button', { name: 'Billing' }).click();
    await expect(page).toHaveURL(
      new RegExp(`/console/iam/users/${userName}/billing/?$`),
    );

    await page.getByRole('button', { name: 'Edit user quota' }).click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel('Max buckets').fill('3');
    await dialog.getByLabel('Total max bytes').fill('1048576');
    await dialog.getByRole('button', { name: 'Save quota' }).click();
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    // Reload + assert via admin API.
    await page.reload();
    const q = await page.request.get(`/admin/v1/iam/users/${userName}/quota`);
    expect(q.status()).toBe(200);
    const body = await q.json();
    expect(body.max_buckets).toBe(3);
    expect(body.total_max_bytes).toBe(1048576);
  });
});
