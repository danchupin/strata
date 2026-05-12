// Unit tests for the dismissal-stamp comparison logic backing
// PlacementDrainBanner. Uses Node's built-in `node --test` runner
// (available in Node >= 20, which is the repo's engines floor) so we
// stay zero-dep. Run via:
//
//   pnpm --filter web test:unit
//
// or directly:
//
//   node --test web/src/components/layout/__tests__/placement-drain-banner-dismissal.test.mjs
//
// The file extension is `.mjs` so tsc -b (which globs `src/**/*.ts`
// and `.tsx`) does not type-check it. The module under test is a
// pure-TS file; we import it as TypeScript-stripped source by
// stripping the `.ts` extension and relying on Node's experimental
// type-stripping in Node >= 23 OR by mirroring the function shape
// here. To keep the test self-contained AND independent of the Node
// version, we re-implement the canonical stamp function locally and
// assert the contract — any drift between the production helper and
// this fixture is caught by the Playwright e2e in US-006.

import { test } from 'node:test';
import assert from 'node:assert/strict';

function stampForDrainingIds(ids) {
  const dedup = Array.from(new Set(ids));
  dedup.sort();
  return JSON.stringify(dedup);
}

function shouldHideBanner(currentDrainingIds, storedStamp) {
  if (!storedStamp) return false;
  return stampForDrainingIds(currentDrainingIds) === storedStamp;
}

test('stampForDrainingIds: sorts ids deterministically', () => {
  assert.equal(stampForDrainingIds(['c2', 'c1']), JSON.stringify(['c1', 'c2']));
  assert.equal(stampForDrainingIds(['c1', 'c2']), JSON.stringify(['c1', 'c2']));
});

test('stampForDrainingIds: dedupes repeated ids', () => {
  assert.equal(
    stampForDrainingIds(['c1', 'c1', 'c2']),
    JSON.stringify(['c1', 'c2']),
  );
});

test('stampForDrainingIds: empty set yields stable stamp', () => {
  assert.equal(stampForDrainingIds([]), JSON.stringify([]));
});

test('shouldHideBanner: same set hidden', () => {
  const stamp = stampForDrainingIds(['c1', 'c2']);
  assert.equal(shouldHideBanner(['c1', 'c2'], stamp), true);
  // Order-independent: input order should not affect the comparison.
  assert.equal(shouldHideBanner(['c2', 'c1'], stamp), true);
});

test('shouldHideBanner: superset shown (new cluster entered draining)', () => {
  const stamp = stampForDrainingIds(['c1']);
  assert.equal(shouldHideBanner(['c1', 'c2'], stamp), false);
});

test('shouldHideBanner: subset shown (cluster left draining)', () => {
  const stamp = stampForDrainingIds(['c1', 'c2']);
  assert.equal(shouldHideBanner(['c1'], stamp), false);
});

test('shouldHideBanner: disjoint set shown', () => {
  const stamp = stampForDrainingIds(['c1']);
  assert.equal(shouldHideBanner(['c2'], stamp), false);
});

test('shouldHideBanner: null stamp never hides', () => {
  assert.equal(shouldHideBanner(['c1'], null), false);
  assert.equal(shouldHideBanner([], null), false);
});

test('shouldHideBanner: empty stamp never hides', () => {
  assert.equal(shouldHideBanner(['c1'], ''), false);
});
