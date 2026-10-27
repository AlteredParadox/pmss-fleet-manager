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
   the markup and wording they produce (internal/web/view_test.mjs): the
   overview's cpu / ram meters and host-share predicates, client
   availability and per-torrent controls, add summaries, table sorting,
   the rTorrent DHT / PeX toggle wording and the box/revision a box control
   carries. */
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

// clientTotals sums a box's torrent counts over its monitored clients
// (qBittorrent and rTorrent). A client that is off, or whose block is
// missing from an older snapshot or event, contributes nothing; a client
// that is on but unreachable still contributes its carried counts, as the
// server's totals do. Used by the fleet totals recomputed from SSE events
// and by the per-box cards, so the two never disagree (fred review on #124).
function clientTotals(b) {
  const t = { torrents: 0, seeding: 0, clients: 0 };
  for (const c of [b?.qbt, b?.rtorrent]) {
    if (!c || !c.enabled) continue;
    t.clients++;
    t.torrents += Number(c.total) || 0;
    t.seeding += Number(c.seeding) || 0;
  }
  return t;
}

// torrentControls says which of a row's start / pause / stop buttons apply
// (the others are shown disabled). qBittorrent has no pause of its own
// (5.x renamed pause to stop), so `pause` is null there and no button is
// drawn. qBittorrent reports stopped torrents with problems as kind
// "error" (error, missingFiles), so start is what recovers them; rTorrent's
// "error" is a running torrent with a tracker message, which pause and stop
// manage (daybreak blue review of #124). rTorrent's state tells a paused
// torrent (started, inactive: start resumes it, stop closes it) from a
// stopped or closed one.
function torrentControls(client, kind, state) {
  if (client === 'rtorrent') {
    const stopped = state === 'stopped' || state === 'closed';
    const paused = state === 'paused';
    return { start: stopped || paused, pause: !stopped && !paused, stop: !stopped };
  }
  const startable = kind === 'paused' || kind === 'error';
  return { start: startable, pause: null, stop: !startable };
}

// rateSourceLabel names where the live bandwidth figure comes from.
function rateSourceLabel(source) {
  switch (source) {
    case 'qbt': return 'live from qBittorrent';
    case 'rtorrent': return 'live from rTorrent';
    case 'clients': return 'live from qBittorrent + rTorrent';
    case 'pmss-15min': return 'PMSS 15-min average (refreshed twice an hour)';
    default: return 'no rate source';
  }
}

// clientAvailability says how a monitored client's state should read:
// `down` when it does not answer and that is unexpected, `stopped` when
// the tenant stopped it on purpose (rTorrent's ~/.rtorrentDisable), which
// is neither a warning nor a place to add torrents.
function clientAvailability(st) {
  if (!st || !st.enabled) return { down: false, stopped: false };
  if (st.reachable) return { down: false, stopped: false };
  return { down: !st.disabled, stopped: !!st.disabled };
}

// clientActionable says whether a torrent in client `client` of box b may be
// acted on right now: the client is monitored and not stopped on purpose.
// A row carried over from an earlier poll while the client is stopped keeps
// no live controls (daybreak blue re-review of #124).
function clientActionable(b, client) {
  const st = client === 'rtorrent' ? b?.rtorrent : b?.qbt;
  if (!st || !st.enabled) return false;
  return !clientAvailability(st).stopped;
}

// showClient says whether box b's client `key` gets a section on the
// torrents tab: when it is monitored, or, for rTorrent, when it carries the
// recorded outcome of a failed remove-with-data, which must stay visible and
// dismissable until dismissed even after monitoring was switched off
// (daybreak blue re-review of #124).
function showClient(b, key) {
  const st = key === 'rtorrent' ? b?.rtorrent : b?.qbt;
  if (!st) return false;
  if (st.enabled) return true;
  return key === 'rtorrent' && !!st.remove_failed;
}

// torrentsTab says whether the box gets a torrents tab at all: a monitored
// or installed client, rTorrent seen running or stopped on purpose, or a
// removal notice still to dismiss.
function torrentsTab(b) {
  const q = b?.qbt || {}, r = b?.rtorrent || {};
  return !!(q.enabled || q.installed || r.enabled || r.process_up || r.disabled || r.remove_failed);
}

