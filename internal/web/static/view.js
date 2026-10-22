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
   overview's cpu / ram meters against your own quota and limit, and the
   predicates that say when your share of the host is a measurement rather
   than a zero-initialised field. */
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

/* ramHostShare: your process memory as a share of the host's total, or
   null when either side is unknown (no cgroup sample: the ram fields are
   zero-initialised, not measurements; no host memory reading). */
function ramHostShare(ram, host) {
  const r = ram || {};
  if (!r.ok || !host || !(Number(host.mem_total) > 0)) return null;
  return ((Number(r.rss_bytes) || Number(r.used_bytes) || 0) / host.mem_total) * 100;
}

/* cpuQuotaMeter / ramLimitMeter: the overview bars, your usage against your
   own cpu quota or memory limit (what the tile is for); `cls` colours the
   ram one warn / bad near the limit. An unknown sample draws empty and is
   named with a dash, never as 0%. */
function cpuQuotaMeter(cpu) {
  const c = cpu || {};
  const title = `${c.ok ? pct(c.pct) : '—'} of your ${c.quota_cpus ? `${Number(c.quota_cpus).toFixed(1)}-core quota` : 'host cores'}`;
  return `<div class="meter thick" title="${esc(title)}"><i style="width:${c.ok ? Math.min(100, Math.max(0, Number(c.pct) || 0)) : 0}%"></i></div>`;
}
function ramLimitMeter(ram, cls = '') {
  const r = ram || {};
  const title = `${r.ok ? pct(r.pct) : '—'} of your memory limit`;
  return `<div class="meter" title="${esc(title)}"><i class="${esc(cls)}" style="width:${r.ok ? Math.min(100, Math.max(0, Number(r.pct) || 0)) : 0}%"></i></div>`;
}

globalThis.pfmView = { hostShare, ramHostShare, cpuQuotaMeter, ramLimitMeter, pctFine };
})();
