// Unit tests for the pure helper backing the state-aware action button
// on the ClustersSubsection card (US-007 drain-cleanup). Following the
// precedent in bulk-placement-fix-dialog.test.mjs + live-cluster-weight
// -slider.test.mjs, we re-implement the helper logic locally and assert
// the contract — drift between this fixture and the production helper
// (`web/src/components/storage/clusterCardAction.ts`) is caught by the
// drain-cleanup Playwright spec (US-005).
//
// Run via:
//   pnpm --filter web test:unit
// or directly:
//   node --test web/src/components/storage/__tests__/cluster-card-action.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';

// Local re-impl of clusterCardAction — keep in sync with
// web/src/components/storage/clusterCardAction.ts.
function clusterCardAction(input) {
  const s = (input.state ?? '').toLowerCase();
  switch (s) {
    case 'pending':
      return 'activate';
    case 'live':
      return 'drain';
    case 'draining_readonly':
      return 'none';
    case 'evacuating': {
      const chunks = input.chunks;
      if (chunks == null || chunks > 0) {
        return 'undrain-confirm-evacuation';
      }
      if (input.deregisterReady) {
        return 'cancel-deregister-prep';
      }
      return 'undrain-disabled-gc';
    }
    case 'removed':
      return 'drain-disabled';
    default:
      return 'drain';
  }
}

function undrainDisabledTooltip(notReadyReasons) {
  if (notReadyReasons.length === 0) {
    return 'Cannot undrain while GC queue is processing.';
  }
  const reasons = notReadyReasons.join(', ');
  return `Cannot undrain while safety probes are pending: ${reasons}.`;
}

test('truth table row: pending → activate', () => {
  assert.equal(
    clusterCardAction({
      state: 'pending',
      chunks: null,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'activate',
  );
});

test('truth table row: live → drain', () => {
  assert.equal(
    clusterCardAction({
      state: 'live',
      chunks: null,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'drain',
  );
});

test('truth table row: draining_readonly → none (DrainProgressBar owns Upgrade+Undrain)', () => {
  assert.equal(
    clusterCardAction({
      state: 'draining_readonly',
      chunks: null,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'none',
  );
});

test('truth table row: evacuating + chunks>0 → undrain-confirm-evacuation', () => {
  assert.equal(
    clusterCardAction({
      state: 'evacuating',
      chunks: 1024,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'undrain-confirm-evacuation',
  );
});

test('truth table row: evacuating + chunks=null (loading) → undrain-confirm-evacuation (safe default)', () => {
  // Falling through to the confirm-evacuation cell is the safe default
  // while drain-progress is still loading — the modal warns about no
  // rollback so an operator cannot accidentally undrain mid-flight.
  assert.equal(
    clusterCardAction({
      state: 'evacuating',
      chunks: null,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'undrain-confirm-evacuation',
  );
});

test('truth table row: evacuating + chunks=0 + deregister_ready=false (gc_pending) → undrain-disabled-gc', () => {
  assert.equal(
    clusterCardAction({
      state: 'evacuating',
      chunks: 0,
      deregisterReady: false,
      notReadyReasons: ['gc_queue_pending'],
    }),
    'undrain-disabled-gc',
  );
});

test('truth table row: evacuating + chunks=0 + deregister_ready=true → cancel-deregister-prep (NO Undrain)', () => {
  assert.equal(
    clusterCardAction({
      state: 'evacuating',
      chunks: 0,
      deregisterReady: true,
      notReadyReasons: [],
    }),
    'cancel-deregister-prep',
  );
});

test('truth table row: removed → drain-disabled', () => {
  assert.equal(
    clusterCardAction({
      state: 'removed',
      chunks: null,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'drain-disabled',
  );
});

test('helper is case-insensitive on state (mirrors stateLower in callers)', () => {
  assert.equal(
    clusterCardAction({
      state: 'EVACUATING',
      chunks: 5,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'undrain-confirm-evacuation',
  );
  assert.equal(
    clusterCardAction({
      state: 'Live',
      chunks: null,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'drain',
  );
});

test('unknown state falls through to drain (safe default — operator can still drain)', () => {
  assert.equal(
    clusterCardAction({
      state: 'mystery',
      chunks: null,
      deregisterReady: false,
      notReadyReasons: [],
    }),
    'drain',
  );
});

test('undrainDisabledTooltip: empty reasons → fallback text', () => {
  assert.equal(
    undrainDisabledTooltip([]),
    'Cannot undrain while GC queue is processing.',
  );
});

test('undrainDisabledTooltip: single reason quoted in title', () => {
  assert.equal(
    undrainDisabledTooltip(['gc_queue_pending']),
    'Cannot undrain while safety probes are pending: gc_queue_pending.',
  );
});

test('undrainDisabledTooltip: multiple reasons comma-joined', () => {
  assert.equal(
    undrainDisabledTooltip(['gc_queue_pending', 'open_multipart']),
    'Cannot undrain while safety probes are pending: gc_queue_pending, open_multipart.',
  );
});
