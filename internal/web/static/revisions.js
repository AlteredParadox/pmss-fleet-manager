// pmss-fleet-manager — a single-pane dashboard for Pulsed Media seedboxes.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

/* Revision bookkeeping for the fleet list, shared by app.js and the Node
   test in internal/web (so it has no DOM and no app state of its own).

   Every stored box state and every state/removed event carries the
   fleet-wide revision it was made at, and a snapshot (/api/state) the
   revision it was taken at. The stream and the snapshot request race each
   other and snapshot requests race one another, so nothing may blindly
   overwrite anything else. The functions below take the page's state
   object, which they read and update through four fields:
     boxes     the fleet list (each box has an id and a rev)
     snapRev   the revision of the newest snapshot merged so far
     removed   Map of box id -> revision of the removed event seen for it
     alertsRev the alert revision the held unacknowledged count is at */
(() => {
'use strict';

const revOf = (x) => x?.rev || 0;

// acceptSnapshot merges a snapshot into the list. One taken before the
// newest already merged is refused outright (it could put back a box
// removed between the two). Otherwise a box is taken from the snapshot
// unless the list holds a newer revision of it, a box only in the list stays
// if it appeared after the snapshot, and a box only in the snapshot is
// dropped if its removal was already seen. kept reports that the stream had
// already moved the list past the snapshot, so its totals are stale too.
function acceptSnapshot(S, boxes, rev) {
  if (rev < S.snapRev) return { accepted: false, kept: false };
  const mine = new Map(S.boxes.map((b) => [b.id, b]));
  const out = [];
  let kept = false;
  for (const b of boxes) {
    const cur = mine.get(b.id);
    mine.delete(b.id);
    if ((S.removed.get(b.id) || 0) > revOf(b)) { kept = true; continue; }
    if (cur && revOf(cur) > revOf(b)) { out.push(cur); kept = true; } else out.push(b);
  }
  for (const cur of mine.values()) if (revOf(cur) > rev) { out.push(cur); kept = true; }
  for (const [id, r] of S.removed) if (r <= rev) S.removed.delete(id); // the snapshot already lacks it
  out.sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id);
  S.boxes = out;
  S.snapRev = rev;
  return { accepted: true, kept };
}

// newer reports whether a state or removed event is news: above the box's
// held revision, or for an unknown box, after the last snapshot and after
// any removal seen for that id (a reconnect can replay an old event).
function newer(S, e) {
  const cur = S.boxes.find((b) => b.id === e.box_id);
  const rev = e.rev || 0;
  if (cur) return rev > revOf(cur);
  return rev > S.snapRev && rev > (S.removed.get(e.box_id) || 0);
}

// applyState stores a state event's box; applyRemoved drops a removed
// event's box and remembers the removal. Both ignore stale events and
// report whether anything changed.
function applyState(S, e) {
  if (!newer(S, e)) return false;
  const i = S.boxes.findIndex((b) => b.id === e.box_id);
  if (i >= 0) S.boxes[i] = e.state; else S.boxes.push(e.state);
  S.boxes.sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id);
  return true;
}
function applyRemoved(S, e) {
  if (!newer(S, e)) return false;
  S.removed.set(e.box_id, e.rev || 0);
  S.boxes = S.boxes.filter((b) => b.id !== e.box_id);
  return true;
}

// acceptAlerts keeps the unacknowledged alert count with the highest alert
// revision seen, whether it arrived on an event or in a snapshot: the two
// race each other and a snapshot request taken earlier can land later. An
// equal revision is taken too (a snapshot reads the count under the same
// revision as the last event, possibly after a change the next event has
// yet to announce). Reports whether the count changed.
function acceptAlerts(S, count, rev) {
  if (typeof count !== 'number' || rev < (S.alertsRev || 0)) return false;
  S.alertsRev = rev;
  if (S.unacked === count) return false;
  S.unacked = count;
  return true;
}

globalThis.pfmRevisions = { acceptSnapshot, newer, applyState, applyRemoved, acceptAlerts };
})();
