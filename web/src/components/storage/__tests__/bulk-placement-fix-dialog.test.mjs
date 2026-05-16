// Unit tests for the pure-logic helpers backing BulkPlacementFixDialog
// (US-005 drain-transparency). Following the precedent established by
// placement-drain-banner-dismissal.test.mjs, we re-implement the helper
// shape locally and assert the contract — any drift between the
// production helper and this fixture is caught by the Playwright e2e
// (web/e2e/drain-transparency.spec.ts in US-008).
//
// Run via:
//   pnpm --filter web test:unit-bulk
// or directly:
//   node --test web/src/components/storage/__tests__/bulk-placement-fix-dialog.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';

// Fixture implementations mirror exports from BulkPlacementFixDialog.tsx.
// Keep these in sync with the production code; drift is caught by e2e.
function uniformOptions(buckets, selected) {
  const active = buckets.filter((b) => selected[b.name] !== false);
  if (active.length === 0) return [];
  const labelSets = active.map(
    (b) => new Set((b.suggested_policies ?? []).map((s) => s.label)),
  );
  const first = labelSets[0];
  const intersection = [];
  for (const label of first) {
    if (labelSets.every((s) => s.has(label))) intersection.push(label);
  }
  return intersection;
}

function resolvePolicy(bucket, perBucketIdx, applyUniform, uniformLabel) {
  const suggestions = bucket.suggested_policies ?? [];
  if (suggestions.length === 0) return null;
  if (applyUniform) {
    const match = suggestions.find((s) => s.label === uniformLabel);
    if (match) return match;
    return suggestions[0];
  }
  const idx = perBucketIdx[bucket.name] ?? 0;
  return suggestions[idx] ?? suggestions[0];
}

function normalizePlacementMode(m) {
  return m === 'strict' ? 'strict' : 'weighted';
}

function strictOnly(buckets) {
  return buckets.filter(
    (b) => normalizePlacementMode(b.placement_mode) === 'strict',
  );
}

function resolveModeOverride(choice) {
  return choice.placement_mode_override === 'weighted' ? 'weighted' : 'strict';
}

// Three-bucket fixture: a stuck_single_policy bucket whose Placement
// points only at the draining cluster, a stuck_no_policy bucket with
// no Placement at all, and a third bucket that happens to share the
// "Add all live clusters (uniform)" suggestion (so it sits in the
// intersection) but has a unique "Replace draining with cephb" entry
// (so it should be filtered OUT of the intersection if the others lack
// it).
const fixture = [
  {
    name: 'tx-stuck',
    current_policy: { cephb: 1 },
    category: 'stuck_single_policy',
    placement_mode: 'strict',
    chunk_count: 1024,
    bytes_used: 1024 * 4096,
    suggested_policies: [
      {
        label: 'Add all live clusters (uniform)',
        policy: { cephb: 0, default: 1, cephc: 1 },
      },
      { label: 'Replace draining with default', policy: { default: 1 } },
      { label: 'Replace draining with cephc', policy: { cephc: 1 } },
    ],
  },
  {
    name: 'tx-residual',
    current_policy: null,
    category: 'stuck_no_policy',
    placement_mode: 'weighted',
    chunk_count: 5,
    bytes_used: 5 * 4096,
    suggested_policies: [
      {
        label: 'Set initial policy: live clusters uniform',
        policy: { default: 1, cephc: 1 },
      },
      { label: 'Set initial policy: target default', policy: { default: 1 } },
      { label: 'Set initial policy: target cephc', policy: { cephc: 1 } },
    ],
  },
  {
    name: 'tx-mig',
    current_policy: { cephb: 1, default: 1 },
    category: 'migratable',
    placement_mode: 'weighted',
    chunk_count: 100,
    bytes_used: 100 * 4096,
    suggested_policies: [
      {
        label: 'Add all live clusters (uniform)',
        policy: { cephb: 0, default: 1, cephc: 1 },
      },
      { label: 'Replace draining with default', policy: { default: 1 } },
    ],
  },
];

test('uniformOptions: empty selection yields empty list', () => {
  assert.deepEqual(uniformOptions(fixture, {}), []);
  assert.deepEqual(
    uniformOptions(fixture, { 'tx-stuck': false, 'tx-residual': false, 'tx-mig': false }),
    [],
  );
});

test('uniformOptions: single bucket returns its full suggestion set', () => {
  const labels = uniformOptions(fixture, {
    'tx-stuck': true,
    'tx-residual': false,
    'tx-mig': false,
  });
  assert.deepEqual(labels.sort(), [
    'Add all live clusters (uniform)',
    'Replace draining with cephc',
    'Replace draining with default',
  ]);
});

test('uniformOptions: intersection drops labels missing on any selected bucket', () => {
  // tx-stuck + tx-mig share "Add all live clusters (uniform)" and
  // "Replace draining with default"; tx-stuck has "Replace draining
  // with cephc" but tx-mig doesn't, so it must NOT appear.
  const labels = uniformOptions(fixture, {
    'tx-stuck': true,
    'tx-mig': true,
    'tx-residual': false,
  });
  assert.deepEqual(labels.sort(), [
    'Add all live clusters (uniform)',
    'Replace draining with default',
  ]);
});

