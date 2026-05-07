import { defineConfig, devices } from '@playwright/test';

// Multi-replica e2e config (US-006). Drives web/e2e/multi-replica.spec.ts
// against a docker-compose-managed lab-tikv stack:
//
//   make up-lab-tikv       # 2 strata-tikv replicas + nginx LB at :9999
//   make wait-strata-lab   # readyz on 9001 / 9002 / 9999
//
// Unlike the default playwright.config.ts there is NO inline webServer block
// here — the gateway is operator-managed (or CI-managed via the gated
// e2e-ui-multi-replica job). baseURL points at the LB so the spec exercises
// load-balanced session stickiness.
//
// The default config testIgnore-s multi-replica.spec.ts so the e2e-ui job
// (memory-mode webServer) does not trip when this file is picked up by a
// non-multi-replica run.
const PORT = 9999;

export default defineConfig({
  testDir: './e2e',
  testMatch: ['multi-replica.spec.ts'],
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  // Container restarts + heartbeat-TTL grace push individual tests past the
  // default 30s timeout; bump to 90s so the dead-grace sleep + reload assertion
  // chain has slack on a busy CI runner.
  timeout: 90_000,
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
      name: 'multi-replica',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
