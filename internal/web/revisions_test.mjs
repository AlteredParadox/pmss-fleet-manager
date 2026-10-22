// Node test (node --test, run by `make jscheck`) for the page's revision
// bookkeeping in static/revisions.js: the orderings a browser produces when
// the event stream and overlapping snapshot requests race (external review
// 3, finding 11). Run: node --test internal/web/
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const src = readFileSync(new URL('./static/revisions.js', import.meta.url), 'utf8');
vm.runInThisContext(src); // defines globalThis.pfmRevisions, as the page's script tag does
const R = globalThis.pfmRevisions;

const box = (id, rev, name = `b${id}`) => ({ id, rev, name });
const fresh = (boxes = [], snapRev = 0) => ({ boxes, snapRev, removed: new Map(), alertsRev: 0, unacked: 0 });
const ids = (S) => S.boxes.map((b) => b.id);

test('a snapshot taken before the newest merged one is refused', () => {
  // A (rev 10, with X=1) is taken, X is removed at 11, B (rev 12, without X)
  // lands first; A lands afterwards and must not put X back.
  const S = fresh([box(1, 9), box(2, 9)], 0);
  // no removed event was seen (the deletion is learned from B alone)
  assert.deepEqual(R.acceptSnapshot(S, [box(2, 9)], 12), { accepted: true, kept: false });
  assert.deepEqual(ids(S), [2]);
  assert.deepEqual(R.acceptSnapshot(S, [box(1, 9), box(2, 9)], 10), { accepted: false, kept: false });
  assert.deepEqual(ids(S), [2]);
  assert.equal(S.snapRev, 12);
});

test('the same with the removed event seen between the two snapshots', () => {
  const S = fresh([box(1, 9), box(2, 9)], 0);
  assert.equal(R.applyRemoved(S, { box_id: 1, rev: 11 }), true);
  assert.deepEqual(ids(S), [2]);
  assert.equal(S.removed.get(1), 11);
  R.acceptSnapshot(S, [box(2, 9)], 12);
  assert.equal(S.removed.has(1), false, 'the tombstone is dropped once a snapshot past it lands');
  assert.equal(R.acceptSnapshot(S, [box(1, 9), box(2, 9)], 10).accepted, false);
  assert.deepEqual(ids(S), [2]);
});

test('a removed event seen before an older snapshot lands keeps the box out', () => {
  // The stream delivers the removal (rev 11) before the snapshot taken at
  // rev 10 arrives: the snapshot still lists X, the tombstone wins.
  const S = fresh([box(1, 9), box(2, 9)], 5);
  R.applyRemoved(S, { box_id: 1, rev: 11 });
  const r = R.acceptSnapshot(S, [box(1, 9), box(2, 9)], 10);
  assert.deepEqual(r, { accepted: true, kept: true });
  assert.deepEqual(ids(S), [2]);
  assert.equal(S.removed.get(1), 11, 'a snapshot older than the removal does not clear the tombstone');
});

test('a state event newer than the snapshot wins over the snapshot', () => {
  const S = fresh([box(1, 15, 'x')], 5);
  const r = R.acceptSnapshot(S, [box(1, 10, 'x')], 12);
  assert.deepEqual(r, { accepted: true, kept: true });
  assert.equal(S.boxes[0].rev, 15);
});

test('a box added after the snapshot was taken stays; one the snapshot lacks and that is older goes', () => {
  const S = fresh([box(1, 13), box(3, 4)], 5);
  R.acceptSnapshot(S, [box(2, 11)], 12);
  assert.deepEqual(ids(S), [1, 2]);
});

test('stale state and removed events are dropped', () => {
  const S = fresh([box(1, 10)], 12);
  assert.equal(R.applyState(S, { box_id: 1, rev: 10, state: box(1, 10) }), false);
  assert.equal(R.applyState(S, { box_id: 1, rev: 9, state: box(1, 9) }), false);
  assert.equal(R.applyState(S, { box_id: 1, rev: 11, state: box(1, 11) }), true);
  assert.equal(S.boxes[0].rev, 11);
  // an unknown box: only an event after the last snapshot is a new box
  assert.equal(R.applyState(S, { box_id: 7, rev: 12, state: box(7, 12) }), false);
  assert.equal(R.applyState(S, { box_id: 7, rev: 13, state: box(7, 13) }), true);
  assert.deepEqual(ids(S), [1, 7]);
  // a replayed removal for a box already removed at a later revision
  R.applyRemoved(S, { box_id: 7, rev: 14 });
  assert.equal(R.applyRemoved(S, { box_id: 7, rev: 13 }), false);
  assert.equal(R.applyState(S, { box_id: 7, rev: 13, state: box(7, 13) }), false, 'nor does its state come back');
  assert.equal(S.removed.get(7), 14);
});

test('a snapshot at the same revision as the last one is merged, not refused', () => {
  const S = fresh([box(1, 10)], 12);
  assert.equal(R.acceptSnapshot(S, [box(1, 10), box(2, 12)], 12).accepted, true);
  assert.deepEqual(ids(S), [1, 2]);
});

test('the alert badge keeps the count with the highest alert revision, from stream or snapshot', () => {
  const S = fresh();
  assert.equal(R.acceptAlerts(S, 2, 5), true);
  assert.equal(S.unacked, 2);
  // an older snapshot landing late
  assert.equal(R.acceptAlerts(S, 7, 4), false);
  assert.equal(S.unacked, 2);
  // a snapshot read under the same revision after a change the next event has yet to announce
  assert.equal(R.acceptAlerts(S, 1, 5), true);
  assert.equal(S.unacked, 1);
  // the event announcing it: same count, nothing changed
  assert.equal(R.acceptAlerts(S, 1, 6), false);
  assert.equal(S.alertsRev, 6);
  // a frame without a count (an old server) changes nothing
  assert.equal(R.acceptAlerts(S, undefined, 9), false);
  assert.equal(S.alertsRev, 6);
});