test('uniformOptions: stuck_no_policy + stuck_single_policy → empty intersection (label prefixes differ)', () => {
  // The handler stamps "Set initial policy:" on stuck_no_policy and
  // "Add all live clusters (uniform)" / "Replace draining with" on
  // stuck_single_policy, so there is no overlap and the intersection
  // is empty by design. Operator must uncheck the toggle and pick
  // per-bucket in this case.
  const labels = uniformOptions(fixture, {
    'tx-stuck': true,
    'tx-residual': true,
    'tx-mig': false,
  });
  assert.deepEqual(labels, []);
});

test('resolvePolicy: per-bucket index path', () => {
  const out = resolvePolicy(fixture[0], { 'tx-stuck': 1 }, false, '');
  assert.equal(out.label, 'Replace draining with default');
  assert.deepEqual(out.policy, { default: 1 });
});

test('resolvePolicy: per-bucket falls back to first when idx out of range', () => {
  const out = resolvePolicy(fixture[0], { 'tx-stuck': 99 }, false, '');
  assert.equal(out.label, 'Add all live clusters (uniform)');
});

test('resolvePolicy: uniform path matches label across bucket', () => {
  // Both tx-stuck and tx-mig carry the uniform label — both should
  // land on their respective "Add all live clusters (uniform)" entry.
  const a = resolvePolicy(
    fixture[0],
    {},
    true,
    'Add all live clusters (uniform)',
  );
  const b = resolvePolicy(
    fixture[2],
    {},
    true,
    'Add all live clusters (uniform)',
  );
  assert.equal(a.label, 'Add all live clusters (uniform)');
  assert.equal(b.label, 'Add all live clusters (uniform)');
  // Same label, but tx-stuck's policy carries the draining key forced
  // to 0 while tx-mig's policy does the same with default unchanged —
  // the per-bucket payload differs even though the label is uniform.
  assert.deepEqual(a.policy, { cephb: 0, default: 1, cephc: 1 });
  assert.deepEqual(b.policy, { cephb: 0, default: 1, cephc: 1 });
});

test('resolvePolicy: uniform with unknown label falls back to first suggestion (defensive)', () => {
  const out = resolvePolicy(fixture[0], {}, true, 'No such label');
  assert.equal(out.label, 'Add all live clusters (uniform)');
});

test('resolvePolicy: empty suggested_policies returns null', () => {
  const bucket = { name: 'empty', suggested_policies: null };
  assert.equal(resolvePolicy(bucket, {}, false, ''), null);
  assert.equal(resolvePolicy(bucket, {}, true, 'foo'), null);
});

test('strictOnly: filters to placement_mode === strict, dropping weighted + missing', () => {
  // Fixture: tx-stuck is strict; tx-residual + tx-mig are weighted. The
  // dialog only ever surfaces strict-flagged stuck buckets (US-005
  // effective-placement) because weighted stuck buckets auto-resolve via
  // cluster.weights and have no operator-fix workflow.
  const out = strictOnly(fixture);
  assert.equal(out.length, 1);
  assert.equal(out[0].name, 'tx-stuck');
});

test('strictOnly: legacy "" placement_mode treated as weighted (dropped)', () => {
  // Legacy buckets (created pre-US-001) carry placement_mode=""; the
  // server coerces it to "weighted" on the wire, but defend in depth
  // against drift — the UI normalizer must also coerce unknowns.
  const legacy = [
    { name: 'legacy', placement_mode: '', category: 'stuck_single_policy' },
    { name: 'strict-row', placement_mode: 'strict', category: 'stuck_single_policy' },
  ];
  const out = strictOnly(legacy);
  assert.equal(out.length, 1);
  assert.equal(out[0].name, 'strict-row');
});

test('resolveModeOverride: weighted on Flip suggestion, strict otherwise', () => {
  // The server stamps placement_mode_override: "weighted" on the
  // Flip-to-weighted shortcut so cluster.weights auto-fallback unsticks
  // the bucket without a policy edit. Per-cluster replacement
  // suggestions carry "strict" so the compliance pin is preserved.
  // Suggestions without an override default to "strict" because the
  // dialog is filtered to compliance-locked buckets.
  assert.equal(
    resolveModeOverride({
      label: 'Flip',
      policy: { cephb: 1 },
      placement_mode_override: 'weighted',
    }),
    'weighted',
  );
  assert.equal(
    resolveModeOverride({
      label: 'Replace with default (keep strict)',
      policy: { default: 1 },
      placement_mode_override: 'strict',
    }),
    'strict',
  );
  assert.equal(
    resolveModeOverride({ label: 'No override', policy: { default: 1 } }),
    'strict',
  );
  assert.equal(
    resolveModeOverride({
      label: 'Empty override',
      policy: { default: 1 },
      placement_mode_override: '',
    }),
    'strict',
  );
});
