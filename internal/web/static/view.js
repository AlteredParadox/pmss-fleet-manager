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

/* Pure rendering helpers for the page: no DOM, no state, loaded before
   app.js and exposed as globalThis.pfmView so `node --test` can exercise
   the markup they produce (internal/web/view_test.mjs). Today: the
   overview's cpu / ram bars in the "node vs. you" style, host in grey and
   your slice in blue, and the predicate that says when your share of the
   host is a measurement rather than a zero-initialised field. */
(() => {
'use strict';

function esc(s) { return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]); }
function pct(p) { return `${Math.round(Number(p) || 0)}%`; }
// Small shares keep a decimal: a tenant's slice of a whole host is often under 1%.
function pctFine(p) { const n = Number(p) || 0; return `${n.toFixed(n < 10 ? 1 : 0)}%`; }

/* hostShare: the account's cpu usage as a share of every host core, or null
   when that is not known: no tenant sample yet, or no host core count (the
   server then leaves host_pct at zero, which is not a measurement). */
function hostShare(cpu) {
  return cpu && cpu.ok && Number(cpu.host_cpus) > 0 ? Number(cpu.host_pct) || 0 : null;
}

/* A bar in the "node vs. you" style: the host's usage of the whole machine in
   grey, your slice inside it in blue, both on one 0-100 scale; `cls` colours
   your part (warn / bad). */
function youInHost(hostPct, youPct, title, cls = '') {
  const host = Math.min(100, Math.max(0, Number(hostPct) || 0));
  const you = Math.min(100, Math.max(0, Number(youPct) || 0));
  const inner = host > 0 ? Math.min(100, you / host * 100) : 0;
  return `<div class="lvbar" title="${esc(title)}"><i style="width:${host}%"><i class="${esc(cls)}" style="width:${inner}%"></i></i></div>`;
}

/* ovCpuBar: the overview cpu bar and its caption. Host grey + you blue once
   the host has a cpu reading; until then the quota-relative meter. An
   unknown share shows as a dash, never as 0%. */
function ovCpuBar(cpu, host) {
  const c = cpu || {};
  if (!host || !host.cpu_ok) {
    return { html: `<div class="meter thick"><i style="width:${Math.min(100, Number(c.pct) || 0)}%"></i></div>`, caption: '' };
  }
  const share = hostShare(c);
  const busy = pct(host.cpu_busy_pct);
  return {
    html: youInHost(host.cpu_busy_pct, share ?? 0, `host ${busy} busy · you ${share === null ? '—' : pctFine(share)} of the host`),
    caption: `host ${busy} busy`,
  };
}

/* ovRamBar: the overview ram bar and its caption. Host used (total minus
   available) in grey, your process memory against the host's total in
   blue; without a host memory reading the meter against your limit. */
function ovRamBar(ram, host, cls = '') {
  const r = ram || {};
  if (!host || !(Number(host.mem_total) > 0)) {
    return { html: `<div class="meter"><i class="${esc(cls)}" style="width:${Math.min(100, Number(r.pct) || 0)}%"></i></div>`, caption: '' };
  }
  const hostPct = (1 - (Number(host.mem_avail) || 0) / host.mem_total) * 100;
  const youPct = ((Number(r.rss_bytes) || Number(r.used_bytes) || 0) / host.mem_total) * 100;
  return {
    html: youInHost(hostPct, youPct, `host memory ${pct(hostPct)} used · your processes ${pctFine(youPct)} of it`, cls),
    caption: `host ${pct(hostPct)}`,
  };
}

globalThis.pfmView = { hostShare, youInHost, ovCpuBar, ovRamBar };
})();
