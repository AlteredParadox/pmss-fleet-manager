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

package collect

import (
	"math"
	"testing"

	"pmss-fleet-manager/internal/probe"
)

func TestLiveView(t *testing.T) {
	const now = int64(1792697161)
	node := func(ago int64, cb, ct, mu, tx float64) probe.LiveNodeSample {
		return probe.LiveNodeSample{T: now - ago, DT: 300, CB: cb, CT: ct, MU: mu, MT: 1000, TX: tx, RX: 0}
	}
	r := probe.LiveRings{
		Node: []probe.LiveNodeSample{node(1200, 50, 100, 500, 0), node(600, 20, 100, 200, 0), node(300, 30, 100, 300, 3.75e7), node(0, 10, 100, 100, 0)},
		User: []probe.LiveUserSample{{T: now - 300, CPU: 3, Mem: 30, TX: 7.5e6}, {T: now, CPU: 1, Mem: 10}},
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.011 }
	st := liveView(r, true)
	if !st.OK || st.At.Unix() != now || len(st.Windows) != 3 {
		t.Fatalf("state = %+v", st)
	}
	// 15 min = slots within (now-900, now]: the ones 600, 300 and 0 s old.
	w := st.Windows[0]
	if w.Samples != 3 || !near(w.CPU.Node, 20) || !near(w.CPU.You, 4.0/3) || !near(w.RAM.Node, 20) || !near(w.RAM.You, 4.0/3) {
		t.Fatalf("15 min = %+v", w)
	}
	// 3.75e7 bytes over 900 s of wall time = 0.33 Mbit/s for the node; the account sent a fifth of it.
	if !near(w.TX.Node, 0.33) || !near(w.TX.You, 0.07) || w.RX.Node != 0 {
		t.Fatalf("traffic = %+v / %+v", w.TX, w.RX)
	}
	if w := st.Windows[2]; w.Samples != 4 || !near(w.CPU.Node, 27.5) || !near(w.CPU.You, 1) {
		t.Fatalf("24 h = %+v", w)
	}
	if len(st.Series.CPU) != 4 || st.Series.CPU[0] != [2]float64{50, 0} || st.Series.CPU[3] != [2]float64{10, 1} {
		t.Fatalf("series = %v", st.Series.CPU)
	}
	if st := liveView(r, false); st.OK {
		t.Fatal("unknown rings give no view")
	}
	if st := liveView(probe.LiveRings{Node: r.Node[:1]}, true); st.Windows[0].Samples != 1 || st.Windows[0].CPU.You != 0 || len(st.Series.RAM) != 1 {
		t.Fatalf("a lone slot without a user sample counts the account as zero: %+v", st.Windows[0])
	}
	// 300 slots fold into at most liveBuckets points.
	var many probe.LiveRings
	for i := int64(0); i < 288; i++ {
		many.Node = append(many.Node, node((287-i)*300, 1, 2, 1, 0))
	}
	if n := len(liveView(many, true).Series.CPU); n > liveBuckets || n < liveBuckets/2 {
		t.Fatalf("series has %d points", n)
	}
}
