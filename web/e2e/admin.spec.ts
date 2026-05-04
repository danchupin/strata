import { expect, test, type Page } from '@playwright/test';

// Phase 2 admin coverage (US-022). Five flows, each independent so a failure
// in one does not bleed selector state into the next:
//   1. bucket-lifecycle  — login → CreateBucket → upload 5 MB → DeleteObject → DeleteBucket
//   2. iam-keys          — CreateUser → CreateAccessKey → DisableKey → DeleteKey → DeleteUser
//   3. lifecycle-rule    — CreateBucket → add 30-day expiration rule → save → reload → assert
//   4. policy-editor     — open bucket policy → paste public-read → validate → save → reload → assert
//   5. multipart-watchdog— init multipart via fetch → visit page → assert row → bulk-abort → assert gone
//
// The spec relies on STRATA_AUTH_MODE=off (set by playwright.config.ts webServer)
// so plain HTTP S3 PUT/POST requests against the gateway succeed without SigV4.

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

test.describe('Strata admin console — Phase 2 critical paths', () => {
  test('bucket-lifecycle: create → upload 5 MB → delete object → delete bucket', async ({
    page,
    request,
  }) => {
    const bucket = `e2e-lc-${Date.now()}`;
    const key = 'large.bin';

    await login(page);
    await createBucketViaUI(page, bucket);

    // 5 MiB body via plain HTTP PUT — STRATA_AUTH_MODE=off lets us skip SigV4.
    const body = Buffer.alloc(5 * 1024 * 1024, 0xab);
    const put = await request.put(`/${bucket}/${key}`, { data: body });
    expect(put.status(), 'object PUT should succeed').toBe(200);

    // Drive object delete through the UI: detail → ObjectDetailSheet → Delete.
    await page.goto(`/console/buckets/${bucket}`);
    await expect(page.getByRole('button', { name: key }).first()).toBeVisible({
      timeout: 10_000,
    });
    await page.getByRole('button', { name: key }).first().click();
    await page.getByRole('button', { name: 'Delete object' }).click();
    await page.getByRole('button', { name: 'Confirm delete' }).click();
    // List eventually empties; the row disappears.
    await expect(page.getByRole('button', { name: key })).toHaveCount(0, {
      timeout: 10_000,
    });

    // Delete bucket via UI: header button → type-to-confirm dialog → Delete.
    await page.getByRole('button', { name: /Delete bucket/i }).click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel(/Type/i).fill(bucket);
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/buckets\/?$/, { timeout: 10_000 });
    await expect(
      page.getByRole('link', { name: bucket, exact: true }),
    ).toHaveCount(0);
  });

  test('iam-keys: create user → create + disable + delete key → delete user', async ({
    page,
  }) => {
    const userName = `e2e-user-${Date.now()}`;

    await login(page);
    await page.goto('/console/iam');
    await page
      .getByRole('button', { name: 'Create user', exact: true })
      .click();
    const createUserDialog = page.getByRole('dialog');
    await createUserDialog.getByLabel('User name').fill(userName);
    await createUserDialog.getByRole('button', { name: 'Create user' }).click();
    await expect(createUserDialog).toBeHidden({ timeout: 10_000 });

    // Drill into user detail.
    await page.getByRole('link', { name: userName, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/console/iam/users/${userName}/?$`));

    // Mint access key — Done button after the create.
    await page
      .getByRole('button', { name: 'Create access key', exact: true })
      .first()
      .click();
    const akDialog = page.getByRole('dialog');
    await akDialog.getByRole('button', { name: 'Create access key' }).click();
    await expect(
      akDialog.getByText(/only time the secret will be shown/i),
    ).toBeVisible({ timeout: 10_000 });
    // Capture the access key id from the dialog before closing.
    const accessKeyID = await akDialog
      .locator('code')
      .first()
      .innerText();
    expect(accessKeyID).toMatch(/^[A-Z0-9]{16,32}$/);
    await akDialog.getByRole('button', { name: 'Done' }).click();
    await expect(akDialog).toBeHidden();

    // Active row becomes visible; flip it to Disabled.
    const row = page.getByRole('row', { name: new RegExp(accessKeyID) });
    await expect(row).toBeVisible({ timeout: 10_000 });
    await expect(row.getByText('Active')).toBeVisible();
    await row.getByRole('button', { name: 'Disable' }).click();
    await expect(row.getByText('Disabled')).toBeVisible({ timeout: 10_000 });

    // Delete the access key (last 8 chars confirm).
    await row
      .getByRole('button', { name: `Delete access key ${accessKeyID}` })
      .click();
    const delKeyDialog = page.getByRole('dialog');
    await delKeyDialog.getByLabel(/Type the last 8 characters/i).fill(
      accessKeyID.slice(-8),
    );
    await delKeyDialog
      .getByRole('button', { name: 'Delete access key' })
      .click();
    await expect(delKeyDialog).toBeHidden({ timeout: 10_000 });
    await expect(row).toHaveCount(0);

    // Back to users list, delete the user.
    await page.goto('/console/iam');
    const userRow = page.getByRole('row', { name: new RegExp(userName) });
    await expect(userRow).toBeVisible({ timeout: 10_000 });
    await userRow.getByRole('button', { name: `Delete user ${userName}` }).click();
    const delUserDialog = page.getByRole('dialog');
    await delUserDialog.getByLabel(/Type/i).fill(userName);
    await delUserDialog.getByRole('button', { name: 'Delete user' }).click();
    await expect(delUserDialog).toBeHidden({ timeout: 10_000 });
    await expect(page.getByRole('row', { name: new RegExp(userName) })).toHaveCount(0);
  });

  test('lifecycle-rule: add 30-day expiration → save → reload → persists', async ({
    page,
  }) => {
    const bucket = `e2e-lcrule-${Date.now()}`;

    await login(page);
    await createBucketViaUI(page, bucket);

    await page.goto(`/console/buckets/${bucket}`);
    await page.getByRole('tab', { name: 'Lifecycle' }).click();
    // Visual tab is the default; Add rule seeds expiration: { days: 30 }.
    await page.getByRole('button', { name: /Add rule/ }).click();
    await page.getByRole('button', { name: 'Save lifecycle' }).click();

    // Reload + assert persisted via the admin API (source of truth).
    // page.request shares cookies with the page so the session JWT is sent.
    await page.reload();
    const lc = await page.request.get(`/admin/v1/buckets/${bucket}/lifecycle`);
    expect(lc.status()).toBe(200);
    const body = await lc.json();
    expect(body.rules?.length).toBeGreaterThanOrEqual(1);
    expect(body.rules[0].expiration?.days).toBe(30);
  });

  test('policy-editor: paste PublicRead template → validate → save → reload → persists', async ({
    page,
  }) => {
    const bucket = `e2e-pol-${Date.now()}`;

    await login(page);
    await createBucketViaUI(page, bucket);

    await page.goto(`/console/buckets/${bucket}`);
    await page.getByRole('tab', { name: 'Policy' }).click();
    // The starter-policy <Select> is a Radix combobox.
    await page.getByRole('combobox').first().click();
    await page.getByRole('option', { name: 'PublicRead' }).click();
    await page.getByRole('button', { name: 'Validate' }).click();
    await expect(page.getByText(/parses cleanly/i)).toBeVisible({
      timeout: 10_000,
    });
    await page.getByRole('button', { name: 'Save policy' }).click();

    // Reload + assert persisted via the admin API.
    await page.reload();
    const pol = await page.request.get(`/admin/v1/buckets/${bucket}/policy`);
    expect(pol.status()).toBe(200);
    const text = await pol.text();
    expect(text).toContain('"Sid": "PublicRead"');
    expect(text).toContain(`arn:aws:s3:::${bucket}/*`);
  });

  test('multipart-watchdog: row appears → bulk-abort → row gone', async ({
    page,
    request,
  }) => {
    const bucket = `e2e-mp-${Date.now()}`;
    const key = 'streamed.bin';

    await login(page);
    await createBucketViaUI(page, bucket);

    // Initiate a multipart upload directly via the S3 surface (auth off).
    const init = await request.post(`/${bucket}/${key}?uploads`);
    expect(init.status(), 'CreateMultipartUpload should succeed').toBe(200);
    const initBody = await init.text();
    const m = initBody.match(/<UploadId>([^<]+)<\/UploadId>/);
    expect(m, `UploadId in ${initBody}`).not.toBeNull();
    const uploadID = m![1];

    await page.goto('/console/multipart');
    await page.getByRole('button', { name: 'Refresh multipart uploads' }).click();
    const row = page.getByRole('row', { name: new RegExp(bucket) });
    await expect(row).toBeVisible({ timeout: 15_000 });

    // Bulk-abort path: select-all checkbox → Abort selected → confirm.
    await page.getByRole('checkbox', { name: 'Select all visible uploads' }).check();
    page.once('dialog', (d) => void d.accept());
    await page.getByRole('button', { name: /Abort selected/ }).click();

    // Refresh and assert the row is gone.
    await page.getByRole('button', { name: 'Refresh multipart uploads' }).click();
    await expect(
      page.getByRole('row', { name: new RegExp(uploadID.slice(0, 8)) }),
    ).toHaveCount(0, { timeout: 10_000 });
  });
});
