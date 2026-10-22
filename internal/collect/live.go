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
// along with this program. If not, see <https://www.gnu.org/licenses/>.//
// The summary arithmetic and the row join in this file follow PMSS's
// etc/skel/www/statsLive.php (pmssStatsLiveSummary, pmssStatsLiveRows):
// Copyright (C) MagnaCapax / the PMSS Team, GPL-3.0-only,
// https://github.com/MagnaCapax/PMSS. GPLv3 section 13 permits combining
// that work with this AGPL program; the derived portion remains under
// GPL-3.0 and the rest of this file under the licence above.

package collect

import (
	"math"
	"time"

	"pmss-fleet-manager/internal/probe"
)

// LiveState is the node-versus-you view PMSS's own panel shows ("Node load
// vs. your share", its ADR 0126), computed from its live-stats rings the
// way that panel computes it: your cpu as a share of every host core, ram
// against the host's memory (yours as charged to the cgroup, page cache
// included; the node's as total minus available, so its reclaimable cache
// counts as free and an account can exceed the node), the primary
// interface's bytes as Mbit/s. Averages over 15 min, 1 h and 24 h
// and a 24 h series of up to liveBuckets points. Five-minute samples, so
// it lags the live tiles by up to ten minutes and is refreshed with the
// inventory.
type LiveState struct {
	OK      bool         `json:"ok"`
	At      time.Time    `json:"at"` // newest node sample
	Windows []LiveWindow `json:"windows,omitempty"`
	Series  LiveSeries   `json:"series"`
}

// LiveWindow is one averaging window; Samples is how many five-minute
// slots fed it (0 when the ring does not reach back that far yet).
type LiveWindow struct {
	Label   string   `json:"label"`
	Seconds int      `json:"seconds"`
	Samples int      `json:"samples"`
	CPU     LivePair `json:"cpu"` // percent of the whole host
	RAM     LivePair `json:"ram"` // percent of the host's memory
	TX      LivePair `json:"tx"`  // Mbit/s out
	RX      LivePair `json:"rx"`  // Mbit/s in
}

// LivePair is the node's figure and this account's.
type LivePair struct {
	Node float64 `json:"node"`
	You  float64 `json:"you"`
}

// LiveSeries holds [node, you] per bucket, oldest first.
type LiveSeries struct {
	CPU [][2]float64 `json:"cpu,omitempty"`
	RAM [][2]float64 `json:"ram,omitempty"`
	TX  [][2]float64 `json:"tx,omitempty"`
	RX  [][2]float64 `json:"rx,omitempty"`
}

// liveBuckets is how many points the 24 h series keeps (PMSS draws 96).
const liveBuckets = 96

var liveWindows = []struct {
	label string
	secs  int64
}{{"15 min", 900}, {"1 hour", 3600}, {"24 hours", 86400}}

type liveRow struct {
	node probe.LiveNodeSample
	user probe.LiveUserSample // zero when the account has no slot of that time, as PMSS counts it
}

// liveView builds the state from the rings; not known or empty gives OK false.
func liveView(r probe.LiveRings, known bool) LiveState {
	if !known || len(r.Node) == 0 {
		return LiveState{}
	}
	now := r.Node[len(r.Node)-1].T
	byTime := make(map[int64]probe.LiveUserSample, len(r.User))
	for _, u := range r.User {
		byTime[u.T] = u
	}
	rows := func(window int64) []liveRow {
		var out []liveRow
		for _, n := range r.Node {
			if n.T <= now-window || n.T > now {
				continue
			}
			out = append(out, liveRow{node: n, user: byTime[n.T]})
		}
		return out
	}
	st := LiveState{OK: true, At: time.Unix(now, 0).UTC()}
	for _, w := range liveWindows {
		rs := rows(w.secs)
		lw := LiveWindow{Label: w.label, Seconds: int(w.secs), Samples: len(rs)}
		if len(rs) > 0 {
			lw.CPU, lw.RAM, lw.TX, lw.RX = liveSummary(rs)
		}
		st.Windows = append(st.Windows, lw)
	}
	all := rows(86400)
	if len(all) > 0 {
		per := (len(all) + liveBuckets - 1) / liveBuckets
		for i := 0; i < len(all); i += per {
			cpu, ram, tx, rx := liveSummary(all[i:min(i+per, len(all))])
			st.Series.CPU = append(st.Series.CPU, [2]float64{cpu.Node, cpu.You})
			st.Series.RAM = append(st.Series.RAM, [2]float64{ram.Node, ram.You})
			st.Series.TX = append(st.Series.TX, [2]float64{tx.Node, tx.You})
			st.Series.RX = append(st.Series.RX, [2]float64{rx.Node, rx.You})
		}
	}
	return st
}

// liveSummary averages rows exactly as PMSS's pmssStatsLiveSummary does:
// cpu as busy (or yours) over total CPU-seconds, ram as the mean of each
// slot's used-over-total, traffic as bytes over wall seconds.
func liveSummary(rs []liveRow) (cpu, ram, tx, rx LivePair) {
	var ct, cb, cu, dt, rn, ru, ntx, nrx, utx, urx float64
	for _, r := range rs {
		n, u := r.node, r.user
		mt := math.Max(1, n.MT)
		ct += n.CT
		cb += n.CB
		cu += u.CPU
		dt += n.DT
		rn += n.MU / mt
		ru += u.Mem / mt
		ntx += n.TX
		nrx += n.RX
		utx += u.TX
		urx += u.RX
	}
	c, ct, dt := float64(len(rs)), math.Max(1, ct), math.Max(1, dt)
	pct := func(v float64) float64 { return liveRound(math.Min(100, v*100)) }
	mbit := func(bytes float64) float64 { return liveRound(bytes * 8 / dt / 1e6) }
	return LivePair{pct(cb / ct), pct(cu / ct)}, LivePair{pct(rn / c), pct(ru / c)},
		LivePair{mbit(ntx), mbit(utx)}, LivePair{mbit(nrx), mbit(urx)}
}

// liveRound keeps two decimals: a tenant's share of a host is often well
// under one percent, and the series is 96 points by four metrics by two.
func liveRound(v float64) float64 { return math.Round(v*100) / 100 }