// addSummary words an add's outcome for the toast. rTorrent: what was
// added (rTorrent acknowledged the load), what it refused, and how many
// loaded torrents have a label that could not be applied or confirmed;
// labels are attempted once and never retried, so nothing is promised about
// them (daybreak blue re-review of #124). qBittorrent: its own counts, or
// what was sent when the server did not count.
function addSummary(r, stopped, client) {
  const tail = stopped ? ' (stopped)' : '';
  if (client === 'rtorrent') {
    const parts = [`${r?.added || 0} added to rTorrent${tail}`];
    if (r?.failed) parts.push(`${r.failed} not added (refused by rTorrent or not a torrent)`);
    if (r?.unlabelled) parts.push(`${r.unlabelled} loaded, label not applied or confirmed`);
    // uncertain is set only when the stopping entry was sent and its reply
    // lost; otherwise that entry was refused before sending (daybreak blue
    // audit of 7506bd4).
    if (r?.stopped_early) {
      parts.push(r.uncertain > 0
        ? `then the batch stopped (${r.stopped_early}); the entry being sent may have been accepted, so check rTorrent before retrying`
        : `then the batch stopped (${r.stopped_early}); the next entry was not sent and the rest were not tried`);
    }
    return parts.join('; ');
  }
  if (!r?.counted) {
    const n = (r?.links || 0) + (r?.files || 0);
    return `${n} torrent${n === 1 ? '' : 's'} sent to qBittorrent${tail}`;
  }
  const parts = [`${r.added} added${tail}`];
  if (r.pending) parts.push(`${r.pending} still being fetched`);
  if (r.failed) parts.push(`${r.failed} not added (already present or not a torrent)`);
  return parts.join(', ');
}

// Torrent table sorting. Each column header sorts by its key; a click on
// a new column sorts it (text A→Z, numbers largest first), a second click
// reverses, a third returns to the default order (active first, busiest
// first, as the server and the fleet list give it). Ties fall back to the
// name so rows do not jump between polls.
const TOR_TEXT_KEYS = new Set(['name', 'box', 'client']);
const CLIENT_NAMES = { qbt: 'qbittorrent', rtorrent: 'rtorrent' }; // a row without one is qBittorrent's
const torName = (t) => String(t?.name || '').toLowerCase();
function torSortValue(t, key) {
  switch (key) {
    case 'size': return Number(t?.size) || 0;
    case 'progress': return Number(t?.progress) || 0;
    case 'up': return Number(t?.up_bps) || 0;
    case 'down': return Number(t?.down_bps) || 0;
    case 'uploaded': return Number(t?.uploaded) || 0;
    case 'downloaded': return Number(t?.downloaded) || 0;
    case 'ratio': return t?.ratio < 0 ? Infinity : Number(t?.ratio) || 0; // ∞: seeded with nothing downloaded
    case 'seeds': return Number(t?.seeds) || 0;
    case 'peers': return Number(t?.peers) || 0;
  }
  return 0;
}
function nextSort(cur, key) {
  if (!cur || cur.key !== key) return { key, dir: TOR_TEXT_KEYS.has(key) ? 'asc' : 'desc' };
  if (cur.dir === (TOR_TEXT_KEYS.has(key) ? 'asc' : 'desc')) return { key, dir: cur.dir === 'asc' ? 'desc' : 'asc' };
  return null;
}
// sortTorrents returns a sorted copy of rows; get maps a row to its torrent
// (identity for a plain list) and boxName to its box's name (the fleet
// list's "seedbox" column; "client" sorts by the client's name). No sort
// keeps the given order.
function sortTorrents(rows, sort, get = (r) => r, boxName = () => '') {
  if (!sort?.key) return rows.slice();
  const sign = sort.dir === 'asc' ? 1 : -1;
  const val = (r) => {
    if (sort.key === 'box') return String(boxName(r) || '').toLowerCase();
    if (sort.key === 'name') return torName(get(r));
    if (sort.key === 'client') { const c = get(r)?.client || 'qbt'; return CLIENT_NAMES[c] || String(c).toLowerCase(); }
    return torSortValue(get(r), sort.key);
  };
  return rows.map((r, i) => [r, val(r), i]).sort((a, b) => {
    if (a[1] !== b[1]) return (a[1] < b[1] ? -1 : 1) * sign;
    const na = torName(get(a[0])), nb = torName(get(b[0]));
    if (na !== nb) return na < nb ? -1 : 1;
    return a[2] - b[2];
  }).map((x) => x[0]);
}

