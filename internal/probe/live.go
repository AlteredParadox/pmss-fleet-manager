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

package probe

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// LiveRings are PMSS's live-stats rings (its ADR 0126): one five-minute
// sample per slot for the last 24 hours, of the whole node
// (/var/lib/pmss/public/node-live.json, world-readable) and of this
// account (/var/lib/pmss/live/<user>.json, readable by its owner). They
// are what PMSS's own "Node load vs. your share" panel draws from, so a
// view built on them agrees with it.
type LiveRings struct {
	Node []LiveNodeSample
	User []LiveUserSample
}

// LiveNodeSample is one node slot: CPU-seconds busy (CB, steal inside)
// and total (CT) over DT wall seconds, memory used and total in bytes,
// primary-interface bytes out and in. CS is steal CPU-seconds, present
// only once PMSS writes it (HasCS).
type LiveNodeSample struct {
	T     int64
	DT    float64
	CB    float64
	CT    float64
	MU    float64
	MT    float64
	TX    float64
	RX    float64
	CS    float64
	HasCS bool
}

// LiveUserSample is this account's slot: CPU-seconds of its cgroup over
// the collector's interval, memory charged to it (page cache included),
// bytes out and in.
type LiveUserSample struct {
	T   int64
	CPU float64
	Mem float64
	TX  float64
	RX  float64
}

// liveSlots is the ring length PMSS keeps (24 h of five-minute slots);
// anything longer is a forged file and is trimmed to its newest slots.
const liveSlots = 288

// liveMaxBytes bounds one ring file; a real one is about 40 KB.
const liveMaxBytes = 256 << 10

type liveNodeJSON struct {
	T  *float64 `json:"t"`
	DT *float64 `json:"dt"`
	CB *float64 `json:"cb"`
	CT *float64 `json:"ct"`
	MU *float64 `json:"mu"`
	MT *float64 `json:"mt"`
	TX *float64 `json:"tx"`
	RX *float64 `json:"rx"`
	CS *float64 `json:"cs"`
}

type liveUserJSON struct {
	T   *float64 `json:"t"`
	CPU *float64 `json:"cpu"`
	Mem *float64 `json:"mem"`
	TX  *float64 `json:"tx"`
	RX  *float64 `json:"rx"`
}

// liveMaxAge bounds a slot's time: a ring holds a day, so anything over
// a year old is a stopped cron or a forgery, and a future time (a clock
// stepped back, or milliseconds where seconds belong) would otherwise
// become a time encoding/json refuses and take /api/state down.
const liveMaxAge = 366 * 24 * time.Hour

// parseLiveRings reads the two ring files. The node ring decides whether
// anything is known: without it there is no node to compare with. Samples
// with a missing, negative, non-finite or absurd field, or a time more
// than liveMaxAge before now or a day after it, are dropped; slots are
// sorted by time with duplicates collapsed, and only the newest liveSlots
// remain.
func parseLiveRings(node, user string, now time.Time) (LiveRings, bool) {
	var r LiveRings
	tOK := func(t float64) bool {
		return t >= float64(now.Add(-liveMaxAge).Unix()) && t <= float64(now.Add(24*time.Hour).Unix())
	}
	nodeRaw, ok := liveDecode(node)
	if !ok {
		return r, false
	}
	for _, raw := range nodeRaw {
		var s liveNodeJSON
		if json.Unmarshal(raw, &s) != nil || !liveOK(s.T, s.DT, s.CB, s.CT, s.MU, s.MT, s.TX, s.RX) || !tOK(*s.T) || *s.DT < 1 || *s.CT <= 0 || *s.MT <= 0 {
			continue
		}
		out := LiveNodeSample{T: int64(*s.T), DT: *s.DT, CB: *s.CB, CT: *s.CT, MU: *s.MU, MT: *s.MT, TX: *s.TX, RX: *s.RX}
		if s.CS != nil && liveNum(*s.CS) {
			out.CS, out.HasCS = *s.CS, true
		}
		r.Node = append(r.Node, out)
	}
	sort.SliceStable(r.Node, func(i, j int) bool { return r.Node[i].T < r.Node[j].T })
	r.Node = liveDedupe(r.Node, func(s LiveNodeSample) int64 { return s.T })
	if len(r.Node) > liveSlots {
		r.Node = r.Node[len(r.Node)-liveSlots:]
	}
	if userRaw, ok := liveDecode(user); ok {
		for _, raw := range userRaw {
			var s liveUserJSON
			if json.Unmarshal(raw, &s) != nil || !liveOK(s.T, s.CPU, s.Mem, s.TX, s.RX) || !tOK(*s.T) {
				continue
			}
			r.User = append(r.User, LiveUserSample{T: int64(*s.T), CPU: *s.CPU, Mem: *s.Mem, TX: *s.TX, RX: *s.RX})
		}
		sort.SliceStable(r.User, func(i, j int) bool { return r.User[i].T < r.User[j].T })
		r.User = liveDedupe(r.User, func(s LiveUserSample) int64 { return s.T })
		if len(r.User) > liveSlots {
			r.User = r.User[len(r.User)-liveSlots:]
		}
	}
	return r, len(r.Node) > 0
}

// liveDecode returns the ring's samples still encoded, so that one slot
// that does not decode is dropped on its own rather than taking the ring
// with it; a document that is not {"samples":[...]} is refused whole.
func liveDecode(raw string) ([]json.RawMessage, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > liveMaxBytes {
		return nil, false
	}
	var d struct {
		Samples []json.RawMessage `json:"samples"`
	}
	if json.Unmarshal([]byte(raw), &d) != nil || d.Samples == nil {
		return nil, false
	}
	return d.Samples, true
}

// liveNum accepts a finite, non-negative value below 2^53, where float64
// still counts bytes exactly.
func liveNum(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1<<53 }

func liveOK(vals ...*float64) bool {
	for _, v := range vals {
		if v == nil || !liveNum(*v) {
			return false
		}
	}
	return vals[0] != nil && *vals[0] > 0 // the slot time
}

// liveDedupe keeps the first of equal-time slots in a time-sorted slice:
// PMSS's writer refuses a slot at or before the newest one it holds, so a
// repeat can only be an edit after the fact.
func liveDedupe[S any](in []S, t func(S) int64) []S {
	out := in[:0]
	for i, s := range in {
		if i > 0 && t(in[i-1]) == t(s) {
			continue
		}
		out = append(out, s)
	}
	return out
}
