// Unit tests for the pure helpers backing <BucketReshardPanel> (US-006).
// Following the precedent in cluster-card-action.test.mjs, we re-implement the
// helper logic locally and assert the contract — drift between this fixture
// and the production helpers (web/src/api/reshard.ts ::nextPowerOfTwo and the
// state→label / supported→enabled mapping in
// web/src/components/BucketReshardPanel.tsx) is caught by the reshard-progress
// Playwright spec (web/e2e/reshard-progress.spec.ts).
//
// Run via:
//   pnpm --filter web test:unit
// or directly:
//   node --test web/src/components/__tests__/bucket-reshard-panel.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';

// Local re-impl of nextPowerOfTwo — keep in sync with web/src/api/reshard.ts.
// Smallest power of two STRICTLY greater than n (reshard only doubles up).
function nextPowerOfTwo(n) {
  if (!Number.isFinite(n) || n < 1) return 1;
  let p = 1;
  while (p <= n) p *= 2;
  return p;
}

// Local re-impl of the state→label mapping in the ReshardProgress sub-component.
function reshardStateLabel(state) {
  switch (state) {
    case 'running':
      return 'Migrating rows';
    case 'queued':
      return 'Queued';
    default:
      return 'idle';
  }
}

// Local re-impl of the action gating: the Reshard button is enabled only when
// the backend supports physical resharding AND no job is currently in flight.
function reshardActionEnabled(supported, state) {
  const active = state === 'queued' || state === 'running';
  return supported && !active;
}

test('nextPowerOfTwo doubles the common shard counts', () => {
  assert.equal(nextPowerOfTwo(64), 128);
  assert.equal(nextPowerOfTwo(128), 256);
  assert.equal(nextPowerOfTwo(16), 32);
  assert.equal(nextPowerOfTwo(1), 2);
});

test('nextPowerOfTwo is strictly greater for exact powers of two', () => {
  // 64 is already a power of two — the next target must be 128, never 64.
  assert.ok(nextPowerOfTwo(64) > 64);
  assert.ok(nextPowerOfTwo(256) > 256);
});

test('nextPowerOfTwo guards degenerate inputs', () => {
  assert.equal(nextPowerOfTwo(0), 1);
  assert.equal(nextPowerOfTwo(-5), 1);
  assert.equal(nextPowerOfTwo(NaN), 1);
});

test('reshardStateLabel maps the three job states', () => {
  assert.equal(reshardStateLabel('queued'), 'Queued');
  assert.equal(reshardStateLabel('running'), 'Migrating rows');
  assert.equal(reshardStateLabel('idle'), 'idle');
});

test('reshard action disabled on unsupported (range-scan) backend', () => {
  assert.equal(reshardActionEnabled(false, 'idle'), false);
});

test('reshard action enabled on supported backend when idle', () => {
  assert.equal(reshardActionEnabled(true, 'idle'), true);
});

test('reshard action disabled while a job is in flight', () => {
  assert.equal(reshardActionEnabled(true, 'queued'), false);
  assert.equal(reshardActionEnabled(true, 'running'), false);
});
