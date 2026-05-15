// Unit tests for the URL <-> filter state helpers backing
// RecentTracesPanel (drain-followup US-002). Re-implements the four
// helpers locally per the same node:test convention used by the other
// __tests__/*.mjs files in this repo — keeps the test zero-dep and
// avoids tsc -b picking it up. Any drift between the production helper
// and this fixture is caught by the Playwright spec
// `web/e2e/trace-filter.spec.ts`.

import { test } from 'node:test';
import assert from 'node:assert/strict';

const METHODS = ['PUT', 'GET', 'DELETE', 'POST', 'HEAD', 'OPTIONS', 'PATCH'];
const STATUSES = ['Error', 'OK'];

const EMPTY_FILTER = {
  method: '',
  status: '',
  path: '',
  minDurationMs: '',
};

function readFilterFromURL(sp) {
  const m = (sp.get('method') ?? '').toUpperCase();
  const method = METHODS.includes(m) ? m : '';
  const s = sp.get('status') ?? '';
  const status = STATUSES.includes(s) ? s : '';
  const path = sp.get('path') ?? '';
  const min = sp.get('min_duration_ms') ?? '';
  const minDurationMs = /^\d+$/.test(min) ? min : '';
  return { method, status, path, minDurationMs };
}

function writeFilterToParams(sp, f) {
  const next = new URLSearchParams(sp);
  if (f.method) next.set('method', f.method);
  else next.delete('method');
  if (f.status) next.set('status', f.status);
  else next.delete('status');
  if (f.path) next.set('path', f.path);
  else next.delete('path');
  if (f.minDurationMs) next.set('min_duration_ms', f.minDurationMs);
  else next.delete('min_duration_ms');
  return next;
}

function isEmptyFilter(f) {
  return !f.method && !f.status && !f.path && !f.minDurationMs;
}

function toRecentTracesQuery(f, debouncedPath) {
  const q = {};
  if (f.method) q.method = f.method;
  if (f.status) q.status = f.status;
  if (debouncedPath) q.path = debouncedPath;
  if (f.minDurationMs && /^\d+$/.test(f.minDurationMs)) {
    q.minDurationMs = Number(f.minDurationMs);
  }
  return q;
}

test('readFilterFromURL: empty URL yields empty filter', () => {
  assert.deepEqual(readFilterFromURL(new URLSearchParams('')), EMPTY_FILTER);
});

test('readFilterFromURL: parses each axis', () => {
  const sp = new URLSearchParams('method=PUT&status=Error&path=demo&min_duration_ms=100');
  assert.deepEqual(readFilterFromURL(sp), {
    method: 'PUT',
    status: 'Error',
    path: 'demo',
    minDurationMs: '100',
  });
});

test('readFilterFromURL: lowercases method input but stores uppercase enum', () => {
  const sp = new URLSearchParams('method=put');
  assert.equal(readFilterFromURL(sp).method, 'PUT');
});

test('readFilterFromURL: rejects unknown method enum (stale bookmark guard)', () => {
  const sp = new URLSearchParams('method=BOGUS');
  assert.equal(readFilterFromURL(sp).method, '');
});

test('readFilterFromURL: rejects unknown status enum', () => {
  const sp = new URLSearchParams('status=Unset');
  assert.equal(readFilterFromURL(sp).status, '');
});

test('readFilterFromURL: rejects non-numeric min_duration_ms', () => {
  const sp = new URLSearchParams('min_duration_ms=fast');
  assert.equal(readFilterFromURL(sp).minDurationMs, '');
});

test('readFilterFromURL: rejects negative min_duration_ms (no leading sign)', () => {
  const sp = new URLSearchParams('min_duration_ms=-5');
  assert.equal(readFilterFromURL(sp).minDurationMs, '');
});

test('writeFilterToParams: empty filter writes no filter params', () => {
  const out = writeFilterToParams(new URLSearchParams(''), EMPTY_FILTER);
  assert.equal(out.toString(), '');
});

test('writeFilterToParams: each axis materialises in URL', () => {
  const out = writeFilterToParams(new URLSearchParams(''), {
    method: 'PUT',
    status: 'Error',
    path: 'demo',
    minDurationMs: '100',
  });
  // URLSearchParams.toString preserves set-order — assert each pair present.
  const got = Object.fromEntries(out.entries());
  assert.deepEqual(got, {
    method: 'PUT',
    status: 'Error',
    path: 'demo',
    min_duration_ms: '100',
  });
});

test('writeFilterToParams: clearing a previously-set axis deletes the key', () => {
  const initial = new URLSearchParams('method=PUT&status=Error');
  const out = writeFilterToParams(initial, {
    method: '',
    status: 'Error',
    path: '',
    minDurationMs: '',
  });
  assert.equal(out.get('method'), null);
  assert.equal(out.get('status'), 'Error');
});

test('writeFilterToParams: preserves unrelated URL params (other components)', () => {
  const initial = new URLSearchParams('tab=data&unrelated=xyz');
  const out = writeFilterToParams(initial, { ...EMPTY_FILTER, method: 'GET' });
  assert.equal(out.get('tab'), 'data');
  assert.equal(out.get('unrelated'), 'xyz');
  assert.equal(out.get('method'), 'GET');
});

test('isEmptyFilter: distinguishes blank vs any axis set', () => {
  assert.equal(isEmptyFilter(EMPTY_FILTER), true);
  assert.equal(isEmptyFilter({ ...EMPTY_FILTER, method: 'PUT' }), false);
  assert.equal(isEmptyFilter({ ...EMPTY_FILTER, path: 'x' }), false);
  assert.equal(isEmptyFilter({ ...EMPTY_FILTER, minDurationMs: '10' }), false);
});

test('toRecentTracesQuery: omits empty axes', () => {
  assert.deepEqual(toRecentTracesQuery(EMPTY_FILTER, ''), {});
});

test('toRecentTracesQuery: minDurationMs becomes a number', () => {
  const q = toRecentTracesQuery(
    { method: 'PUT', status: '', path: '', minDurationMs: '250' },
    '',
  );
  assert.equal(q.method, 'PUT');
  assert.equal(q.minDurationMs, 250);
  assert.equal(typeof q.minDurationMs, 'number');
});

test('toRecentTracesQuery: prefers debouncedPath over filter.path (race-window snapshot)', () => {
  const q = toRecentTracesQuery({ ...EMPTY_FILTER, path: 'typing-now' }, 'debounced');
  assert.equal(q.path, 'debounced');
});

test('toRecentTracesQuery: bad minDurationMs is dropped', () => {
  const q = toRecentTracesQuery({ ...EMPTY_FILTER, minDurationMs: 'NaN' }, '');
  assert.equal('minDurationMs' in q, false);
});

test('round-trip: write then read returns the same filter', () => {
  const original = {
    method: 'GET',
    status: 'OK',
    path: 'hello world',
    minDurationMs: '5',
  };
  const sp = writeFilterToParams(new URLSearchParams(''), original);
  assert.deepEqual(readFilterFromURL(sp), original);
});
