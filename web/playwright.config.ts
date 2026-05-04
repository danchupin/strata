import { defineConfig, devices } from '@playwright/test';

// Strata web console e2e config (US-012).
//
// - Chromium-only on CI for speed; the console is a vanilla SPA without
//   browser-specific APIs so cross-browser coverage is low value here.
// - baseURL targets the gateway dev port (:9999, set by `make run-memory`),
//   not :9000 — Makefile and embedded /console handler share that port.
// - webServer boots the gateway in memory mode with the static-credential
//   seed the spec logs in with. STRATA_AUTH_MODE stays `off` so the spec
//   can seed buckets via plain HTTP PUT without dragging SigV4 into the
//   test harness; the admin login path validates against
//   STRATA_STATIC_CREDENTIALS regardless of S3 auth mode.
const PORT = 9999;

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: [
    ['list'],
    ['html', { outputFolder: 'playwright-report', open: 'never' }],
  ],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: [
      `STRATA_LISTEN=:${PORT}`,
      'STRATA_META_BACKEND=memory',
      'STRATA_DATA_BACKEND=memory',
      'STRATA_AUTH_MODE=off',
      'STRATA_STATIC_CREDENTIALS=test:test:owner',
      'STRATA_CONSOLE_JWT_SECRET=000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f',
      'STRATA_CLUSTER_NAME=strata-e2e',
      'STRATA_NODE_ID=e2e-node',
      'go run ./cmd/strata server',
    ].join(' '),
    cwd: '..',
    url: `http://127.0.0.1:${PORT}/console/`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    stdout: 'pipe',
    stderr: 'pipe',
  },
});