// rtDiscovery describes rTorrent's peer-discovery toggles: DHT and peer
// exchange (rTorrent has no local service discovery). `on` is what the
// toggle sets and shows: the config files' value when they name one (what
// a restart uses, and what a click rewrites), else the live one. `live` is
// the running client's; `pending` is set when the two differ, i.e. the
// change waits for a restart. `warn` (orange) when either is on.
function rtDiscovery(r) {
  if (!r?.reachable) return [];
  const out = [];
  const one = (k, label, live, conf, known = true) => {
    if (!known && !conf) return;
    const on = conf ? conf === 'on' : !!live;
    out.push({ k, label, on, live: known ? !!live : null, pending: known && !!conf && on !== !!live, warn: on || (known && !!live) });
  };
  one('dht', 'DHT', r.dht, r.conf_dht);
  one('pex', 'PeX', r.pex, r.conf_pex, !!r.pex_known);
  return out;
}

// The wording for rTorrent's toggles. DHT is client-wide and running or
// not; protocol.pex is only the default rTorrent copies into each torrent
// when it is loaded, so a torrent already loaded (even one stopped and
// started again) keeps its own PeX setting (fred on PR 126). The copy says
// so rather than presenting the default as every torrent's state.
const rtWhat = (k) => (k === 'pex' ? 'PeX default for new torrents' : 'DHT');
const PEX_KEEP = '; torrents already loaded keep their own PeX setting';
// rtDiscoveryState says what the config and the running client have;
// rtDiscoveryHint adds what a click does (monitor-only mode shows the state
// alone, fred review on PR 134).
function rtDiscoveryState(d) {
  let s = `${rtWhat(d.k)} is ${d.on ? 'on' : 'off'} in the rTorrent config`;
  if (d.pending) s += `, but the running client has it ${d.live ? 'on' : 'off'} until rTorrent restarts`;
  if (d.k === 'pex') s += PEX_KEEP;
  return s;
}
// A box control names the box and the record revision it was drawn from
// (data-box / data-rev), and its request sends them back (boxPath, from the
// control's dataset or an add draft opened from one): a control drawn
// before an edit is refused (409) rather than run on the edited box, even
// when the page has not redrawn yet (fred on PR 136).
function boxRef(b) {
  return `data-box="${Number(b?.id) || 0}" data-rev="${Number(b?.box_rev) || 0}"`;
}
function boxPath(ref, path) {
  return `/api/boxes/${encodeURIComponent(ref?.box ?? '')}/${path}?rev=${encodeURIComponent(ref?.rev ?? '')}`;
}

function rtDiscoveryHint(d) {
  return `${rtDiscoveryState(d)} — click to ${d.on ? 'disable' : 'enable'} (saved to ~/.rtorrent.rc.custom)`;
}
function rtDiscoveryToast(k, on, r) {
  const base = `${rtWhat(k)} ${on ? 'enabled' : 'disabled'}`, keep = k === 'pex' ? PEX_KEEP : '';
  if (!r?.live) return { text: `${base}: ${r?.note || 'saved to ~/.rtorrent.rc.custom'}${keep}`, kind: 'warn' };
  return { text: `${base} (saved to ~/.rtorrent.rc.custom)${keep}`, kind: 'ok' };
}
// rtDiscoveryOn: what is on in the running client, for the fleet list and
// the overview, with a title that keeps DHT (running now) apart from the
// PeX default.
function rtDiscoveryOn(r) {
  const labels = [], why = [];
  if (r?.dht) { labels.push('DHT'); why.push('DHT is running now'); }
  if (r?.pex) { labels.push('PeX'); why.push('PeX is the default for new torrents (torrents already loaded keep their own setting)'); }
  return { labels, title: why.length ? `${why.join('; ')} — private trackers usually forbid these` : '' };
}

globalThis.pfmView = { hostShare, ramHostShare, cpuQuotaMeter, ramLimitMeter, pctFine, clientTotals, torrentControls, rateSourceLabel, clientAvailability, clientActionable, showClient, torrentsTab, addSummary, nextSort, sortTorrents, rtDiscovery, rtDiscoveryState, rtDiscoveryHint, rtDiscoveryToast, rtDiscoveryOn, boxRef, boxPath };
})();
