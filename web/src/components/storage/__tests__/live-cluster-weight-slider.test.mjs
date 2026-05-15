// Unit tests for the debounce + revert contract backing
// LiveClusterWeightSlider (US-004 cluster-weights). Following the
// precedent in bulk-placement-fix-dialog.test.mjs we re-implement the
// pure behaviour locally and assert the contract — drift between this
// fixture and the production component is caught by the cluster-weights
// Playwright spec (US-005).
//
// Run via:
//   pnpm --filter web test:unit
// or directly:
//   node --test web/src/components/storage/__tests__/live-cluster-weight-slider.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';

function clampWeight(n) {
  if (!Number.isFinite(n)) return 0;
  if (n < 0) return 0;
  if (n > 100) return 100;
  return Math.round(n);
}

// createSliderController is the production debounce-and-revert flow
// extracted: one timer at a time, every schedule() restarts it; on
// success, savedWeight advances to the last submitted value; on
// failure, pendingWeight reverts to savedWeight and the error is
// surfaced via onError (status 409 vs other).
function createSliderController({
  initialWeight,
  delayMs,
  put,
  onError,
  invalidate,
}) {
  let pendingWeight = initialWeight;
  let savedWeight = initialWeight;
  let timer = null;
  let inFlight = 0;

  function schedule(next) {
    pendingWeight = next;
    if (timer != null) clearTimeout(timer);
    timer = setTimeout(async () => {
      timer = null;
      inFlight++;
      try {
        await put(next);
        savedWeight = next;
        invalidate?.();
      } catch (err) {
        pendingWeight = savedWeight;
        onError?.(err, savedWeight);
      } finally {
        inFlight--;
      }
    }, delayMs);
  }

  return {
    schedule,
    get pendingWeight() {
      return pendingWeight;
    },
    get savedWeight() {
      return savedWeight;
    },
    get hasPending() {
      return timer != null;
    },
    get inFlight() {
      return inFlight;
    },
  };
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

test('clampWeight: bounds + integer rounding', () => {
  assert.equal(clampWeight(-5), 0);
  assert.equal(clampWeight(0), 0);
  assert.equal(clampWeight(50.4), 50);
  assert.equal(clampWeight(50.6), 51);
  assert.equal(clampWeight(100), 100);
  assert.equal(clampWeight(101), 100);
  assert.equal(clampWeight(Number.NaN), 0);
  assert.equal(clampWeight(Number.POSITIVE_INFINITY), 0);
});

test('schedule: rapid drags coalesce to one PUT with the final value', async () => {
  const calls = [];
  const ctrl = createSliderController({
    initialWeight: 10,
    delayMs: 20,
    put: async (v) => {
      calls.push(v);
    },
  });
  ctrl.schedule(25);
  ctrl.schedule(40);
  ctrl.schedule(55);
  ctrl.schedule(72);
  assert.equal(calls.length, 0, 'no PUT before debounce expires');
  assert.equal(ctrl.pendingWeight, 72);
  await sleep(40);
  assert.deepEqual(calls, [72]);
  assert.equal(ctrl.savedWeight, 72);
  assert.equal(ctrl.pendingWeight, 72);
});

test('schedule: single drag fires one PUT after the debounce', async () => {
  const calls = [];
  const ctrl = createSliderController({
    initialWeight: 10,
    delayMs: 20,
    put: async (v) => {
      calls.push(v);
    },
  });
  ctrl.schedule(33);
  await sleep(40);
  assert.deepEqual(calls, [33]);
});

test('revert: 4xx restores savedWeight + invokes onError with status', async () => {
  const errors = [];
  const ctrl = createSliderController({
    initialWeight: 50,
    delayMs: 10,
    put: async () => {
      const err = new Error('weight cephb: 409 Conflict');
      err.status = 409;
      throw err;
    },
    onError: (err, reverted) => {
      errors.push({ status: err.status, reverted });
    },
  });
  ctrl.schedule(75);
  await sleep(30);
  assert.equal(ctrl.pendingWeight, 50, 'pending reverts to saved');
  assert.equal(ctrl.savedWeight, 50);
  assert.deepEqual(errors, [{ status: 409, reverted: 50 }]);
});

test('success: savedWeight advances + invalidate fires once per successful PUT', async () => {
  let invalidated = 0;
  const ctrl = createSliderController({
    initialWeight: 0,
    delayMs: 10,
    put: async () => {},
    invalidate: () => {
      invalidated++;
    },
  });
  ctrl.schedule(15);
  await sleep(20);
  ctrl.schedule(30);
  await sleep(20);
  assert.equal(ctrl.savedWeight, 30);
  assert.equal(invalidated, 2);
});

test('schedule: after a successful PUT a subsequent failure reverts to the latest saved value, not the original', async () => {
  const errors = [];
  let okPath = true;
  const ctrl = createSliderController({
    initialWeight: 10,
    delayMs: 10,
    put: async () => {
      if (!okPath) {
        const err = new Error('weight cephb: 500');
        err.status = 500;
        throw err;
      }
    },
    onError: (err, reverted) => {
      errors.push({ status: err.status, reverted });
    },
  });
  ctrl.schedule(40);
  await sleep(20);
  assert.equal(ctrl.savedWeight, 40);
  okPath = false;
  ctrl.schedule(80);
  await sleep(20);
  assert.equal(ctrl.pendingWeight, 40, 'reverts to latest saved, not initial');
  assert.deepEqual(errors, [{ status: 500, reverted: 40 }]);
});
