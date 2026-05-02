import { expect, test, type Page } from '@playwright/test';

// Critical-path coverage for the Phase 1 console (US-012):
//   anonymous → /console/login redirect
//   login with seed creds → /console/ overview ("1 of 1 nodes healthy")
//   Buckets nav → list page (empty initially)
//   create a bucket via plain S3 HTTP PUT (auth mode off, no SigV4 needed)
//   reload buckets → bucket appears
//   click bucket name → bucket detail page loads
//   sign out → cookie cleared, back on /console/login
//
// The gateway is started by playwright.config.ts webServer with
// STRATA_AUTH_MODE=off + STRATA_STATIC_CREDENTIALS=test:test:owner so:
//   - admin login validates the access key/secret pair against the
//     static-creds store (login does not consult S3 auth mode);
//   - this spec can PUT a bucket over plain HTTP without signing.

const BUCKET = 'e2e-fixture';

// react-router-dom BrowserRouter with basename='/console' yields URLs like
// '/console' for the root and '/console/buckets' for nested routes — no
// trailing slash on the index. Use regex matchers to stay tolerant of either
// shape so the spec doesn't get brittle around router internals.
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

test.describe('Strata console — critical path', () => {
  test.beforeAll(async ({ request }) => {
    // Seed an empty bucket up front via plain HTTP PUT — STRATA_AUTH_MODE=off
    // so the gateway accepts anonymous bucket-create requests.
    const res = await request.put(`/${BUCKET}`);
    if (![200, 409].includes(res.status())) {
      throw new Error(`bucket seed PUT /${BUCKET} → ${res.status()}`);
    }
  });

  test('anon → login redirect, login → overview', async ({ page }) => {
    await login(page);
    // Hero card surfaces "1 of 1 nodes healthy" once the heartbeat goroutine
    // has written its first row (synchronous on Run, so available on first
    // /admin/v1/cluster/status response).
    await expect(
      page.getByText(/1 of 1 nodes? healthy/i),
    ).toBeVisible({ timeout: 15_000 });
    // Page header is the only true <h1> on the overview route; the cluster
    // name surfaces inside a shadcn CardTitle which is a <div>, so check it
    // by text rather than by heading role.
    await expect(
      page.getByRole('heading', { level: 1, name: 'Cluster Overview' }),
    ).toBeVisible();
    await expect(page.getByText('strata-e2e').first()).toBeVisible();
  });

  test('Buckets nav → list shows seeded bucket', async ({ page }) => {
    await login(page);
    await page.getByRole('link', { name: 'Buckets', exact: true }).click();
    await expect(page).toHaveURL(/\/console\/buckets\/?$/);
    await expect(
      page.getByRole('heading', { name: 'Buckets' }),
    ).toBeVisible();
    // The seed bucket created in beforeAll lands as a row link.
    await expect(
      page.getByRole('link', { name: BUCKET, exact: true }),
    ).toBeVisible({ timeout: 10_000 });
  });

  test('Bucket row → bucket detail page loads', async ({ page }) => {
    await login(page);
    await page.goto('/console/buckets');
    await page.getByRole('link', { name: BUCKET, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/console/buckets/${BUCKET}/?$`));
    await expect(
      page.getByRole('heading', { name: BUCKET }),
    ).toBeVisible({ timeout: 10_000 });
  });

  test('Sign out → cookie cleared, back on login', async ({ page }) => {
    await login(page);
    // UserMenu trigger is the access-key button in the top bar.
    await page.getByRole('button', { name: /test/ }).click();
    await page.getByRole('menuitem', { name: 'Sign out' }).click();
    await expect(page).toHaveURL(CONSOLE_LOGIN);
    // The session cookie is dropped (Path=/admin scope).
    const cookies = await page.context().cookies();
    expect(cookies.find((c) => c.name === 'strata_session')).toBeUndefined();
  });
});
