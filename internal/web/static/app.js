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

/* Fleet Manager frontend: vanilla JS, hash routing, SSE live updates. */
(() => {
'use strict';

// The page is only ever served at <base-path>/ (the server redirects
// <base-path> to it), so the prefix is whatever precedes that final slash:
// '' at the root, '/fleet' behind a proxy that forwards the sub-path. Taking
// it from the URL rather than baking it into this file also works when the
// proxy strips the prefix before forwarding, since it strips it again here.
const BASE = location.pathname.replace(/\/+$/, '');

const S = {
  view: 'fleet', fleetView: 'cards', tab: 'overview', selId: null,
  boxes: [], totals: {}, unacked: 0, settings: {}, detail: null,
  alerts: [], filter: '', live: false,
  torAdd: null, // open "add torrent" form: { urls, category, stopped, files: File[], busy }
  torrents: null, // fleet-wide torrent list from /api/torrents: [{id, name, status, status_reason, qbt}]
  add: { mode: 'add', step: 1, form: null, errors: {}, detect: null, busy: false, editId: null },
  settingsDraft: null, settingsTouched: {},
};

/* ---------- helpers ---------- */
const $ = (sel, root = document) => root.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const KIB = 1024;
function fmtBytes(n, digits) {
  n = Number(n) || 0;
  if (n < KIB) return `${Math.round(n)} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = -1;
  do { n /= KIB; i++; } while (n >= KIB && i < units.length - 1);
  const d = digits ?? (n >= 100 ? 0 : n >= 10 ? 1 : 2);
  return `${n.toFixed(d)} ${units[i]}`;
}
function fmtBits(bps) {
  const b = (Number(bps) || 0) * 8;
  if (b < 1000) return `${Math.round(b)} b/s`;
  if (b < 1e6) return `${(b / 1e3).toFixed(0)} kb/s`;
  if (b < 1e9) return `${(b / 1e6).toFixed(b < 1e7 ? 1 : 0)} Mb/s`;
  return `${(b / 1e9).toFixed(2)} Gb/s`;
}
function fmtBps(bps) {
  const n = Number(bps) || 0;
  if (n <= 0) return '—';
  if (n < KIB) return `${Math.round(n)} B/s`;
  if (n < KIB * KIB) return `${(n / KIB).toFixed(0)} KiB/s`;
  return `${(n / KIB / KIB).toFixed(n < 10 * KIB * KIB ? 2 : 1)} MiB/s`;
}
function fmtPct(p) { return `${Math.round(Number(p) || 0)}%`; }
const CUR = { EUR: '€', USD: '$', GBP: '£' };
function fmtMoney(v, cur) { const n = Number(v) || 0; return `${CUR[cur] || (cur ? esc(cur) + ' ' : '')}${n.toFixed(2)}`; }
function cycleShort(c) { return ({ monthly: 'mo', quarterly: 'qtr', 'semi-annually': '6 mo', annually: 'yr', biennially: '2 yr', triennially: '3 yr' })[String(c || '').toLowerCase()] || (c || ''); }
function safeHref(u) { return /^https?:\/\//i.test(String(u || '')) ? u : '#'; } // never render javascript: or data: links from API data
function retryNote(b) {
  // next_poll is the zero time ("0001-01-01…", a truthy string) when no retry is scheduled.
  const due = Date.parse(b.next_poll);
  if (b.status === 'disabled' || !b.failures || !Number.isFinite(due) || due <= 0) return '';
  const s = Math.max(0, Math.round((due - Date.now()) / 1000));
  return `retry ${s < 5 ? 'now' : s < 90 ? `in ${s}s` : `in ${Math.round(s / 60)} min`}${b.backoff ? ', backed off' : ''}`;
}
function planOf(b) { return b.plan || b.service?.product || ''; }
// serviceRenewal mirrors the server's EffectiveRenewal: an active or suspended
// (usually overdue) service's next due date is a renewal; a cancelled one's is history.
function serviceRenewal(sv) { return sv && /^(active|suspended)$/i.test(sv.status || '') ? sv.next_due || '' : ''; }
function renewalOf(b) { return b.renewal || serviceRenewal(b.service); }
function fmtDur(sec) {
  sec = Math.max(0, Math.floor(Number(sec) || 0));
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${sec}s`;
}
function fmtAgo(iso) {
  if (!iso) return '—';
  const t = new Date(iso).getTime();
  if (!t || t < 86400e3 * 365) return '—';
  const s = Math.round((Date.now() - t) / 1000);
  if (s < 5) return 'just now';
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}
// Calendar days from today to a YYYY-MM-DD date: 0 on the day itself, -1 the
// day after. Both ends are taken as civil dates in the browser's zone (the
// server does the same in its zone), so the count changes at local midnight
// and a DST day does not shift it.
function daysUntil(ymd) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(ymd || '');
  if (!m) return null;
  const due = Date.UTC(+m[1], +m[2] - 1, +m[3]);
  const dd = new Date(due);
  if (dd.getUTCMonth() !== +m[2] - 1 || dd.getUTCDate() !== +m[3]) return null; // 2026-02-30 is not a date
  const now = new Date();
  const today = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate());
  return Math.round((due - today) / 86400e3);
}
function levelClass(pct, threshold) {
  if (pct >= 100 || pct >= threshold) return 'bad';
  if (pct >= threshold - 15) return 'warn';
  return '';
}
function statusLabel(st) {
  return { online: 'online', degraded: 'degraded', offline: 'offline', pending: 'waiting for first poll', disabled: 'disabled' }[st] || st;
}
function toast(msg, kind = '') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), msg.length > 80 ? 9000 : 4500);
}
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body instanceof FormData) opts.body = body; // multipart: the browser sets the boundary
  else if (body !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const r = await fetch(path, opts);
  let data = null;
  try { data = await r.json(); } catch (e) { /* no body */ }
  if (!r.ok) throw Object.assign(new Error((data?.error ? data.error + (data.output ? ' — ' + data.output.split('\n').slice(-2).join(' · ') : '') : null) || `${r.status} ${r.statusText}`), { status: r.status });
  return data;
}

/* ---------- routing ---------- */
function route() {
  const h = location.hash.replace(/^#\/?/, '');
  const parts = h.split('/').filter(Boolean);
  const view = parts[0] || 'fleet';
  S.view = ['fleet', 'torrents', 'box', 'add', 'alerts', 'settings', 'edit'].includes(view) ? view : 'fleet';
  if (S.view === 'box') {
    S.selId = Number(parts[1]) || null;
    S.tab = parts[2] || 'overview';
    if (S.detail && S.detail.id !== S.selId) S.detail = null; // never show one box's page under another's URL
    void loadDetail(S.selId);
  } else if (S.view === 'edit') {
    S.selId = Number(parts[1]) || null;
    void startEdit(S.selId);
  } else if (S.view === 'add' && S.add.mode !== 'add') {
    resetAdd();
  }
  if (S.view === 'alerts') void loadAlerts();
  if (S.view === 'torrents') void loadTorrents();
  if (S.view === 'settings') { S.settingsDraft = { ...S.settings, pm_api_key: '', clear_pm_api_key: false }; S.settingsTouched = {}; void loadServices(); }
  S.filter = '';
  S.torAdd = null;
  render();
}
const go = (hash) => { location.hash = hash; };

/* ---------- data ---------- */
async function loadState() {
  try {
    const d = await api('GET', BASE + '/api/state');
    if (d.instance && S.instance && d.instance !== S.instance) { location.reload(); return; } // server was rebuilt: pick up new JS/CSS
    S.instance = d.instance || S.instance;
    S.version = d.version; S.sourceUrl = d.source_url;
    S.boxes = d.boxes || [];
    S.totals = d.totals || {};
    S.unacked = d.alerts_unacked || 0;
    S.settings = d.settings || {};
    applyTheme(S.view === 'settings' && S.settingsDraft && S.settingsTouched.theme ? S.settingsDraft.theme : S.settings.theme); // keep an unsaved preview
    if (!S.fleetViewTouched) S.fleetView = S.settings.fleet_view || 'cards';
    S.baseline = true; // a full snapshot has landed at least once
    return true;
  } catch (e) { toast(`state: ${e.message}`, 'bad'); return false; }
}
async function loadDetail(id) {
  if (!id) return;
  try {
    const d = await api('GET', `${BASE}/api/boxes/${id}`);
    if (S.selId !== id) return;
    S.detail = d.state;
    render();
  } catch (e) {
    toast(`box: ${e.message}`, 'bad');
    if (e.status === 404 && S.selId === id) {
      // The box is gone (removed, then Back): drop its last detail and
      // summary so the page says so instead of showing a live-looking copy.
      if (S.detail && S.detail.id === id) S.detail = null;
      S.boxes = S.boxes.filter((b) => b.id !== id);
      recomputeTotals();
      render();
    }
  }
}
let torSeq = 0; // only the newest response may land
let torTimer = 0, torInflight = false, torDirty = false, torLast = 0;
const TOR_MIN_MS = 2000;
// Every box's state event asks for the combined view, and pollers are
// staggered, so events trickle in one at a time. Keep at most one request
// in flight, at most one every TOR_MIN_MS, and one trailing reload for
// whatever arrived during a request.
function loadTorrentsSoon() {
  if (torInflight) { torDirty = true; return; }
  if (torTimer) return;
  const wait = Math.max(0, torLast + TOR_MIN_MS - Date.now());
  torTimer = setTimeout(() => { torTimer = 0; if (S.view === 'torrents') void loadTorrents(); }, wait);
}
// Direct callers (route, resync, actions) share the guard: one in flight,
// and a direct load supersedes a scheduled one.
async function loadTorrents() {
  if (torInflight) { torDirty = true; return; }
  if (torTimer) { clearTimeout(torTimer); torTimer = 0; }
  const seq = ++torSeq;
  torInflight = true; torDirty = false;
  try {
    const d = await api('GET', BASE + '/api/torrents');
    if (seq === torSeq) S.torrents = d.boxes || [];
  } catch (e) { toast(`torrents: ${e.message}`, 'bad'); }
  torInflight = false; torLast = Date.now();
  if (S.view === 'torrents') render();
  if (torDirty) loadTorrentsSoon();
}
async function loadServices() { try { S.services = await api('GET', BASE + '/api/services'); } catch (e) { S.services = { configured: false, services: [], error: e.message }; } if (S.view === 'settings') render(); }
async function loadAlerts() {
  try { S.alerts = await api('GET', BASE + '/api/alerts'); render(); } catch (e) { toast(`alerts: ${e.message}`, 'bad'); }
}
function applyTheme(t) {
  document.documentElement.dataset.theme = t === 'light' ? 'light' : 'dark';
}
let rafPending = false;
// One fleet rebuild per frame, however many box events land in it.
function renderSoon() {
  if (rafPending) return;
  rafPending = true;
  requestAnimationFrame(() => { rafPending = false; render(); });
}
function connectEvents() {
  const es = new EventSource(BASE + '/api/events');
  let wasDown = false;
  let resyncGen = 0;
  // Events missed while disconnected (a box removed during a server restart,
  // say) are only recovered by a full reload, and nothing later will repeat
  // them: keep trying until one lands or the stream drops again.
  const resync = async () => {
    const gen = ++resyncGen;
    for (let wait = 2000; S.live && gen === resyncGen; wait = Math.min(wait * 2, 30000)) {
      if (await loadState()) {
        if (gen !== resyncGen) return;
        if (S.view === 'box' && S.selId) void loadDetail(S.selId); else if (S.view === 'alerts') void loadAlerts(); else if (S.view === 'torrents') void loadTorrents();
        render();
        return;
      }
      await new Promise((r) => setTimeout(r, wait));
    }
  };
  es.onopen = () => {
    S.live = true;
    // Also when the boot-time snapshot failed: the stream alone never
    // delivers settings, totals or boxes that have not polled since.
    if (wasDown || !S.baseline) void resync(); else renderSide();
    wasDown = false;
  };
  es.onerror = () => { S.live = false; wasDown = true; resyncGen++; renderSide(); };
  es.addEventListener('state', (ev) => {
    const e = JSON.parse(ev.data);
    if (typeof e.alerts_unacked === 'number') S.unacked = e.alerts_unacked;
    const i = S.boxes.findIndex((b) => b.id === e.box_id);
    if (i >= 0) S.boxes[i] = e.state; else S.boxes.push(e.state);
    S.boxes.sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id);
    recomputeTotals();
    if (S.view === 'box' && S.selId === e.box_id) void loadDetail(e.box_id);
    else if (S.view === 'torrents') loadTorrentsSoon();
    else if (S.view === 'fleet') renderSoon(); else renderSide();
  });
  es.addEventListener('removed', (ev) => {
    const e = JSON.parse(ev.data);
    S.boxes = S.boxes.filter((b) => b.id !== e.box_id);
    if (S.detail && S.detail.id === e.box_id) S.detail = null;
    if (typeof e.alerts_unacked === 'number') S.unacked = e.alerts_unacked;
    recomputeTotals();
    if ((S.view === 'box' || S.view === 'edit') && S.selId === e.box_id) {
      if (S.removingId !== e.box_id) { toast('that box was removed', 'bad'); }
      go('/fleet');
    } else if (S.view === 'alerts') { void loadAlerts(); } else if (S.view === 'torrents') { loadTorrentsSoon(); } else { render(); }
  });
  es.addEventListener('settings', () => {
    // Saved from another tab: refresh our copy. On the settings form, fields
    // the user has not touched follow the new values so a later save here
    // does not revert them; touched fields and the theme preview stay.
    void loadState().then(() => {
      if (S.view !== 'settings') { render(); return; }
      for (const k of Object.keys(S.settings)) if (!S.settingsTouched[k] && k !== 'pm_api_key') S.settingsDraft[k] = S.settings[k];
      applyTheme(S.settingsDraft.theme);
      render();
    });
  });
  es.addEventListener('services', async (ev) => {
    const e = JSON.parse(ev.data);
    if (typeof e.alerts_unacked === 'number') S.unacked = e.alerts_unacked;
    await loadState();
    if (S.view === 'settings') void loadServices();
    else if (S.view === 'box' && S.selId) void loadDetail(S.selId);
    else render();
  });
  es.addEventListener('alerts', (ev) => {
    const e = JSON.parse(ev.data);
    if (typeof e.alerts_unacked === 'number') S.unacked = e.alerts_unacked;
    if (S.view === 'alerts') void loadAlerts(); else renderSide();
  });
}
function recomputeTotals() {
  const t = { count: 0, online: 0, disk_used: 0, disk_quota: 0, traffic_out: 0, traffic_in: 0, up_bps: 0, down_bps: 0, torrents: 0, seeding: 0, containers: 0, next_renewal: '', last_poll: '',
    monthly_cost: 0, monthly_cost_fleet: S.totals?.monthly_cost_fleet || 0, currency: S.totals?.currency || '' };
  let last = 0;
  const seenSvc = new Set(); // two boxes on one service count it once, as the server does
  for (const b of S.boxes) {
    t.count++;
    if (b.status === 'online' || b.status === 'degraded') t.online++;
    t.disk_used += b.disk.used_bytes; t.disk_quota += b.disk.quota_bytes;
    t.traffic_out += b.traffic.out_month_bytes; t.traffic_in += b.traffic.in_month_bytes;
    t.up_bps += b.rates.up_bps; t.down_bps += b.rates.down_bps;
    t.torrents += b.qbt.total; t.seeding += b.qbt.seeding; t.containers += b.docker.running;
    const r = renewalOf(b);
    if (r && (!t.next_renewal || r < t.next_renewal)) t.next_renewal = r;
    if (b.service && /^active$/i.test(b.service.status) && !seenSvc.has(b.service.id)) { seenSvc.add(b.service.id); t.monthly_cost += b.service.monthly || 0; t.currency = b.service.currency; }
    const lp = new Date(b.last_poll).getTime();
    if (lp > last) last = lp;
  }
  if (last) t.last_poll = new Date(last).toISOString();
  S.totals = t;
}

/* ---------- rendering ---------- */
function render() { renderSide(); renderMain(); }

function renderSide() {
  const t = S.totals || {};
  const navActive = (v) => (S.view === v || (v === 'fleet' && (S.view === 'box' || S.view === 'edit'))) ? 'active' : '';
  $('#side').innerHTML = `
    <div class="brand"><div class="led"></div><div class="name">FLEET<span>.ctl</span></div></div>
    <button class="nav ${navActive('fleet')}" data-go="/fleet"><span>fleet</span></button>
    <button class="nav ${navActive('torrents')}" data-go="/torrents"><span>torrents</span></button>
    <button class="nav ${navActive('add')}" data-go="/add"><span>add box</span></button>
    <button class="nav ${navActive('alerts')}" data-go="/alerts"><span>alerts</span>${S.unacked ? `<span class="badge">${S.unacked}</span>` : ''}</button>
    <button class="nav ${navActive('settings')}" data-go="/settings"><span>settings</span></button>
    <div class="spacer"></div>
    <div class="side-stats">
      <div><span>boxes</span><b>${t.count || 0}</b></div>
      <div><span>online</span><b class="ok">${t.online || 0}</b></div>
      <div><span>storage</span><b>${fmtBytes(t.disk_quota || 0, 1)}</b></div>
      <div><span>last poll</span><b>${fmtAgo(t.last_poll)}</b></div>
      <div class="conn ${S.live ? 'live' : ''}"><span class="dot"></span>${S.live ? 'live updates' : 'reconnecting…'}</div>
    </div>`;
}

function renderMain() {
  const m = $('#main');
  switch (S.view) {
    case 'box': m.innerHTML = viewBox(); break;
    case 'torrents': m.innerHTML = viewTorrents(); break;
    case 'add': case 'edit': m.innerHTML = viewAdd(); break;
    case 'alerts': m.innerHTML = viewAlerts(); break;
    case 'settings': m.innerHTML = viewSettings(); break;
    default: m.innerHTML = viewFleet();
  }
  if (S.view === 'box') {
    const b = curBox();
    if (b?.status === 'disabled') {
      // Polling is off: the server refuses every action, so say so up front.
      m.querySelectorAll('button[data-act^="qbt:"], button[data-act^="tor:"], button[data-act^="tor-add:"], button[data-act^="disc:"], button[data-act^="ctr:"], button[data-act="du"]').forEach((el) => {
        el.disabled = true;
        el.title = 'polling is disabled for this box; enable it to run actions';
      });
    }
  }
  afterRender();
}

/* ----- fleet ----- */
function viewFleet() {
  const t = S.totals || {};
  const renew = t.next_renewal ? `${esc(t.next_renewal.slice(5))} <small>· ${daysUntil(t.next_renewal) < 0 ? 'overdue' : daysUntil(t.next_renewal) + 'd'}</small>` : '—';
  const body = S.boxes.length === 0
    ? `<div class="empty">no boxes yet — <a href="#/add">add your first box</a></div>`
    : (S.fleetView === 'table' ? fleetTable() : fleetCards());
  return `
    <div class="head">
      <div><div class="eyebrow">PMSS</div><h1>Fleet overview</h1></div>
      <div class="actions">
        <div class="seg">
          <button class="${S.fleetView === 'cards' ? 'on' : ''}" data-fleetview="cards">cards</button>
          <button class="${S.fleetView === 'table' ? 'on' : ''}" data-fleetview="table">table</button>
        </div>
        ${S.fleetView === 'cards' ? `<div class="seg" title="card size">${CARD_SIZES.map(([px, l]) => `<button class="${cardMin() === px ? 'on' : ''}" data-cardsize="${px}">${l}</button>`).join('')}</div>` : ''}
        <button data-act="refresh-all">refresh all</button>
        <button class="primary" data-go="/add">+ add box</button>
      </div>
    </div>
    <div class="strip">
      <div><div class="tile-label">total storage</div><div class="tile-value">${fmtBytes(t.disk_used || 0, 1)} <small>/ ${fmtBytes(t.disk_quota || 0, 1)}</small></div></div>
      <div><div class="tile-label">upload, last 30 days</div><div class="tile-value">${fmtBytes(t.traffic_out || 0, 2)} <small><span class="down">↓</span> ${fmtBytes(t.traffic_in || 0, 1)}</small></div></div>
      <div><div class="tile-label">aggregate up / down</div><div class="tile-value"><span class="nw"><span class="up">↑</span>${fmtBits(t.up_bps)}</span> <span class="nw"><span class="down">↓</span>${fmtBits(t.down_bps)}</span></div></div>
      <div><div class="tile-label">torrents</div><div class="tile-value">${t.torrents || 0} <small>${t.seeding || 0} seeding</small></div></div>
      <div><div class="tile-label">containers running</div><div class="tile-value">${t.containers || 0}</div></div>
      <div><div class="tile-label">next renewal</div><div class="tile-value">${renew}</div></div>
      ${t.currency ? `<div><div class="tile-label">monthly cost</div><div class="tile-value">${fmtMoney(t.monthly_cost, t.currency)}${t.monthly_cost_fleet > t.monthly_cost + 0.005 ? ` <small title="including services not added as boxes">/ ${fmtMoney(t.monthly_cost_fleet, t.currency)} total</small>` : ''}</div></div>` : ''}
    </div>
    ${body}`;
}
const CARD_SIZES = [[280, 'S'], [340, 'M'], [420, 'L'], [520, 'XL']];
function cardMin() { return Number(S.settings.card_min_px) || 340; }
function trafficLabel(b) {
  const tr = b.traffic;
  if (!tr.ok) return 'n/a';
  return tr.limit_bytes > 0 ? `${fmtBytes(tr.out_month_bytes, 2)} / ${fmtBytes(tr.limit_bytes, 1)}` : `${fmtBytes(tr.out_month_bytes, 2)} · unlimited`;
}
function trafficPct(b) {
  const tr = b.traffic;
  if (tr.limit_bytes > 0) return Math.min(100, tr.pct);
  return Math.min(100, tr.out_month_bytes / (20 * KIB ** 4) * 100);
}
function fleetCards() {
  const dt = S.settings.disk_threshold_pct || 90, tt = S.settings.traffic_threshold_pct || 90;
  return `<div class="cards" style="--card-min:${cardMin()}px">${S.boxes.map((b) => {
    const tl = b.traffic.limit_bytes > 0;
    return `
    <div class="card click" data-go="/box/${b.id}">
      <div class="card-head">
        <div class="card-title"><span class="led ${b.status}" title="${esc(statusLabel(b.status))}${b.status_reason ? ' — ' + esc(b.status_reason) : ''}"></span><span class="n">${esc(b.name)}</span></div>
        <div class="sub">${esc(b.hostname || b.host)}${b.status_reason ? ` · <span class="${b.status === 'offline' ? 'bad' : 'warn'}">${esc(b.status_reason)}</span>` : ''}${(() => { const n = retryNote(b); return n ? ` · <span class="muted">${esc(n)}</span>` : ''; })()}</div>
        <div class="tags"><span class="tag">${esc(b.type)}</span>${planOf(b) ? `<span class="tag" title="plan">${esc(planOf(b))}</span>` : ''}${b.host_sys.cpu_short ? `<span class="tag" title="${esc(b.host_sys.cpu_model)}">${esc(b.host_sys.cpu_short)}${b.host_sys.hw_encode === 'QSV' || b.host_sys.hw_encode === 'VAAPI' || b.host_sys.hw_encode === 'NVENC' ? ' · ' + esc(b.host_sys.hw_encode) : ''}</span>` : ''}${b.qbt.enabled && b.qbt.version ? `<span class="tag" title="qBittorrent${b.qbt.update_available ? ', update available' : ''}">qbt ${esc(b.qbt.version)}${b.qbt.update_available ? ' ↑' : ''}</span>` : ''}${discoveryTag(b.qbt)}</div>
      </div>
      <div style="display:grid;gap:8px">
        <div><div class="kv"><span>disk</span><b>${b.disk.ok || b.disk.quota_bytes ? `${fmtBytes(b.disk.used_bytes, 1)} / ${fmtBytes(b.disk.quota_bytes, 1)}` : 'n/a'}</b></div>
          <div class="meter"><i class="${levelClass(b.disk.pct, dt)}" style="width:${Math.min(100, b.disk.pct)}%"></i></div></div>
        <div><div class="kv"><span>upload 30d${b.traffic.throttled ? ' <span class="bad">· throttled</span>' : ''}</span><b>${trafficLabel(b)}</b></div>
          <div class="meter"><i class="${tl ? levelClass(b.traffic.pct, tt) : 'dim'}" style="width:${trafficPct(b)}%"></i></div></div>
      </div>
      <div class="card-foot">
        <div><div class="l">up/down</div><div><span class="up">↑</span>${fmtBits(b.rates.up_bps)} <span class="down">↓</span>${fmtBits(b.rates.down_bps)}</div></div>
        <div><div class="l">torrents</div><div>${b.qbt.enabled ? `${b.qbt.total} <span class="muted">r${b.qbt.global_ratio ? b.qbt.global_ratio.toFixed(2) : '—'}</span>` : (b.docker.enabled ? `${b.docker.running} <span class="muted">containers</span>` : '—')}</div></div>
        <div><div class="l">cpu/ram</div><div>${b.cpu.ok ? fmtPct(b.cpu.pct) : '—'} / ${b.ram.ok ? fmtPct(b.ram.pct) : '—'}</div></div>
      </div>
    </div>`;
  }).join('')}</div>`;
}
function fleetTable() {
  const dt = S.settings.disk_threshold_pct || 90, tt = S.settings.traffic_threshold_pct || 90;
  return `<div class="table fleet-table">
    <div class="trow head"><span>box</span><span>type</span><span>host cpu</span><span>disk</span><span>upload 30d</span><span>up / down</span><span>torrents</span><span>cpu/ram</span><span>host up</span></div>
    ${S.boxes.map((b) => `
    <div class="trow click" data-go="/box/${b.id}">
      <div style="display:flex;align-items:center;gap:8px;min-width:0"><span class="led ${b.status}"></span><div class="ellip"><div class="ellip" style="font-weight:600">${esc(b.name)}</div><div class="sub">${esc(b.hostname || b.host)}</div></div></div>
      <span class="muted">${esc(b.type)}</span>
      <span class="ellip" title="${esc(b.host_sys.cpu_model || '')}">${esc(b.host_sys.cpu_short || '—')}${b.host_sys.hw_encode ? ` <span class="${b.host_sys.gpu_access ? 'ok' : 'warn'}" style="font-size:10px">${esc(b.host_sys.hw_encode)}</span>` : ''}</span>
      <div><div style="font-size:11px">${b.disk.quota_bytes ? `${fmtBytes(b.disk.used_bytes, 1)} / ${fmtBytes(b.disk.quota_bytes, 1)}` : 'n/a'}</div><div class="meter" style="margin-top:4px;height:3px"><i class="${levelClass(b.disk.pct, dt)}" style="width:${Math.min(100, b.disk.pct)}%"></i></div></div>
      <div><div style="font-size:11px">${trafficLabel(b)}</div><div class="meter" style="margin-top:4px;height:3px"><i class="${b.traffic.limit_bytes > 0 ? levelClass(b.traffic.pct, tt) : 'dim'}" style="width:${trafficPct(b)}%"></i></div></div>
      <span class="num"><span class="up">↑</span>${fmtBits(b.rates.up_bps)} <span class="down">↓</span>${fmtBits(b.rates.down_bps)}</span>
      <span class="num">${b.qbt.enabled ? `${b.qbt.total} <span class="muted">r${b.qbt.global_ratio ? b.qbt.global_ratio.toFixed(2) : '—'}</span>` : '—'}</span>
      <span class="num">${b.cpu.ok ? fmtPct(b.cpu.pct) : '—'} / ${b.ram.ok ? fmtPct(b.ram.pct) : '—'}</span>
      <span class="muted">${b.host_sys.uptime_sec ? fmtDur(b.host_sys.uptime_sec) : '—'}</span>
    </div>`).join('')}
  </div>`;
}

/* ----- box detail ----- */
// The selected box: its loaded detail when it is for this id, else the fleet summary.
function curBox() {
  const d = S.detail && S.detail.id === S.selId ? S.detail : null;
  return d || S.boxes.find((x) => x.id === S.selId);
}
function viewBox() {
  const b = curBox();
  if (!b) return `<button class="crumb" data-go="/fleet">← fleet</button><div class="empty">box not found</div>`;
  const tabs = [['overview', 'overview'], ['traffic', 'traffic']];
  if (b.qbt.enabled || b.qbt.installed) tabs.push(['torrents', 'torrents']);
  if (b.docker.enabled || b.docker.socket_up) tabs.push(['containers', 'containers']);
  tabs.push(['services', 'services'], ['storage', 'storage'], ['host', 'host']);
  if (!tabs.some(([k]) => k === S.tab)) S.tab = 'overview';
  const renewDays = daysUntil(renewalOf(b));
  let body = '';
  switch (S.tab) {
    case 'traffic': body = tabTraffic(b); break;
    case 'torrents': body = tabTorrents(b); break;
    case 'containers': body = tabContainers(b); break;
    case 'services': body = tabServices(b); break;
    case 'storage': body = tabStorage(b); break;
    case 'host': body = tabHost(b); break;
    default: body = tabOverview(b, renewDays);
  }
  return `
    <button class="crumb" data-go="/fleet">← fleet</button>
    ${boxHeader(b)}
    ${boxTabStrip(tabs)}
    ${body}`;
}
// restart/stop (client running) or start (client down) buttons; nothing when monitoring is off.
function boxPowerBtn(b, q) {
  return q.enabled ? (q.process_up
    ? `<button data-act="qbt:restart" data-confirm="Restart qbittorrent-nox on ${esc(b.name)}? It is asked to exit cleanly, then started again with the box's start command (default: ~/bin/qbt-start when present)">${q.restart_needed ? '<span class="warn">restart client</span>' : 'restart client'}</button><button data-act="qbt:stop" data-confirm="Stop qbittorrent-nox on ${esc(b.name)}? Torrents stop seeding until it is started again (the cron helper restarts it within 5 min if installed).">stop client</button>`
    : `<button data-act="qbt:start">start client</button>`) : '';
}
function boxInstallBtn(b, q) {
  if (q.pmss_managed) return '';
  return (q.enabled || q.installed || b.type === 'seedbox')
    ? `<button data-act="qbt:install" data-confirm="${q.installed ? 'Download the latest qbittorrent-nox build and replace ~/bin/qbittorrent-nox on ' + esc(b.name) + '? The running instance keeps the old version until restarted.' : 'Download qbittorrent-nox to ~/bin on ' + esc(b.name) + '?'}">${q.installed ? (q.update_available ? `<span class="warn">update client → ${esc(q.latest_version)}</span>` : 'update client') : 'install client'}</button>` : '';
}
function boxActions(b) {
  return `<div class="actions">
        ${b.status === 'disabled' ? '' : `<button data-act="refresh" title="${b.failures ? 'reconnect and poll now, ignoring the backoff' : 'poll now'}">${b.failures ? 'retry now' : 'refresh'}</button>`}
        ${boxInstallBtn(b, b.qbt)}
        ${boxPowerBtn(b, b.qbt)}
        <button data-go="/edit/${b.id}">edit</button>
        <button class="danger" data-act="remove" data-confirm="Remove ${esc(b.name)} from the fleet? Its alerts are deleted. Nothing on the box is touched.">remove</button>
      </div>`;
}
function boxStatusLine(b) {
  return b.status_reason ? `<div class="meta"><span class="${b.status === 'offline' ? 'bad' : 'warn'}">${esc(statusLabel(b.status))} — ${esc(b.status_reason)}</span>${b.failures ? `<span class="muted" title="${b.backoff ? 'after repeated failures the dashboard retries less often, up to every 10 minutes; use retry now to reconnect immediately' : 'retrying at the normal interval'}">${retryNote(b) ? esc(retryNote(b)) + ' · ' : ''}${b.failures} failed</span>` : ''}</div>` : '';
}
// Title, meta line, status line and the action buttons of the box detail page.
function boxHeader(b) {
  return `<div class="head" style="align-items:flex-start;margin-bottom:20px">
      <div>
        <div style="display:flex;align-items:center;gap:10px"><span class="led ${b.status}" style="width:10px;height:10px"></span><h1 style="margin:0">${esc(b.name)}</h1><span class="tag">${esc(b.type)}</span></div>
        <div class="meta"><span>${esc(b.hostname || b.host)}</span><span>${esc(b.user)}@${esc(b.host)}:${b.port}</span>${b.datacenter ? `<span>${esc(b.datacenter)}</span>` : ''}${planOf(b) ? `<span>${esc(planOf(b))}</span>` : ''}</div>
        ${boxStatusLine(b)}
      </div>
      ${boxActions(b)}
    </div>`;
}
function boxTabStrip(tabs) {
  return `<div class="tabs">${tabs.map(([k, l]) => `<button class="${S.tab === k ? 'on' : ''}" data-tab="${k}">${l}</button>`).join('')}</div>`;
}
function tabOverview(b, renewDays) {
  const dt = S.settings.disk_threshold_pct || 90, tt = S.settings.traffic_threshold_pct || 90;
  return `<div class="grid stats">
    ${ovDiskPanel(b, dt)}
    ${ovUploadPanel(b, tt)}
    ${ovBandwidthPanel(b)}
    ${ovCpuRamPanel(b)}
  </div>
  <div class="grid wide">
    ${ovBoxPanel(b, b.qbt)}
    ${ovPlanBillingPanel(b, renewDays)}
  </div>`;
}
/* overview tiles */
function ovDiskPanel(b, dt) {
  return `<div class="panel"><div class="tile-label">disk</div>
      <div class="tile-value">${fmtBytes(b.disk.used_bytes, 2)} <small>/ ${b.disk.quota_bytes ? fmtBytes(b.disk.quota_bytes, 1) : 'n/a'}</small></div>
      <div class="meter thick"><i class="${levelClass(b.disk.pct, dt)}" style="width:${Math.min(100, b.disk.pct)}%"></i></div>
      <div class="note">${b.disk.quota_bytes ? `${fmtPct(b.disk.pct)} used · ${fmtBytes(Math.max(0, b.disk.quota_bytes - b.disk.used_bytes), 1)} free` : 'quota unknown'}${b.disk.source === 'file' ? ` · snapshot ${fmtDur(b.disk.age_sec)} old` : ''}</div></div>`;
}
function ovUploadPanel(b, tt) {
  const tr = b.traffic;
  const trNote = !tr.ok ? 'no traffic data yet' : tr.limit_bytes > 0
    ? `${fmtPct(tr.pct)} of limit · rolling 30 days${tr.bonus_bytes ? ` · incl. ${fmtBytes(tr.bonus_bytes, 0)} bonus` : ''}${tr.throttled ? ' · <span class="bad">throttled</span>' : ''}`
    : 'no monthly cap on this account · rolling 30 days';
  return `<div class="panel"><div class="tile-label">upload, last 30 days</div>
      <div class="tile-value">${fmtBytes(tr.out_month_bytes, 2)} <small>/ ${tr.limit_bytes > 0 ? fmtBytes(tr.limit_bytes, 1) : 'unlimited'}</small></div>
      <div class="meter thick"><i class="${tr.limit_bytes > 0 ? levelClass(tr.pct, tt) : 'dim'}" style="width:${trafficPct(b)}%"></i></div>
      <div class="note">${trNote}</div></div>`;
}
function ovBandwidthPanel(b) {
  const tr = b.traffic;
  const rateNote = b.rates.source === 'qbt' ? 'live from qBittorrent' : b.rates.source === 'pmss-15min' ? 'PMSS 15-min average (refreshed twice an hour)' : 'no rate source';
  // PMSS's cap_mbit is the upload rate the account is held to while it is
  // over its traffic limit, not a standing cap: only meter against it then.
  const cap = tr.throttled && tr.cap_mbit ? tr.cap_mbit : 0;
  return `<div class="panel"><div class="tile-label">bandwidth now</div>
      <div class="tile-value"><span class="nw"><span class="up">↑</span> ${fmtBits(b.rates.up_bps)}</span> <small class="nw"><span class="down">↓</span> ${fmtBits(b.rates.down_bps)}</small></div>
      ${cap ? `<div class="meter thick" title="upload vs. the ${cap} Mbit/s it is throttled to"><i style="width:${Math.min(100, (b.rates.up_bps / (cap * 1e6)) * 100)}%"></i></div>` : ''}
      <div class="note">${rateNote}${cap ? ` · throttled to ${cap} Mbit/s` : ''}</div></div>`;
}
function ovCpuRamPanel(b) {
  return `<div class="panel"><div class="tile-label">cpu / ram</div>
      <div class="tile-value">${b.cpu.ok ? fmtPct(b.cpu.pct) : '—'} <small>cpu${b.cpu.quota_cpus ? ` · of ${b.cpu.quota_cpus.toFixed(1)} cores` : b.cpu.host_cpus ? ` · of ${b.cpu.host_cpus} host cores` : ''}</small></div>
      <div class="meter thick"><i style="width:${Math.min(100, b.cpu.pct || 0)}%"></i></div>
      <div class="note" style="display:flex;justify-content:space-between;gap:8px"><span title="process memory (rss) against the slice limit; page cache is charged to the slice too but is reclaimed, not OOM-killed">ram ${b.ram.ok ? fmtPct(b.ram.pct) : '—'} · ${b.ram.rss_bytes ? `${fmtBytes(b.ram.rss_bytes, 1)} proc + ${fmtBytes(b.ram.cache_bytes, 1)} cache` : fmtBytes(b.ram.used_bytes, 1)} / ${b.ram.limit_bytes ? fmtBytes(b.ram.limit_bytes, 1) : '?'}${b.ram.last_oom && Date.now() - Date.parse(b.ram.last_oom) < 864e5 ? ` · <span class="bad" title="kernel OOM kill noticed ${esc(b.ram.last_oom)}">OOM kill</span>` : ''}</span><span>${b.ram.pids ? `${b.ram.pids} proc` : ''}</span></div>
      <div class="meter" style="margin-top:4px"><i class="${levelClass(b.ram.pct, 95)}" style="width:${Math.min(100, b.ram.pct || 0)}%"></i></div></div>`;
}
/* overview "box" panel rows */
function ovHostStatusRows(b) {
  return `<div><span>status</span><span class="${b.status === 'online' ? 'ok' : b.status === 'offline' ? 'bad' : 'warn'}">${esc(statusLabel(b.status))}</span></div>
      <div><span>last poll</span><span>${fmtAgo(b.last_poll)}${b.poll_ms ? ` <span class="muted">· ${b.poll_ms} ms</span>` : ''}</span></div>`;
}
function ovClientRows(q) {
  return `<div><span>client</span><span>${q.enabled ? `qBittorrent ${esc(q.version || '')}${q.reachable ? '' : ' <span class="warn">(not responding)</span>'}` : (q.installed ? 'qbittorrent-nox present, monitoring off' : '—')}${q.pmss_managed ? ' · <span class="muted" title="qBittorrent is switched on in the Pulsed Media panel: PMSS runs, restarts and configures it, so install and autostart here are off">panel-managed</span>' : ''}</span></div>
      ${q.enabled && !q.reachable && q.error ? `<div><span>client error</span><span class="warn" style="text-align:right">${esc(q.error)}</span></div>` : ''}`;
}
// Incoming connectivity as libtorrent reports it, with the listen port peers
// must reach; "firewalled" right after a start is normal until a peer dials in.
function ovConnectionRow(q) {
  if (!q.enabled || !q.reachable || !q.connection) return '';
  const port = q.listen_port ? ` · listen port ${q.listen_port}` : '';
  if (q.connection === 'connected') return `<div><span>connection</span><span>incoming OK${port}</span></div>`;
  const since = q.firewalled_since ? ` since ${fmtAgo(q.firewalled_since)}` : '';
  return `<div><span>connection</span><span class="warn" title="${q.connection === 'firewalled' ? 'no peer has connected inbound yet: expected for a few minutes after a start, otherwise the listen port is wrong or not the one Pulsed Media assigned' : 'libtorrent reports no network connection'}">${q.connection === 'firewalled' ? 'firewalled' : 'disconnected'}${since}${port}</span></div>`;
}
function ovBinaryRow(q) {
  return `<div><span>binary</span><span>${q.binary_version ? esc(q.binary_version) : (q.installed ? 'present' : 'not installed')}${q.latest_version ? ` <span class="muted">· latest ${esc(q.latest_version)}</span>` : ''}${q.update_available ? ' · <span class="warn">update available</span>' : ''}${q.restart_needed ? ' · <span class="warn">restart to apply</span>' : ''}</span></div>`;
}
function ovAutostartRow(b, q) {
  if (q.pmss_managed) return `<div><span>autostart</span><span class="muted">Pulsed Media watchdog</span></div>`;
  return `<div><span>autostart</span><span>${q.start_helper && q.cron_installed ? 'qbt-start + cron' : q.start_helper ? 'qbt-start present, <span class="warn">no cron</span>' : '<span class="warn">not set up</span>'} <button class="small ghost" data-act="qbt:autostart" data-confirm="Write ~/bin/qbt-start (if missing) and set the @reboot and */5 crontab entries on ${esc(b.name)}? Other crontab lines are kept.">${q.start_helper && q.cron_installed ? 'reapply' : 'set up'}</button></span></div>`;
}
// binary + autostart rows: shown whenever the client is monitored or at least present on the box.
function ovAutostartRows(b, q) {
  return (q.enabled || q.installed) ? `${ovBinaryRow(q)}
      ${ovAutostartRow(b, q)}` : '';
}
function ovWebuiAuthRow(q) {
  return q.enabled ? `<div><span>webui auth</span><span class="muted">${q.local_bypass ? 'localhost bypass on' : q.subnet_bypass ? `subnet bypass (${esc(q.subnet_list || '')})` : 'login required'}${q.has_creds ? ' · credentials set' : ''}${q.conf_user ? ` · user ${esc(q.conf_user)}` : ''}${q.auth_hold ? ' · <span class="warn" title="A login was rejected. To avoid qBittorrent banning the box\'s loopback IP, no further attempts are made until you re-enter the password under edit or restart the client.">login suspended</span>' : ''}</span></div>` : '';
}
function ovTorrentsRow(q) {
  return q.enabled ? `<div><span>torrents</span><span>${q.total} total · ${q.seeding} seeding · ratio ${q.global_ratio ? q.global_ratio.toFixed(2) : '—'}</span></div>` : '';
}
function ovDockerRow(b) {
  return b.docker.enabled ? `<div><span>containers</span><span>${b.docker.running} running${b.docker.stopped ? ` · ${b.docker.stopped} stopped` : ''}</span></div>` : '';
}
// The detected applications in one line: everything but the sessions and
// the user's systemd, then whatever is watched and down.
function ovServicesRow(b) {
  const A = b.apps || {};
  if (!A.ok) return '';
  const up = (A.apps || []).filter((a) => a.running && a.category !== 'shell').map((a) => esc(a.name));
  const down = (A.apps || []).filter((a) => a.watched && !a.running).map((a) => `<span class="bad">${esc(a.name)} down</span>`);
  const list = up.slice(0, 6).join(', ') + (up.length > 6 ? ` +${up.length - 6}` : '');
  return `<div><span>services</span><span>${[list, ...down].filter(Boolean).join(' · ') || '—'} <span class="muted">· ${A.procs} proc${A.procs === 1 ? '' : 's'}</span></span></div>`;
}
function ovHostAccountRows(b) {
  return `<div><span>account</span><span>${b.account.ok ? `${b.account.quota_gib} GiB · ${b.account.ram_mib} MiB RAM${b.account.docker_enabled ? ' · docker' : ''}${b.account.suspended ? ' · <span class="bad">suspended</span>' : ''}` : '—'}</span></div>
      ${b.host_key_fp ? `<div><span>host key</span><span class="muted" title="${esc(b.host_key_fp)}">${esc(b.host_key_fp.slice(0, 22))}…</span></div>` : ''}`;
}
function ovBoxPanel(b, q) {
  return `<div class="panel rows"><div class="tile-label">box</div>
      ${ovHostStatusRows(b)}
      ${ovClientRows(q)}
      ${ovConnectionRow(q)}
      ${ovAutostartRows(b, q)}
      ${ovWebuiAuthRow(q)}
      ${ovTorrentsRow(q)}
      ${ovDockerRow(b)}
      ${ovServicesRow(b)}
      ${ovHostAccountRows(b)}
    </div>`;
}
function ovPlanBillingPanel(b, renewDays) {
  return `<div class="panel rows"><div class="tile-label">plan &amp; billing</div>
      <div><span>plan</span><span>${esc(planOf(b) || '—')}${b.service && !b.plan ? ' <span class="muted" title="from the Pulsed Media API">·api</span>' : ''}</span></div>
      <div><span>renews</span><span class="${renewDays !== null && renewDays <= (S.settings.renewal_warn_days || 7) ? 'warn' : ''}">${renewalOf(b) ? `${esc(renewalOf(b))} (${renewDays < 0 ? 'overdue' : renewDays + 'd'})` : '—'}</span></div>
      ${b.service ? billingRows(b.service) : `<div><span>billing</span><span class="muted">${S.settings.has_pm_api_key ? 'no matching service in the API feed' : 'set a Pulsed Media API key in settings'}</span></div>`}
      <div><span>datacenter</span><span>${esc(b.datacenter || '—')}</span></div>
      <div><span>links</span><span>${boxLinks(b).map(([l, u]) => `<a href="${esc(safeHref(u))}" target="_blank" rel="noopener">${l} ↗</a>`).join(' · ')}</span></div>
    </div>`;
}
// [label, url] pairs for the links row: the Pulsed Media API's service links
// when a service is matched, then the box's own per-user pages.
function boxLinks(b) {
  const sv = b.service || {};
  const out = [];
  if (sv.login_url) out.push(['web ui', sv.login_url]);
  if (sv.permanent_address) out.push(['permanent address', sv.permanent_address]);
  out.push(['data', userUrl(b, 'data/')]);
  if (b.type === 'seedbox') out.push(['qbittorrent', userUrl(b, 'qbittorrent')]);
  return out;
}
function billingRows(sv) {
  const u = sv.usage;
  return `
      <div><span>billing</span><span>${fmtMoney(sv.recurring_amount, sv.currency)} / ${esc(cycleShort(sv.billing_cycle))}${sv.monthly && cycleShort(sv.billing_cycle) !== 'mo' ? ` <span class="muted">· ${fmtMoney(sv.monthly, sv.currency)}/mo</span>` : ''}${sv.status && sv.status !== 'Active' ? ` · <span class="warn">${esc(sv.status)}</span>` : ''}</span></div>
      <div><span>service</span><span><span class="muted">#${sv.id}${sv.registered ? ` · since ${esc(sv.registered)}` : ''}</span></span></div>
      ${u ? `<div><span>pm traffic</span><span class="muted" title="as tracked by Pulsed Media since ${esc(u.tracked_since || '?')}">↑ ${fmtBytes(u.sent_bytes, 1)} · ↓ ${fmtBytes(u.received_bytes, 1)}</span></div>
      <div><span>pm usage</span><span class="muted">${fmtBytes(u.disk_used_bytes, 1)} disk · ${u.cpu_hours == null ? '—' : Number(u.cpu_hours).toFixed(1)} cpu h · ${u.torrents} torrents</span></div>` : ''}`;
}
function tabTraffic(b) {
  const tr = b.traffic;
  const days = tr.daily || [];
  const max = Math.max(1, ...days.map((d) => d.out_mib + d.in_mib));
  const sumOut = days.reduce((a, d) => a + d.out_mib, 0), sumIn = days.reduce((a, d) => a + d.in_mib, 0);
  const MIB = KIB * KIB;
  const nice = niceMax(max);
  const ticks = [0, 0.5, 1].map((f) => f * nice);
  const bars = days.map((d, i) => {
    const up = d.out_mib / nice * 100, dn = d.in_mib / nice * 100;
    const label = `${d.date}  ↑ ${fmtBytes(d.out_mib * MIB, 1)}  ↓ ${fmtBytes(d.in_mib * MIB, 1)}`;
    return `<div class="bar ${i === days.length - 1 ? 'today' : ''}" data-tip="${esc(label)}" style="height:100%">
      ${d.out_mib > 0 ? `<div class="seg up" style="height:${up}%"></div>` : ''}
      ${d.in_mib > 0 ? `<div class="seg down" style="height:${dn}%"></div>` : ''}
      ${d.out_mib + d.in_mib === 0 ? `<div class="seg" style="height:1px;background:var(--line)"></div>` : ''}
    </div>`;
  }).join('');
  return `<div class="chart">
    <div class="chart-head">
      <div><div class="tile-label">last 30 days · daily transfer (PMSS accounting)</div>
        <div style="font-size:18px;font-weight:600;margin-top:2px">${fmtBytes(tr.out_month_bytes, 2)} <small class="muted" style="font-size:12px;font-weight:400">uploaded of ${tr.limit_bytes > 0 ? fmtBytes(tr.limit_bytes, 1) : 'unlimited'} · rolling window</small></div></div>
      <div class="legend"><span><i style="background:var(--s-up)"></i>upload</span><span><i style="background:var(--s-down)"></i>download</span></div>
    </div>
    ${!tr.ok ? `<div class="empty">no traffic data from PMSS yet — ~/.trafficData is written twice an hour</div>` : `
    <div class="plot">
      <div class="yaxis">${ticks.map((t) => `<span style="top:${100 - t / nice * 100}%">${fmtBytes(t * MIB, 0)}</span>`).join('')}</div>
      <div class="bars" id="bars">${[0.5].map((f) => `<div class="grid-line" style="bottom:${f * 100}%"></div>`).join('')}${bars}</div>
      <div class="xaxis"><span>${days[0]?.date?.slice(5) || ''}</span><span>today</span></div>
    </div>`}
    <div class="ministrip">
      <div><div class="tile-label">uploaded (30d)</div><div class="v">${fmtBytes(sumOut * MIB, 2)}</div></div>
      <div><div class="tile-label">downloaded (30d)</div><div class="v">${fmtBytes(sumIn * MIB, 2)}</div></div>
      <div><div class="tile-label">daily average</div><div class="v">${fmtBytes((sumOut + sumIn) / Math.max(1, days.length) * MIB, 1)}</div></div>
      <div><div class="tile-label">today</div><div class="v"><span class="up">↑</span>${fmtBytes(tr.out_day_bytes, 1)} <span class="muted" style="font-size:12px"><span class="down">↓</span>${fmtBytes(tr.in_day_bytes, 1)}</span></div></div>
      <div><div class="tile-label">limit</div><div class="v">${tr.limit_bytes > 0 ? fmtBytes(tr.limit_bytes, 1) : 'unlimited'}${tr.bonus_bytes ? `<div class="hint">${fmtBytes(tr.base_limit_bytes, 0)} base + ${fmtBytes(tr.bonus_bytes, 0)} bonus</div>` : ''}</div></div>
    </div>
    <div class="hint" style="margin-top:12px">PMSS counts a rolling 30-day window, not a billing cycle. Upload is what counts toward the limit.</div>
  </div>`;
}
function niceMax(v) {
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const m = v / p;
  const n = m <= 1 ? 1 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 5 ? 5 : 10;
  return n * p;
}
function tabTorrents(b) {
  const q = b.qbt;
  if (!q.enabled) return `<div class="empty">qBittorrent monitoring is off for this box — enable it under <a href="#/edit/${b.id}">edit</a>.</div>`;
  const f = S.filter.trim().toLowerCase();
  let list = q.torrents || [];
  if (f) list = list.filter((t) => torrentMatches(t, f));
  const shown = list.slice(0, 300);
  return `<div class="table tor-table" style="overflow:hidden">
    <div class="thead-tools">
      <div style="font-size:12px"><b>qBittorrent ${esc(q.version || '')}</b> <span class="muted">· ${q.total} torrents · ${q.seeding} seeding · ${q.leeching} leeching · ${q.paused} paused${q.errored ? ` · <span class="bad">${q.errored} error</span>` : ''}${transferNote(q.torrents || [])}${discoveryNote(q, b)}${connectionNote(q)}${q.reachable ? '' : ` · <span class="warn">not responding${q.error ? ': ' + esc(q.error) : ''}</span>`}</span></div>
      <div class="actions">
        <input class="filter" placeholder="filter…" data-filter value="${esc(S.filter)}">
        <button class="small" data-act="qbt:pause" data-confirm="Pause all torrents on ${esc(b.name)}?">pause all</button>
        <button class="small" data-act="qbt:resume">resume all</button>
        <button class="small" data-act="tor-add:toggle">${S.torAdd ? 'close' : 'add'}</button>
      </div>
    </div>
    ${S.torAdd ? torAddForm(b, q.torrents || []) : ''}
    <div style="overflow:auto">
      ${TOR_HEAD('')}
      ${shown.length === 0 ? `<div class="trow"><span class="muted">${emptyTorrents(q.total, q.torrents, b.status)}</span></div>` : shown.map((t) => torrentRow(b, t, '')).join('')}
      ${list.length > shown.length ? `<div class="trow"><span class="muted">showing ${shown.length} of ${list.length} — refine the filter</span></div>` : ''}
    </div>
  </div>`;
}
const KIND_COLOR = { seeding: 'var(--ok)', leeching: 'var(--accent)', paused: 'var(--dim)', checking: 'var(--warn)', error: 'var(--bad)', unknown: 'var(--dim)' };
// Column headings shared by both torrent tables; lead is the optional first
// heading (the seedbox, in the fleet-wide list).
const TOR_HEAD = (lead) => `<div class="trow head">${lead}<span>name</span><span>size</span><span>progress</span><span>up</span><span>down</span><span title="lifetime uploaded">uploaded</span><span title="lifetime downloaded">downloaded</span><span>ratio</span><span title="connected seeds (swarm total)">seeds</span><span title="connected peers (swarm total)">peers</span><span></span></div>`;
// Lifetime transfer summed over a list, for the table headers.
function transferNote(list) {
  let up = 0, down = 0;
  for (const t of list) { up += t.uploaded || 0; down += t.downloaded || 0; }
  return list.length ? ` · ↑ ${fmtBytes(up, 1)} ↓ ${fmtBytes(down, 1)} lifetime` : '';
}
// "3 <swarm>/120</swarm>": connected count, then the swarm size when known.
const swarmCell = (n, swarm) => `<span class="num">${n}${swarm ? `<span class="muted swarm">/${swarm}</span>` : ''}</span>`;
const torrentMatches = (t, f) => t.name.toLowerCase().includes(f) || (t.category || '').toLowerCase().includes(f) || t.kind.includes(f);
// One torrent row, shared by the per-box tab and the fleet-wide list; lead
// is an optional first cell (the box, in the fleet-wide list).
function torrentRow(b, t, lead) {
  const color = KIND_COLOR[t.kind] || 'var(--dim)';
  return `
      <div class="trow">${lead}
        <div style="min-width:0;display:flex;align-items:center;gap:8px" title="${esc(t.name)}"><span class="led" style="width:6px;height:6px;background:${color}"></span><span class="ellip">${esc(t.name)}</span></div>
        <span class="muted num">${fmtBytes(t.size, 1)}</span>
        <div><div style="display:flex;justify-content:space-between;font-size:10.5px;color:var(--muted)"><span>${esc(t.kind === 'unknown' ? t.state : t.kind)}</span><span>${(t.progress * 100).toFixed(t.progress < 1 ? 1 : 0)}%</span></div><div class="meter" style="height:3px;margin-top:3px"><i style="width:${t.progress * 100}%;background:${color}"></i></div></div>
        <span class="num ${t.up_bps ? 'up' : 'dim'}">${fmtBps(t.up_bps)}</span>
        <span class="num ${t.down_bps ? 'down' : 'dim'}">${fmtBps(t.down_bps)}</span>
        <span class="num ${t.uploaded ? '' : 'dim'}">${fmtBytes(t.uploaded || 0, 1)}</span>
        <span class="num ${t.downloaded ? '' : 'dim'}">${fmtBytes(t.downloaded || 0, 1)}</span>
        <span class="num">${t.ratio >= 0 ? t.ratio.toFixed(2) : '∞'}</span>
        ${swarmCell(t.seeds || 0, t.seeds_swarm)}
        ${swarmCell(t.peers || 0, t.peers_swarm)}
        <div class="actions">${torrentButtons(b, t)}</div>
      </div>`;
}

/* ----- fleet-wide torrents ----- */
function viewTorrents() {
  const boxes = S.torrents;
  if (boxes === null) return `<div class="head"><div><div class="eyebrow">PMSS</div><h1>Torrents</h1></div></div><div class="empty">loading…</div>`;
  const f = S.filter.trim().toLowerCase();
  const sum = { total: 0, seeding: 0, leeching: 0, paused: 0, errored: 0 };
  const rows = [];
  const notes = [];
  for (const b of boxes) {
    const q = b.qbt;
    for (const k of Object.keys(sum)) sum[k] += q[k] || 0;
    if (!q.reachable) notes.push(`<span class="warn">${esc(b.name)}: ${unreachableNote(b)}</span>`);
    for (const t of q.torrents || []) if (!f || torrentMatches(t, f) || b.name.toLowerCase().includes(f)) rows.push([b, t]);
  }
  // Same order as the per-box tab (active first, busiest first), then by
  // box so one box's idle torrents stay together.
  rows.sort(([ba, a], [bb, c]) => {
    const ra = a.up_bps + a.down_bps, rb = c.up_bps + c.down_bps;
    if ((ra > 0) !== (rb > 0)) return ra > 0 ? -1 : 1;
    if (ra !== rb) return rb - ra;
    return ba.name.localeCompare(bb.name) || ba.id - bb.id;
  });
  const shown = rows.slice(0, 500);
  const qbtBoxes = boxes.length;
  return `<div class="head"><div><div class="eyebrow">PMSS</div><h1>Torrents</h1></div></div>
  <div class="table tor-table tor-all" style="overflow:hidden;margin-top:16px">
    <div class="thead-tools">
      <div style="font-size:12px"><b>${sum.total} torrents</b> <span class="muted">· ${qbtBoxes} seedbox${qbtBoxes === 1 ? '' : 'es'} · ${sum.seeding} seeding · ${sum.leeching} leeching · ${sum.paused} paused${sum.errored ? ` · <span class="bad">${sum.errored} error</span>` : ''}${transferNote(boxes.flatMap((b) => b.qbt.torrents || []))}${notes.length ? ' · ' + notes.join(' · ') : ''}</span></div>
      <div class="actions"><input class="filter" placeholder="filter name, category, state or box…" data-filter value="${esc(S.filter)}" style="max-width:260px"></div>
    </div>
    <div style="overflow:auto">
      ${TOR_HEAD('<span>seedbox</span>')}
      ${qbtBoxes === 0 ? `<div class="trow"><span class="muted">no box has qBittorrent monitoring enabled</span></div>`
        : shown.length === 0 ? `<div class="trow"><span class="muted">${emptyTorrents(sum.total, S.torrents.flatMap((b) => b.qbt?.torrents || []), boxes.some((b) => b.status !== 'disabled' && b.qbt?.total) ? '' : 'disabled')}</span></div>`
        : shown.map(([b, t]) => torrentRow(b, t, boxCell(b))).join('')}
      ${rows.length > shown.length ? `<div class="trow"><span class="muted">showing ${shown.length} of ${rows.length} — refine the filter</span></div>` : ''}
    </div>
  </div>`;
}
// The seedbox cell: a link to the box's own torrents tab, with the enabled
// discovery mechanisms in parentheses so private-tracker placement is
// visible per row.
function boxCell(b) {
  const on = discoveryOn(b.qbt);
  return `<div class="ellip" title="${esc(b.name)}${on.length ? ' · ' + esc(on.join(', ')) + ' enabled' : ''}"><a href="#/box/${b.id}/torrents">${esc(b.name)}</a>${on.length ? ` <span class="warn">(${esc(on.join(' · '))})</span>` : ''}</div>`;
}
/* ---------- peer discovery (DHT / PeX / LSD) ---------- */
const DISCOVERY = [['dht', 'DHT'], ['pex', 'PeX'], ['lsd', 'LSD']];
const discoveryOn = (q) => DISCOVERY.filter(([k]) => q.discovery?.[k]).map(([, l]) => l);
// Card tag: only when something is on, since that is what matters before
// adding a private-tracker torrent. Silent when the settings were not read.
function discoveryTag(q) {
  if (!q.enabled || !q.discovery?.known) return '';
  const on = discoveryOn(q);
  return on.length ? `<span class="tag discovery" title="peer discovery enabled: ${esc(on.join(', '))} — private trackers usually forbid these">${esc(on.join(' · '))}</span>` : '';
}
// Torrents header: every mechanism with its state, so "all off" is visible
// too. Each is a toggle; enabling asks first, since that is the direction
// that breaks private-tracker rules.
// " · firewalled" in the torrents header while incoming connectivity is missing.
function connectionNote(q) {
  if (!q.reachable || !q.connection || q.connection === 'connected') return '';
  return ` · <span class="warn" title="no incoming connections${q.listen_port ? ` on listen port ${q.listen_port}` : ''}${q.firewalled_since ? ` since ${fmtAgo(q.firewalled_since)}` : ''}">${esc(q.connection)}</span>`;
}
function discoveryNote(q, b) {
  if (!q.discovery?.known) return q.reachable ? ' · <span class="muted">discovery settings not read</span>' : '';
  return ' · ' + DISCOVERY.map(([k, l]) => {
    const on = q.discovery[k];
    return `<button class="disc ${on ? 'warn' : ''}" data-act="disc:${k}" data-on="${on ? '1' : ''}" title="${l} ${on ? 'enabled — click to disable' : 'disabled — click to enable'}"${on ? '' : ` data-confirm="Enable ${l} on ${esc(b.name)}? Private trackers usually forbid it; torrents from them may be flagged or banned."`}>${l} ${on ? 'on' : 'off'}</button>`;
  }).join(' · ');
}
// Toast after an add. qBittorrent 5.2+ reports per-entry counts; older
// clients only say that at least one entry was accepted.
function addToast(r, stopped) {
  const tail = stopped ? ' (stopped)' : '';
  if (!r?.counted) {
    const n = (r?.links || 0) + (r?.files || 0);
    return `${n} torrent${n === 1 ? '' : 's'} sent to qBittorrent${tail}`;
  }
  const parts = [`${r.added} added${tail}`];
  if (r.pending) parts.push(`${r.pending} still being fetched`);
  if (r.failed) parts.push(`${r.failed} not added (already present or not a torrent)`);
  return parts.join(', ');
}
// Text for an empty torrent table. After a restart the cached counts are
// restored but the list is not until the first poll, so say so rather than
// "no matches".
function emptyTorrents(total, list, status) {
  if (total === 0) return 'no torrents';
  if (!list || list.length === 0) return status === 'disabled' ? 'polling is disabled for the boxes these torrents are on, so their list is not loaded' : 'torrent list not loaded yet — waiting for the next poll';
  return 'no matches';
}
// Why a box's torrents are missing from the fleet view. A box whose client
// is down is classified "degraded" with the reason naming qBittorrent, so
// the reason (plus the client's own error) is shown; when the box itself is
// down, qBittorrent is not the one to blame and only the box status is.
function unreachableNote(b) {
  if (b.status === 'disabled') return 'polling disabled';
  const q = b.qbt || {}, reason = b.status_reason || '';
  const clientDown = b.status === 'online' || /qbittorrent/i.test(reason);
  const detail = clientDown && q.error ? ` — ${esc(q.error)}` : '';
  if (b.status === 'online') return `client not responding${detail}`;
  return `box ${esc(b.status || 'unknown')}${reason ? ` (${esc(reason)})` : ''}${detail}`;
}
// The add-torrent form. Its draft lives in S.torAdd because every state
// event re-renders the tab; File objects are kept there too, since a file
// input cannot be refilled, and the chosen names are listed beside it.
function torAddForm(b, list) {
  const a = S.torAdd;
  const cats = [...new Set(list.map((t) => t.category).filter(Boolean))].sort((x, y) => x.localeCompare(y));
  return `<form class="tor-add" data-tor-add>
    <label class="f" style="grid-column:1/-1">magnet links or .torrent URLs, one per line
      <textarea rows="3" data-ta="urls" placeholder="magnet:?xt=urn:btih:…" spellcheck="false">${esc(a.urls)}</textarea></label>
    <label class="f">.torrent files
      <input type="file" accept=".torrent,application/x-bittorrent" multiple data-ta-files>
      ${a.files.length ? `<span class="hint">${a.files.length} chosen: ${esc(a.files.map((f) => f.name).join(', '))}</span>` : ''}</label>
    <label class="f">category <span class="muted">(optional)</span>
      <input data-ta="category" list="tor-cats" value="${esc(a.category)}" placeholder="${cats.length ? 'existing or new' : 'new'}">
      <datalist id="tor-cats">${cats.map((c) => `<option value="${esc(c)}">`).join('')}</datalist></label>
    <label class="row" style="align-self:end"><input type="checkbox" data-ta="stopped" ${a.stopped ? 'checked' : ''}> add stopped</label>
    <div class="actions" style="align-self:end">
      <button type="submit" class="small" data-act="tor-add:submit" ${a.busy ? 'disabled' : ''}>${a.busy ? 'adding…' : 'add to ' + esc(b.name)}</button>
      <button type="button" class="small ghost" data-act="tor-add:toggle">cancel</button>
    </div>
  </form>`;
}
// Per-torrent controls. Removal always confirms; removing the data as well
// names the size so the two cannot be mistaken for each other.
function torrentButtons(b, t) {
  if (!t.hash) return '';
  const off = b.status === 'disabled' ? ' disabled title="polling is disabled for this box; enable it to run actions"' : '';
  const h = `data-hash="${esc(t.hash)}" data-box="${b.id}"${off}`;
  const name = esc(t.name);
  return `${t.kind === 'paused' || t.kind === 'error' // error/missingFiles are stopped torrents: start is what recovers them
    ? `<button class="small" data-act="tor:start" ${h}>start</button>`
    : `<button class="small" data-act="tor:stop" ${h} data-confirm="Stop ${name} on ${esc(b.name)}?">stop</button>`}
    <button class="small ghost" data-act="tor:remove" ${h} data-confirm="Remove ${name} from qBittorrent on ${esc(b.name)}? The downloaded files stay on disk.">remove</button>
    <button class="small ghost" data-act="tor:remove-data" ${h} data-confirm="Remove ${name} AND DELETE its ${fmtBytes(t.size, 1)} of files on ${esc(b.name)}? This cannot be undone.">+ files</button>`;
}
function tabContainers(b) {
  const d = b.docker;
  if (!d.enabled) return `<div class="empty">docker monitoring is off for this box — enable it under <a href="#/edit/${b.id}">edit</a>.</div>`;
  const list = d.containers || [];
  return `<div class="table ctr-table" style="overflow:hidden">
    <div class="thead-tools">
      <div style="font-size:12px"><b>rootless docker ${esc(d.version || '')}</b> <span class="muted">· ${d.running} running · ${d.stopped} stopped${d.reachable ? '' : ` · <span class="warn">not responding${d.error ? ': ' + esc(d.error) : ''}</span>`}</span></div>
    </div>
    <div style="overflow:auto">
      <div class="trow head"><span>name</span><span>image</span><span>state</span><span>ports</span><span>cpu</span><span>memory</span><span></span></div>
      ${list.length === 0 ? `<div class="trow"><span class="muted">no containers</span></div>` : list.map((c) => {
        const run = c.state === 'running';
        return `<div class="trow">
        <div class="ellip" title="${esc(c.id)}"><span class="led ${run ? 'online' : (c.state === 'exited' ? 'offline' : 'pending')}" style="width:6px;height:6px;margin-right:8px"></span>${esc(c.name)}</div>
        <span class="ellip muted" title="${esc(c.image)}">${esc(c.image)}</span>
        <span class="${run ? '' : 'muted'}" title="${esc(c.status)}">${esc(c.status)}</span>
        <span class="ellip muted" title="${esc((c.ports || []).join(', '))}">${esc((c.ports || []).join(', ') || '—')}</span>
        <span class="num">${c.has_stats ? c.cpu_pct.toFixed(1) + '%' : '—'}</span>
        <span class="num">${c.has_stats ? fmtBytes(c.mem_usage, 0) : '—'}</span>
        <span class="actions" style="justify-content:flex-end">
          ${run ? `<button class="small" data-act="ctr:restart" data-cid="${esc(c.id)}" data-confirm="Restart ${esc(c.name)}?">restart</button><button class="small" data-act="ctr:stop" data-cid="${esc(c.id)}" data-confirm="Stop ${esc(c.name)}?">stop</button>` : `<button class="small" data-act="ctr:start" data-cid="${esc(c.id)}">start</button>`}
        </span>
      </div>`;
      }).join('')}
    </div>
  </div>`;
}
/* ----- services: the account's own processes grouped into applications ----- */
function appUrl(b, a) { return a.web_path ? `https://${b.hostname || b.host}${a.web_path}` : ''; }
function appName(A, key) { const a = (A.apps || []).find((x) => x.key === key); return a ? a.name : key.replace(/^other:/, ''); }
function tabServices(b) {
  const A = b.apps || {};
  const f = S.filter.trim().toLowerCase();
  const apps = (A.apps || []).filter((a) => !f || a.name.toLowerCase().includes(f) || a.category.includes(f) || a.key.includes(f));
  const all = A.processes || [];
  const procs = all.filter((p) => !f || p.name.toLowerCase().includes(f) || (p.args || '').toLowerCase().includes(f) || appName(A, p.app).toLowerCase().includes(f)).slice(0, 200);
  const note = A.ok ? `${A.procs} process${A.procs === 1 ? '' : 'es'} · ${fmtBytes(A.rss, 1)} resident` : `<span class="warn">process list not read${b.status === 'offline' ? ' (box unreachable)' : b.status === 'pending' ? ' yet' : ' (ps printed nothing on the box)'}</span>`;
  return `<div class="table app-table" style="overflow:hidden">
    <div class="thead-tools">
      <div style="font-size:12px"><b>services</b> <span class="muted">· ${note}</span></div>
      <div class="actions"><input class="filter" placeholder="filter…" data-filter value="${esc(S.filter)}"></div>
    </div>
    <div style="overflow:auto">
      <div class="trow head"><span>application</span><span>kind</span><span>state</span><span>cpu</span><span>memory</span><span>up for</span><span title="listening TCP ports held by its processes">ports</span><span>link</span><span title="raise an alert when this application is not running">watch</span></div>
      ${apps.length === 0 ? `<div class="trow"><span class="muted">${A.ok ? 'nothing matches' : '—'}</span></div>` : apps.map((a) => appRow(b, a)).join('')}
    </div>
  </div>
  <div class="hint" style="margin-top:8px">Everything the account runs on the shared host, as its own uid sees it (other tenants' processes are hidden by PMSS). CPU is the share of one core since the previous poll. Watch an application to be alerted when it is not running; a watched application that is down stays listed.</div>
  <div class="table proc-table" style="overflow:hidden;margin-top:14px">
    <div class="thead-tools"><div style="font-size:12px"><b>processes</b> <span class="muted">· largest first${procs.length < all.length ? ` · showing ${procs.length} of ${all.length}` : ''}</span></div></div>
    <div style="overflow:auto">
      <div class="trow head"><span>pid</span><span>name</span><span>application</span><span>cpu</span><span>rss</span><span>up for</span><span>command</span></div>
      ${procs.length === 0 ? `<div class="trow"><span class="muted">${A.ok ? 'nothing matches' : '—'}</span></div>` : procs.map((p) => `<div class="trow"><span class="num muted">${p.pid}</span><span class="ellip" title="${esc(p.name)}">${esc(p.name)}</span><span class="ellip muted">${esc(appName(A, p.app))}</span><span class="num">${p.cpu_known ? p.cpu_pct.toFixed(1) + '%' : '—'}</span><span class="num">${fmtBytes(p.rss, 1)}</span><span class="muted">${fmtDur(p.elapsed_sec)}</span><span class="ellip muted" title="${esc(p.args || '')}">${esc(p.args || '')}</span></div>`).join('')}
    </div>
  </div>`;
}
function appRow(b, a) {
  const url = appUrl(b, a);
  return `<div class="trow">
    <div class="ellip" title="${esc(a.key)}"><span class="led ${a.running ? 'online' : 'offline'}" style="width:6px;height:6px;margin-right:8px"></span>${esc(a.name)}</div>
    <span><span class="tag">${esc(a.category)}</span></span>
    <span class="${a.running ? '' : 'bad'}">${a.running ? `running · ${a.procs} proc${a.procs === 1 ? '' : 's'}` : 'not running'}</span>
    <span class="num">${a.cpu_known ? a.cpu_pct.toFixed(1) + '%' : '—'}</span>
    <span class="num">${a.running ? fmtBytes(a.rss, 1) : '—'}</span>
    <span class="muted">${a.running ? fmtDur(a.uptime_sec) : '—'}</span>
    <span class="num muted ellip">${(a.ports || []).join(', ') || '—'}</span>
    <span>${url ? `<a href="${esc(url)}" target="_blank" rel="noopener" title="${esc(url)}">open ↗</a>` : '—'}</span>
    <span><button class="small ${a.watched ? 'good' : ''}" data-act="app:watch" data-key="${esc(a.key)}" data-on="${a.watched ? '1' : ''}" title="${a.watched ? 'watched: an alert is raised when it is not running; click to stop watching' : 'alert when this application is not running'}">${a.watched ? 'watching' : 'watch'}</button></span>
  </div>`;
}
function tabStorage(b) {
  const dirs = b.dirs || {};
  const entries = (dirs.entries || []).slice();
  const quota = b.disk.quota_bytes || b.disk.used_bytes || 1;
  const top = entries.slice(0, 7);
  const rest = entries.slice(7).reduce((a, e) => a + e.bytes, 0);
  if (rest > 0) top.push({ name: entries.some((e) => e.name === '(smaller entries)') ? `other (${entries.length - 8}+ entries)` : `other (${entries.length - 7} entries)`, bytes: rest });
  const cols = ['var(--c1)', 'var(--c2)', 'var(--c3)', 'var(--c4)', 'var(--c5)', 'var(--c6)', 'var(--c7)', 'var(--dim)'];
  const measured = entries.reduce((a, e) => a + e.bytes, 0);
  const free = Math.max(0, b.disk.quota_bytes - b.disk.used_bytes);
  return `<div class="two">
    <div class="panel">
      <div style="display:flex;justify-content:space-between;align-items:baseline;gap:8px;flex-wrap:wrap">
        <div><div class="tile-label">usage by directory</div><div style="font-size:18px;font-weight:600;margin:2px 0 14px">${fmtBytes(b.disk.used_bytes, 2)} <small class="muted" style="font-size:12px;font-weight:400">of ${b.disk.quota_bytes ? fmtBytes(b.disk.quota_bytes, 1) : 'n/a'}</small></div></div>
        <div class="actions"><span class="hint">${dirs.running ? '<span class="pulse">measuring…</span>' : dirs.at && new Date(dirs.at).getTime() > 0 ? `measured ${fmtAgo(dirs.at)}` : 'not measured yet'}</span><button class="small" data-act="du" ${dirs.running || !dirs.enabled ? 'disabled' : ''}${!dirs.enabled ? ' title="directory measurement is off for this box; enable it in the box settings"' : ''}>measure now</button></div>
      </div>
      ${!dirs.enabled ? `<div class="hint">directory measurement is off for this box.</div>` : ''}
      ${dirs.error ? `<div class="err-box">${esc(dirs.error)}</div>` : ''}
      ${top.length ? `
      <div class="segbar">${top.map((e, i) => `<i title="${esc(e.name)} ${fmtBytes(e.bytes, 1)}" style="background:${cols[i]};width:${(e.bytes / quota * 100).toFixed(2)}%"></i>`).join('')}<i class="free"></i></div>
      <div style="display:grid;gap:8px">
        ${top.map((e, i) => `<div class="dirrow"><span class="sw" style="background:${cols[i]}"></span><span class="ellip" title="${esc(e.name)}">${esc(e.name)}</span><span class="pct num">${(e.bytes / quota * 100).toFixed(1)}%</span><span class="sz num">${fmtBytes(e.bytes, 1)}</span></div>`).join('')}
        <div class="dirrow muted"><span class="sw" style="background:var(--line)"></span><span>free</span><span class="pct num">${(free / quota * 100).toFixed(1)}%</span><span class="sz num">${fmtBytes(free, 1)}</span></div>
      </div>
      ${Math.abs(measured - b.disk.used_bytes) > b.disk.used_bytes * 0.1 && measured > 0 ? `<div class="hint" style="margin-top:12px">du total ${fmtBytes(measured, 1)} differs from quota usage ${fmtBytes(b.disk.used_bytes, 1)} (quota counts blocks incl. hidden files and snapshots).</div>` : ''}` : ''}
    </div>
    <div class="stack">
      <div class="panel rows"><div class="tile-label">quota</div>
        <div><span>used</span><span>${fmtBytes(b.disk.used_bytes, 2)}</span></div>
        <div><span>soft limit</span><span>${b.disk.quota_bytes ? fmtBytes(b.disk.quota_bytes, 2) : '—'}</span></div>
        <div><span>hard limit</span><span>${b.disk.hard_bytes ? fmtBytes(b.disk.hard_bytes, 2) : '—'}</span></div>
        <div><span>files</span><span>${b.disk.files_used ? `${b.disk.files_used.toLocaleString()} / ${b.disk.files_soft.toLocaleString()}` : '—'}</span></div>
        <div><span>home fs</span><span>${esc(b.host_sys.home_fs_type || '—')} · ${fmtBytes(b.host_sys.home_size, 0)} shared</span></div>
        <div><span>host free</span><span>${fmtBytes(b.host_sys.home_avail, 1)}</span></div>
        <div><span>source</span><span class="muted">${b.disk.source === 'quota' ? 'live quota command' : b.disk.source === 'file' ? '~/.quota snapshot' : '—'}</span></div>
      </div>
      <div class="panel rows"><div class="tile-label">access</div>
        <div><span>sftp</span><span>${esc(b.user)}@${esc(b.hostname || b.host)}:${b.port}</span></div>
        <div><span>ftps</span><span>${esc(b.hostname || b.host)}:21</span></div>
        ${userLinkRow(b, 'panel', '')}
        ${userLinkRow(b, 'data', 'data/')}
        ${b.type === 'seedbox' ? userLinkRow(b, 'qbittorrent', 'qbittorrent') : ''}
        ${b.qbt.enabled && b.qbt.port ? `<div><span>qbt port</span><span class="muted">127.0.0.1:${b.qbt.port} on the box (tunnelled)</span></div>` : ''}
      </div>
    </div>
  </div>`;
}
// One row of the access panel: a per-user link on the box's own web server.
// Every tenant path hangs off https://<host>/user-<name>/, so only the tail
// differs ('' for the panel, 'data/', 'qbittorrent').
function userUrl(b, tail) { return `https://${b.hostname || b.host}/user-${b.user}/${tail}`; }
function userLinkRow(b, label, tail) {
  const u = esc(userUrl(b, tail));
  return `<div><span>${label}</span><a href="${u}" target="_blank" rel="noopener">${u.replace(/^https:\/\//, '')}</a></div>`;
}
function tabHost(b) {
  const h = b.host_sys;
  const memPct = h.mem_total ? (1 - h.mem_avail / h.mem_total) * 100 : 0;
  const homePct = h.home_size ? h.home_used / h.home_size * 100 : 0;
  return `<div class="grid">
    <div class="panel rows"><div class="tile-label">shared host</div>
      <div><span>hostname</span><span>${esc(b.hostname || '—')}</span></div>
      <div><span>uptime</span><span>${h.uptime_sec ? fmtDur(h.uptime_sec) : '—'}</span></div>
      <div><span>cpu</span><span title="${esc(h.cpu_model || '')}">${esc(h.cpu_model || '—')}</span></div>
      <div><span>hw encode</span><span class="${h.gpu_present ? (h.gpu_access ? 'ok' : 'warn') : 'muted'}">${h.gpu_present ? `${esc(h.hw_encode)} · ${esc(h.gpu_vendor)} ${esc(h.gpu_device)} ${h.gpu_access ? '(accessible)' : '(no permission for this account)'}` : 'no /dev/dri render node'}</span></div>
      ${h.cpu_flags ? `<div><span>flags</span><span class="muted">${esc(h.cpu_flags)}</span></div>` : ''}
      <div><span>load 1 / 5 / 15</span><span class="num">${h.load1?.toFixed(2)} / ${h.load5?.toFixed(2)} / ${h.load15?.toFixed(2)}${h.ncpu ? ` <span class="muted">· ${h.ncpu} cores</span>` : ''}</span></div>
      <div><span>kernel</span><span>${esc(h.kernel || '—')}</span></div>
      <div><span>debian</span><span>${esc(h.debian || '—')}</span></div>
      <div><span>PMSS</span><span class="ellip" title="${esc(h.pmss_version)}">${esc(h.pmss_version || '—')}</span></div>
    </div>
    <div class="panel"><div class="tile-label">host memory</div>
      <div class="tile-value">${fmtPct(memPct)} <small>used · ${fmtBytes(h.mem_total - h.mem_avail, 1)} / ${fmtBytes(h.mem_total, 0)}</small></div>
      <div class="meter thick"><i class="${levelClass(memPct, 95)}" style="width:${memPct}%"></i></div>
      <div class="note">all tenants combined · your slice: ${b.ram.rss_bytes ? `${fmtBytes(b.ram.rss_bytes, 1)} process + ${fmtBytes(b.ram.cache_bytes, 1)} page cache` : fmtBytes(b.ram.used_bytes, 1)}${b.ram.limit_bytes ? ` of ${fmtBytes(b.ram.limit_bytes, 0)}` : ''}${b.ram.oom_kills ? ` · ${b.ram.oom_kills} OOM kills` : ''}</div></div>
    <div class="panel"><div class="tile-label">host /home</div>
      <div class="tile-value">${fmtPct(homePct)} <small>used · ${fmtBytes(h.home_used, 1)} / ${fmtBytes(h.home_size, 1)}</small></div>
      <div class="meter thick"><i class="${levelClass(homePct, 95)}" style="width:${homePct}%"></i></div>
      <div class="note">${esc(h.home_fs_type || '')} · ${fmtBytes(h.home_avail, 1)} free for everyone</div></div>
    <div class="panel"><div class="tile-label">host load</div>
      <div class="tile-value">${h.load1?.toFixed(2) ?? '—'} <small>1 min${h.ncpu ? ` · ${(h.load1 / h.ncpu * 100).toFixed(0)}% of ${h.ncpu} cores` : ''}</small></div>
      <div class="meter thick"><i class="${levelClass(h.ncpu ? h.load1 / h.ncpu * 100 : 0, 100)}" style="width:${h.ncpu ? Math.min(100, h.load1 / h.ncpu * 100) : 0}%"></i></div>
      <div class="note">a busy neighbour shows up here, not in your own cpu meter</div></div>
  </div>`;
}

/* ----- add / edit ----- */
function blankForm() {
  return { name: '', host: '', port: 22, user: '', auth_method: '', key_path: '', password: '', type: 'seedbox', plan: '', datacenter: '', renewal: '',
    qbt_enabled: true, qbt_port: 0, qbt_user: '', qbt_pass: '', qbt_start_cmd: '', docker_enabled: false, du_enabled: true, enabled: true, host_key: '' };
}
function resetAdd() { S.add = { mode: 'add', step: 1, form: blankForm(), errors: {}, detect: null, busy: false, editId: null, service: null }; }
async function startEdit(id) {
  S.add = { mode: 'edit', step: 1, form: null, errors: {}, detect: null, busy: false, editId: id, service: boxById(id)?.service || null };
  try {
    const d = await api('GET', `${BASE}/api/boxes/${id}`);
    if (S.add.mode !== 'edit' || S.add.editId !== id) return; // navigated on before this answer came back
    const b = d.box;
    S.add.form = { ...blankForm(), ...b, password: '', qbt_pass: '', port: b.port || 22, host_key: '', forget_host_key: false, clear_qbt_pass: false };
    S.add.pinned = !!b.host_key; // the key itself never round-trips: changing host/port must clear it
    render();
  } catch (e) { toast(e.message, 'bad'); if (S.add.mode === 'edit' && S.add.editId === id) go('/fleet'); }
}
function viewAdd() {
  const A = S.add;
  if (!A.form) { if (A.mode === 'add') resetAdd(); else return `<div class="empty pulse">loading…</div>`; }
  const f = A.form, E = A.errors;
  const edit = A.mode === 'edit';
  const stepCls = (n) => A.step === n ? 'on' : A.step > n ? 'done' : '';
  const back = edit ? `/box/${A.editId}` : '/fleet';
  let body = '';
  if (A.step === 1) {
    body = addStep1(A, f, E, edit, back);
  } else if (A.step === 2) {
    body = addStep2(A, f, edit);
  } else {
    body = addStep3(A, f, edit);
  }
  return `<div style="max-width:620px">
    <button class="crumb" data-go="${back}">← ${edit ? 'box' : 'fleet'}</button>
    <h1 style="margin:0 0 6px">${edit ? `Edit ${esc(f.name || 'box')}` : 'Add box'}</h1>
    <div class="steps"><span class="${stepCls(1)}">1 connect</span><span>—</span><span class="${stepCls(2)}">2 detect</span><span>—</span><span class="${stepCls(3)}">3 confirm</span></div>
    ${body}
  </div>`;
}
/* add/edit wizard, step 1: the connection form */
function addIdentityFields(f, E) {
  return `<div class="f2">
        <label class="f">display name<input data-f="name" value="${esc(f.name)}" placeholder="e.g. seed-02"></label>
        <label class="f">box type<select data-f="type"><option value="seedbox" ${f.type === 'seedbox' ? 'selected' : ''}>seedbox</option><option value="storage" ${f.type === 'storage' ? 'selected' : ''}>storage box</option></select></label>
      </div>
      <div class="f2" style="grid-template-columns:1fr 110px">
        <label class="f">hostname *<input data-f="host" class="${E.host ? 'err' : ''}" value="${esc(f.host)}" placeholder="server.pulsedmedia.com">${E.host ? `<span class="err-box">${esc(E.host)}</span>` : ''}</label>
        <label class="f">ssh port<input data-f="port" type="number" min="1" max="65535" value="${f.port || 22}"></label>
      </div>`;
}
function addUserAuthFields(f, E) {
  return `<div class="f2">
        <label class="f">username *<input data-f="user" class="${E.user ? 'err' : ''}" value="${esc(f.user)}" autocomplete="off">${E.user ? `<span class="err-box">${esc(E.user)}</span>` : ''}</label>
        <label class="f">ssh auth<select data-f="auth_method">
          <option value="" ${f.auth_method === '' ? 'selected' : ''}>auto (keys, agent, password)</option>
          <option value="key" ${f.auth_method === 'key' ? 'selected' : ''}>private key file</option>
          <option value="agent" ${f.auth_method === 'agent' ? 'selected' : ''}>ssh agent</option>
          <option value="password" ${f.auth_method === 'password' ? 'selected' : ''}>password</option></select></label>
      </div>`;
}
function addKeyPasswordFields(f, E, edit) {
  return `<div class="f2">
        <label class="f">key path <span class="hint">(empty = ~/.ssh/id_*)</span><input data-f="key_path" value="${esc(f.key_path)}" placeholder="~/.ssh/id_ed25519" ${f.auth_method === 'password' || f.auth_method === 'agent' ? 'disabled' : ''}></label>
        <label class="f">ssh password${edit ? ` <span class="hint">${f.has_password ? '(blank keeps saved)' : '(none saved)'}</span>` : ''}<input data-f="password" type="password" class="${E.password ? 'err' : ''}" value="${esc(f.password)}" autocomplete="new-password" ${f.auth_method === 'key' || f.auth_method === 'agent' ? 'disabled' : ''}>${E.password ? `<span class="err-box">${esc(E.password)}</span>` : ''}</label>
      </div>`;
}
function addQbtFields(f, edit) {
  // The detected port is only a hint: a saved 0 keeps following qBittorrent.conf.
  const dp = Number(f.qbt_port) ? 0 : Number(S.add.detect?.box?.qbt_port) || 0;
  return `<label class="check"><input type="checkbox" data-f="qbt_enabled" ${f.qbt_enabled ? 'checked' : ''}> monitor qbittorrent-nox via its Web API (tunnelled over SSH)</label>
        <div class="f3">
          <label class="f">WebUI port <span class="hint">(0 = detect from config${dp ? `; detected ${dp}` : ''})</span><input data-f="qbt_port" type="number" min="0" max="65535" value="${f.qbt_port || 0}"></label>
          <label class="f">WebUI user<input data-f="qbt_user" value="${esc(f.qbt_user)}" autocomplete="off"></label>
          <label class="f">WebUI password${edit ? ` <span class="hint">${f.has_qbt_pass ? '(blank keeps saved)' : '(none saved)'}</span>` : ''}<input data-f="qbt_pass" type="password" value="${esc(f.qbt_pass)}" autocomplete="new-password"></label>
        </div>
        <label class="f">start command <span class="hint">(used by “start client”; it must detach, e.g. -d or the qbt-start helper: a command left in the foreground is killed after 30 s)</span><input data-f="qbt_start_cmd" value="${esc(f.qbt_start_cmd)}" placeholder='cd "$HOME" && ( "$HOME/bin/qbittorrent-nox" -d </dev/null >/dev/null 2>&1 )'></label>`;
}
function addDockerDuFields(f) {
  return `<label class="check"><input type="checkbox" data-f="docker_enabled" ${f.docker_enabled ? 'checked' : ''}> monitor rootless docker containers</label>
        <label class="check"><input type="checkbox" data-f="du_enabled" ${f.du_enabled ? 'checked' : ''}> measure directory usage periodically (du at lowest I/O priority)</label>`;
}
// Checkboxes that only make sense for an existing box (each renders empty in add mode).
function addEditExtras(A, f, edit) {
  return `${edit ? `<label class="check"><input type="checkbox" data-f="enabled" ${f.enabled ? 'checked' : ''}> polling enabled</label>` : ''}
        ${edit && A.pinned ? `<label class="check"><input type="checkbox" data-f="forget_host_key" ${f.forget_host_key ? 'checked' : ''}> forget the pinned SSH host key <span class="hint">(after a reinstall or migration; the next connect trusts and pins whatever key answers)</span></label>` : ''}
        ${edit && f.has_qbt_pass ? `<label class="check"><input type="checkbox" data-f="clear_qbt_pass" ${f.clear_qbt_pass ? 'checked' : ''}> remove the saved WebUI password <span class="hint">(the SSH password is removed automatically when auth is switched to key or agent)</span></label>` : ''}`;
}
function addStep1(A, f, E, edit, back) {
  return `<div class="form">
      ${addIdentityFields(f, E)}
      ${addUserAuthFields(f, E)}
      ${addKeyPasswordFields(f, E, edit)}
      <div class="f3">
        <label class="f">plan <span class="hint">(optional)</span><input data-f="plan" value="${esc(f.plan)}" placeholder="${esc(A.service?.product || 'Dragon-R 2TB')}"></label>
        <label class="f">datacenter<input data-f="datacenter" value="${esc(f.datacenter)}" placeholder="Helsinki, FI"></label>
        <label class="f">renewal date<input data-f="renewal" type="date" value="${esc(f.renewal)}"></label>
      </div>
      ${addServiceHint(A.service)}
      <details class="adv" ${edit ? 'open' : ''}><summary>qBittorrent, docker and measurement options</summary><div>
        ${addQbtFields(f, edit)}
        ${addDockerDuFields(f)}
        ${addEditExtras(A, f, edit)}
      </div></details>
      <div class="form-foot">
        <button data-go="${back}">cancel</button>
        ${edit ? `<button data-act="add:next" ${A.busy ? 'disabled' : ''}>test connection →</button><button class="primary" data-act="edit:save" ${A.busy ? 'disabled' : ''}>save</button>`
               : `<button class="primary" data-act="add:next" ${A.busy ? 'disabled' : ''}>test connection →</button>`}
      </div>
    </div>`;
}
/* step 2: the detect log */
function addDetectLog(steps, mark) {
  return steps.map((s) => `<div><span class="mk ${s.status === 'run' ? 'pulse' : ''}" style="color:${mark[s.status]?.[1]}">${mark[s.status]?.[0] || '·'}</span><span style="color:${s.status === 'fail' ? 'var(--bad)' : s.status === 'info' ? 'var(--muted)' : 'var(--text)'}">${esc(s.text)}</span></div>`).join('');
}
// addServiceHint explains why plan and renewal are left blank when the box is
// matched to a Pulsed Media service: the API values stay live, a typed value
// would freeze at today's date and go stale after the next billing cycle.
function addServiceHint(sv) {
  if (!sv) return '';
  const r = serviceRenewal(sv);
  return `<div class="hint">Matched to the Pulsed Media service <b>${esc(sv.product)}</b>${r ? `, renews ${esc(r)}` : ''}. Plan and renewal date are taken from the API and stay in sync as it changes; fill them in above only to override them with fixed values.</div>`;
}
function addStep2(A, f, edit) {
  const D = A.detect;
  const mark = { ok: ['✓', 'var(--ok)'], warn: ['!', 'var(--warn)'], fail: ['✗', 'var(--bad)'], info: ['·', 'var(--muted)'], run: ['…', 'var(--accent)'] };
  const steps = D ? D.steps : [{ status: 'run', text: `connecting to ${f.host}:${f.port || 22}…` }];
  return `<div class="form" style="gap:10px">
      <div class="log">${addDetectLog(steps, mark)}</div>
      ${D ? `<div class="form-foot">
        <button data-act="add:back">← back</button>
        ${D.ok ? `<button class="primary" data-act="add:next">${edit ? 'apply detected →' : 'continue →'}</button>` : `<button data-act="add:retry">retry</button>`}
      </div>` : ''}
    </div>`;
}
/* step 3: the detected facts and the confirm button */
function addDetectedFacts(f, F, pb) {
  return `<div><span>name</span><span>${esc(f.name || pb.name)}</span></div>
        <div><span>hostname</span><span>${esc(F.hostname || f.host)}</span></div>
        <div><span>account</span><span>${esc(F.user)} (uid ${F.uid})</span></div>
        <div><span>PMSS</span><span>${esc(F.pmss_version || '—')} · Debian ${esc(F.debian || '—')}</span></div>
        <div><span>disk quota</span><span>${F.quota_bytes ? `${fmtBytes(F.quota_bytes, 1)} · ${fmtBytes(F.disk_used_bytes, 1)} used` : '—'}</span></div>
        <div><span>traffic limit</span><span>${F.traffic_limit_bytes ? fmtBytes(F.traffic_limit_bytes, 1) + ' / 30 days' : 'unlimited or unknown'}</span></div>
        <div><span>resources</span><span>${F.ram_limit_bytes ? fmtBytes(F.ram_limit_bytes, 0) + ' RAM' : '—'}${F.cpu_quota_cpus ? ` · ${F.cpu_quota_cpus.toFixed(1)} CPUs` : ''}</span></div>`;
}
function addDetectedServiceFacts(f, F, pb, sv) {
  return `<div><span>client</span><span>${pb.qbt_enabled ? `qBittorrent ${esc(F.qbt_version || '(not responding)')} on port ${F.qbt_port}` : 'none'}</span></div>
        <div><span>docker</span><span>${pb.docker_enabled ? `${esc(F.docker_version || 'enabled, daemon down')}${F.containers ? ` · ${F.containers} containers` : ''}` : 'no'}</span></div>
        <div><span>host key</span><span class="muted">${esc(F.host_key_fp || '')}</span></div>
        <div><span>plan / renewal</span><span>${addPlanRenewalCell(f, sv)}</span></div>`;
}
// addPlanRenewalCell shows what the box will display after saving: a typed value, else the matched service's, marked as API-sourced.
function addPlanRenewalCell(f, sv) {
  const api = (v) => `${esc(v)} <span class="muted" title="from the Pulsed Media API, kept in sync">·api</span>`;
  const r = serviceRenewal(sv);
  const plan = f.plan ? esc(f.plan) : sv?.product ? api(sv.product) : '—';
  const ren = f.renewal ? esc(f.renewal) : r ? api(r) : '—';
  return `${plan} · ${ren}`;
}
function addStep3(A, f, edit) {
  const D = A.detect, F = D.facts || {}, pb = D.box || {};
  return `<div class="form" style="gap:10px">
      <div class="tile-label">detected</div>
      <div class="rows">
        ${addDetectedFacts(f, F, pb)}
        ${addDetectedServiceFacts(f, F, pb, A.service)}
      </div>
      <div class="form-foot">
        <button data-act="add:back">← back</button>
        <button class="good" data-act="add:confirm" ${A.busy ? 'disabled' : ''}>${edit ? 'save box' : 'add to fleet'}</button>
      </div>
    </div>`;
}
function validateForm(f) {
  const E = {};
  if (!/^[a-z0-9.-]+$/i.test(f.host.trim())) E.host = 'enter a valid hostname';
  if (!f.user.trim()) E.user = 'required';
  if (f.auth_method === 'password' && !f.password && S.add.mode === 'add') E.password = 'required for password auth';
  if (S.add.mode === 'edit' && f.auth_method === 'password' && !f.password && !f.has_password) E.password = 'no password is saved for this box; enter one';
  return E;
}
function payload(f) {
  const p = { ...f };
  p.port = Number(p.port) || 22; p.qbt_port = Number(p.qbt_port) || 0;
  p.host = p.host.trim(); p.user = p.user.trim();
  if (p.auth_method === 'key' || p.auth_method === 'agent') p.password = ''; // typed before switching auth: not used, not stored
  return p;
}
async function runDetect() {
  const A = S.add;
  A.detect = null; A.busy = true; render();
  try {
    const res = await api('POST', BASE + '/api/detect', payload(A.form));
    A.detect = res;
    if (res.ok && res.box) {
      // Adopt detected facts into the form where the user left defaults.
      const b = res.box;
      A.form.host_key = b.host_key || '';
      if (!A.form.name) A.form.name = b.name;
      // qbt_port stays as typed: 0 follows qBittorrent.conf on every poll, so
      // the detected port is shown as a hint instead of being pinned.
      A.form.qbt_enabled = b.qbt_enabled;
      A.form.docker_enabled = b.docker_enabled;
    }
  } catch (e) {
    A.detect = { ok: false, steps: [{ status: 'fail', text: e.message }] };
  }
  A.busy = false; render();
}
async function confirmAdd() {
  const A = S.add;
  A.busy = true; render();
  try {
    if (A.mode === 'edit') {
      await api('PUT', `${BASE}/api/boxes/${A.editId}`, payload(A.form));
      toast('box updated', 'ok');
      go(`/box/${A.editId}`);
    } else {
      const b = await api('POST', BASE + '/api/boxes', payload(A.form));
      toast(`${b.name} added to fleet`, 'ok');
      await loadState();
      resetAdd();
      go(`/box/${b.id}`);
    }
  } catch (e) { toast(e.message, 'bad'); A.busy = false; render(); }
}

/* ----- alerts ----- */
function viewAlerts() {
  const open = S.alerts.filter((a) => a.active && !a.acked);
  const acked = S.alerts.filter((a) => a.active && a.acked);
  const done = S.alerts.filter((a) => !a.active).slice(0, 50);
  const color = { bad: 'var(--bad)', warn: 'var(--warn)' };
  const row = (a) => `<div class="alert ${a.active && !a.acked ? '' : 'done'}">
      <span class="led" style="background:${color[a.severity] || 'var(--dim)'};box-shadow:0 0 8px ${color[a.severity] || 'transparent'}"></span>
      <div style="min-width:0"><div class="t">${esc(a.title)}</div>
        <div class="m"><button data-go="/box/${a.box_id}">${esc(boxName(a.box_id))}</button> · ${a.active ? `since ${fmtAgo(a.created_at)}` : `resolved ${fmtAgo(a.resolved_at || a.updated_at)}`}${a.acked && a.active ? ' · acknowledged, condition still present' : ''}</div></div>
      ${a.active && !a.acked ? `<button class="small" data-act="ack" data-id="${a.id}">ack</button>` : `<button class="small ghost" data-act="alert:dismiss" data-id="${a.id}" title="remove from the list">dismiss</button>`}
    </div>`;
  return `<div style="max-width:820px">
    <div class="head" style="margin-bottom:20px"><h1 style="margin:0">Alerts</h1>
      <div class="actions">
        <button data-act="ack-all" ${open.length ? '' : 'disabled'}>acknowledge all</button>
        <button data-act="alerts:clear-resolved" ${done.length ? '' : 'disabled'}>clear resolved</button>
      </div></div>
    ${open.length === 0 ? `<div class="empty">no unacknowledged alerts</div>` : `<div class="alerts">${open.map(row).join('')}</div>`}
    ${acked.length ? `<div class="tile-label" style="margin:24px 0 8px">acknowledged · still active (${acked.length})</div><div class="alerts">${acked.map(row).join('')}</div><div class="hint" style="margin-top:8px">These resolve on their own when the condition clears; dismiss hides one until it is raised again.</div>` : ''}
    ${done.length ? `<div class="tile-label" style="margin:24px 0 8px">recently resolved (${done.length})</div><div class="alerts">${done.map(row).join('')}</div>` : ''}
  </div>`;
}
function boxName(id) { const b = S.boxes.find((x) => x.id === id); return b ? b.name : 'removed box'; }

/* ----- settings ----- */
function viewSettings() {
  const s = S.settingsDraft || S.settings;
  const sel = (name, opts, cur) => `<select data-s="${name}">${opts.map(([v, l]) => `<option value="${v}" ${String(cur) === String(v) ? 'selected' : ''}>${l}</option>`).join('')}</select>`;
  return `<div class="settings">
    <h1 style="margin:0 0 6px">Settings</h1>
    <div class="panel"><div class="tile-label">appearance</div>
      <div class="row"><span>theme</span><div class="seg"><button class="${s.theme !== 'light' ? 'on' : ''}" data-s-seg="theme" data-v="dark">dark</button><button class="${s.theme === 'light' ? 'on' : ''}" data-s-seg="theme" data-v="light">light</button></div></div>
      <div class="row"><span>card size</span><div class="seg">${CARD_SIZES.map(([px, l]) => `<button class="${Number(s.card_min_px) === px ? 'on' : ''}" data-s-seg="card_min_px" data-v="${px}">${l}</button>`).join('')}</div></div>
      <div class="row"><span>default fleet view</span><div class="seg"><button class="${s.fleet_view !== 'table' ? 'on' : ''}" data-s-seg="fleet_view" data-v="cards">cards</button><button class="${s.fleet_view === 'table' ? 'on' : ''}" data-s-seg="fleet_view" data-v="table">table</button></div></div>
    </div>
    <div class="panel"><div class="tile-label">polling &amp; alerts</div>
      <label class="row">refresh interval${sel('poll_interval_sec', [[10, '10 s'], [30, '30 s'], [60, '60 s'], [120, '2 min'], [300, '5 min']], s.poll_interval_sec)}</label>
      <label class="row">disk alert threshold${sel('disk_threshold_pct', [[80, '80%'], [90, '90%'], [95, '95%']], s.disk_threshold_pct)}</label>
      <label class="row">traffic alert threshold${sel('traffic_threshold_pct', [[75, '75%'], [90, '90%'], [100, '100%']], s.traffic_threshold_pct)}</label>
      <label class="row">renewal warning${sel('renewal_warn_days', [[3, '3 days'], [7, '7 days'], [14, '14 days'], [30, '30 days']], s.renewal_warn_days)}</label>
      <label class="row">directory measurement${sel('du_interval_hours', [[0, 'manual only'], [6, 'every 6 h'], [12, 'every 12 h'], [24, 'daily'], [168, 'weekly']], s.du_interval_hours)}</label>
    </div>
    <div class="panel"><div class="tile-label">qbittorrent-nox</div>
      <label class="f">download URL for install / update<input data-s="qbt_download_url" value="${esc(s.qbt_download_url || '')}"></label>
      <div class="hint">Default is the userdocs static x86_64 build. Installed to ~/bin/qbittorrent-nox, mode 700, after an ELF and --version check.</div>
    </div>
    <div class="panel"><div class="tile-label">pulsedmedia account</div>
      <label class="row">client area<input data-s="client_area_url" value="${esc(s.client_area_url || '')}"></label>
      ${s.client_area_url ? `<div class="row"><span></span><a href="${esc(safeHref(s.client_area_url))}" target="_blank" rel="noopener">open client area ↗</a></div>` : ''}
      <label class="row">API key<input data-s="pm_api_key" type="password" autocomplete="off" value="${esc(s.pm_api_key || '')}" placeholder="${s.clear_pm_api_key ? 'will be removed on save' : S.settings.has_pm_api_key ? '•••••••• (stored)' : 'paste key'}"></label>
      <div class="hint">Read-only customer API key from the client area (<a href="https://pulsedmedia.com/clients/index.php?m=pmapi" target="_blank" rel="noopener">?m=pmapi ↗</a>). Fills in plan names, prices and renewal dates for boxes whose host or username matches a service. Stored in the local database; leave blank to keep the current key.${S.settings.has_pm_api_key && !s.clear_pm_api_key ? ` <button class="small ghost" data-act="settings:clear-key">remove key</button>` : ''}</div>
    </div>
    <div class="form-foot" style="justify-content:flex-start"><button class="primary" data-act="settings:save">save settings</button></div>
    <div class="panel"><div class="tile-label">about &amp; licensing</div>
      <div class="row"><span>version</span><span>${esc(S.version || 'dev')}</span></div>
      <div class="hint">pmss-fleet-manager is free software under the GNU Affero General Public License v3 or later. Because you are using it over a network, you are entitled to its source code.</div>
      <div class="row" style="justify-content:flex-start;gap:16px"><a href="${esc(BASE)}/license" target="_blank" rel="noopener">license ↗</a><a href="${esc(BASE)}/third-party-licenses" target="_blank" rel="noopener">third-party notices ↗</a><a href="${esc(S.sourceUrl || BASE + '/source')}" target="_blank" rel="noopener">source code ↗</a></div>
    </div>
  </div>
  ${viewServices()}`;
}
function viewServices() {
  const v = S.services;
  if (!v?.configured) return '';
  const boxById = (id) => S.boxes.find((b) => b.id === id);
  const status = v.error ? `<span class="bad">${esc(v.error)}</span>` : v.synced_at ? `synced ${fmtAgo(v.synced_at)}${v.generated_at ? ` · feed generated ${fmtAgo(v.generated_at)}` : ''}` : 'not synced yet';
  return `<div class="svc">
    <div class="head" style="margin:24px 0 10px"><div><div class="eyebrow">pulsedmedia.com${v.client_id ? ` · client ${v.client_id}` : ''}</div><h2 style="margin:0">Services</h2><div class="hint" style="margin-top:4px">${status}</div></div>
      <div class="actions"><span class="muted" style="font-size:12px">${v.services.length} services${v.currency ? ` · ${fmtMoney(v.monthly_cost, v.currency)}/mo` : ''}</span><button data-act="svc:sync">sync now</button></div></div>
    ${v.services.length ? `<div class="table svc-table">
      <div class="trow head"><span>product</span><span>login</span><span>price</span><span>per month</span><span>next due</span><span>status</span><span>box</span></div>
      ${v.services.map((sv) => { const b = sv.box_id ? boxById(sv.box_id) : null; const d = daysUntil(sv.next_due); return `
      <div class="trow">
        <div class="ellip"><div class="ellip" title="${esc(sv.product)}">${esc(sv.product)}</div><div class="sub">${esc(sv.group || '')}${sv.registered ? ` · since ${esc(sv.registered)}` : ''}</div></div>
        <div class="ellip"><div class="ellip">${esc(sv.username || '—')}@${esc(sv.domain)}</div><div class="sub">${sv.login_url ? `<a href="${esc(safeHref(sv.login_url))}" target="_blank" rel="noopener">web ui ↗</a>` : ''}${sv.permanent_address ? ` <a href="${esc(safeHref(sv.permanent_address))}" target="_blank" rel="noopener">permanent ↗</a>` : ''}</div></div>
        <span class="num">${fmtMoney(sv.recurring_amount, sv.currency)} <span class="muted">/ ${esc(cycleShort(sv.billing_cycle))}</span></span>
        <span class="num">${sv.monthly ? fmtMoney(sv.monthly, sv.currency) : '—'}</span>
        <span class="${d !== null && d <= (S.settings.renewal_warn_days || 7) ? 'warn' : ''}">${sv.next_due ? `${esc(sv.next_due)} <span class="muted">· ${d < 0 ? 'overdue' : d + 'd'}</span>` : '—'}</span>
        <span class="${sv.status === 'Active' ? 'ok' : 'warn'}">${esc(sv.status)}</span>
        <span>${b ? `<a href="#/box/${b.id}" title="matched by ${esc(sv.matched_by || '')}">${esc(b.name)}</a>` : sv.username ? `<button class="small" data-act="svc:add" data-id="${sv.id}">+ add box</button>` : '<span class="muted">—</span>'}</span>
      </div>`; }).join('')}
    </div>` : '<div class="empty">no services on this account</div>'}
  </div>`;
}

/* ---------- events ---------- */
function afterRender() {
  // Tooltips for the traffic bars.
  const bars = $('#bars');
  if (bars) {
    let tip = null;
    bars.addEventListener('mousemove', (e) => {
      const bar = e.target.closest('.bar');
      if (!bar) { if (tip) { tip.remove(); tip = null; } return; }
      if (!tip) { tip = document.createElement('div'); tip.className = 'tip'; bars.appendChild(tip); }
      tip.textContent = bar.dataset.tip;
      const r = bars.getBoundingClientRect(), br = bar.getBoundingClientRect();
      tip.style.left = `${br.left - r.left + br.width / 2}px`;
      tip.style.top = `${Math.max(0, br.top - r.top + (br.height - (bar.querySelector('.seg')?.getBoundingClientRect().height || 0)) - 6)}px`;
    });
    bars.addEventListener('mouseleave', () => { if (tip) { tip.remove(); tip = null; } });
  }
}
document.addEventListener('click', async (e) => {
  const goEl = e.target.closest('[data-go]');
  if (goEl && !e.target.closest('[data-act]')) { e.preventDefault(); go(goEl.dataset.go); return; }
  const cs = e.target.closest('[data-cardsize]');
  if (cs) {
    S.settings.card_min_px = Number(cs.dataset.cardsize);
    render();
    // Send only this field: the server merges it onto its current settings,
    // so a stale copy in this tab cannot revert changes made elsewhere.
    api('PUT', BASE + '/api/settings', { card_min_px: S.settings.card_min_px }).then((d) => { S.settings = d; }).catch((err) => toast(err.message, 'bad'));
    return;
  }
  const fv = e.target.closest('[data-fleetview]');
  if (fv) { S.fleetView = fv.dataset.fleetview; S.fleetViewTouched = true; render(); return; }
  const tab = e.target.closest('[data-tab]');
  if (tab) { S.tab = tab.dataset.tab; S.filter = ''; S.torAdd = null; history.replaceState(null, '', `#/box/${S.selId}/${S.tab}`); render(); return; }
  const seg = e.target.closest('[data-s-seg]');
  if (seg) {
    S.settingsDraft[seg.dataset.sSeg] = seg.dataset.v; S.settingsTouched[seg.dataset.sSeg] = true;
    if (seg.dataset.sSeg === 'theme') { applyTheme(seg.dataset.v); }
    render(); return;
  }
  const act = e.target.closest('[data-act]');
  if (!act) return;
  e.preventDefault();
  if (act.dataset.confirm && !confirm(act.dataset.confirm)) return;
  const a = act.dataset.act;
  try {
    await runAction(a, act);
  } catch (err) {
    toast(err.message, 'bad');
    act.disabled = false;
  }
});
/* data-act handlers, one per action group. API errors propagate to the click
   handler, which shows the toast and re-enables the button. */
async function boxAction(a) {
  if (a === 'refresh-all') {
    const live = S.boxes.filter((b) => b.status !== 'disabled');
    const rs = await Promise.allSettled(live.map((b) => api('POST', `${BASE}/api/boxes/${b.id}/refresh`)));
    const failed = rs.filter((r) => r.status === 'rejected').length;
    if (failed) toast(`refresh requested for ${live.length - failed} of ${live.length} boxes; ${failed} failed`, 'warn');
    else toast(`refresh requested for ${live.length} boxes`);
  }
  else if (a === 'refresh') { await api('POST', `${BASE}/api/boxes/${S.selId}/refresh`); toast('refresh requested'); }
  else if (a === 'du') { const id = S.selId; await api('POST', `${BASE}/api/boxes/${id}/du`); toast('measuring directories at low priority…'); if (S.detail && S.detail.id === id) { S.detail.dirs.running = true; render(); } }
  else if (a === 'remove') { S.removingId = S.selId; await api('DELETE', `${BASE}/api/boxes/${S.selId}`); toast('box removed'); await loadState(); go('/fleet'); }
}
async function qbtAction(a, act) {
  const what = a.slice(4);
  act.disabled = true;
  if (what === 'install') toast('downloading qbittorrent-nox on the box…');
  const r = await api('POST', `${BASE}/api/boxes/${S.selId}/qbt/${what}`);
  toast(`qBittorrent ${what}: ok${r?.output ? ' — ' + r.output.split('\n').slice(0, 2).join(' · ') : ''}`, 'ok');
}
async function torrentAction(a, act) {
  const what = a.slice(4);
  act.disabled = true;
  await api('POST', `${BASE}/api/boxes/${act.dataset.box || S.selId}/qbt/torrents/${encodeURIComponent(act.dataset.hash)}/${what}`);
  toast(`torrent ${{ stop: 'stopped', start: 'started', remove: 'removed', 'remove-data': 'removed with its files' }[what]}`, 'ok');
  if (S.view === 'torrents') void loadTorrents();
}
async function discoveryAction(a, act) {
  const k = a.slice(5), on = !act.dataset.on;
  act.disabled = true;
  await api('POST', `${BASE}/api/boxes/${S.selId}/qbt/discovery`, { [k]: on });
  toast(`${k.toUpperCase()} ${on ? 'enabled' : 'disabled'}`, 'ok');
  await loadDetail(S.selId);
}
async function torAddAction(a) {
  if (a === 'tor-add:toggle') { S.torAdd = S.torAdd ? null : { urls: '', category: '', stopped: false, files: [], busy: false }; render(); return; }
  const d = S.torAdd;
  if (!d || d.busy) return;
  const fd = new FormData();
  fd.append('urls', d.urls);
  fd.append('category', d.category);
  fd.append('stopped', String(d.stopped));
  d.files.forEach((f) => fd.append('torrents', f, f.name));
  d.busy = true; render();
  try {
    const r = await api('POST', `${BASE}/api/boxes/${S.selId}/qbt/torrents`, fd);
    toast(addToast(r, d.stopped), r?.counted && r.failed ? 'warn' : 'ok');
    if (S.torAdd === d) S.torAdd = null; // not a draft the user opened meanwhile
  } finally {
    if (S.torAdd) S.torAdd.busy = false;
    render();
  }
}
async function appWatchAction(act) {
  const on = !act.dataset.on;
  act.disabled = true;
  await api('POST', `${BASE}/api/boxes/${S.selId}/apps/watch`, { key: act.dataset.key, on });
  toast(on ? 'watching: an alert is raised when it is not running' : 'no longer watched', 'ok');
  await loadDetail(S.selId);
}
async function containerAction(a, act) {
  act.disabled = true; await api('POST', `${BASE}/api/boxes/${S.selId}/containers/${act.dataset.cid}/${a.slice(4)}`); toast(`container ${a.slice(4)} ok`, 'ok');
}
async function alertsAction(a, act) {
  if (a === 'ack') { act.disabled = true; await api('POST', `${BASE}/api/alerts/${act.dataset.id}/ack`); toast('acknowledged', 'ok'); await loadAlerts(); }
  else if (a === 'ack-all') { await api('POST', BASE + '/api/alerts/ack-all'); toast('all alerts acknowledged', 'ok'); await loadAlerts(); }
  else if (a === 'alert:dismiss') { act.disabled = true; await api('DELETE', `${BASE}/api/alerts/${act.dataset.id}`); await loadAlerts(); }
  else if (a === 'alerts:clear-resolved') { await api('POST', BASE + '/api/alerts/clear-resolved'); toast('resolved alerts cleared', 'ok'); await loadAlerts(); }
}
async function settingsAction(a) {
  if (a === 'settings:save') {
    const d = { ...S.settingsDraft };
    for (const k of ['poll_interval_sec', 'disk_threshold_pct', 'traffic_threshold_pct', 'renewal_warn_days', 'du_interval_hours', 'card_min_px']) d[k] = Number(d[k]);
    const keyChanged = !!(d.pm_api_key || d.clear_pm_api_key);
    S.settings = await api('PUT', BASE + '/api/settings', d); S.settingsDraft = { ...S.settings, pm_api_key: '', clear_pm_api_key: false }; S.settingsTouched = {}; applyTheme(S.settings.theme); S.fleetViewTouched = false; S.fleetView = S.settings.fleet_view; toast('settings saved', 'ok'); render();
    if (keyChanged) { S.services = null; setTimeout(loadServices, 1500); }
  }
  else if (a === 'settings:clear-key') { S.settingsDraft.clear_pm_api_key = true; S.settingsDraft.pm_api_key = ''; render(); }
}
async function servicesAction(a, act) {
  if (a === 'svc:sync') { act.disabled = true; toast('syncing services from pulsedmedia.com…'); S.services = await api('POST', BASE + '/api/services/sync'); await loadState(); toast(`synced ${S.services.services.length} services`, 'ok'); render(); }
  else if (a === 'svc:add') {
    const sv = (S.services?.services || []).find((x) => String(x.id) === act.dataset.id);
    if (!sv) return;
    resetAdd();
    S.add.service = sv; // plan and renewal are not copied into the form: see addServiceHint
    Object.assign(S.add.form, { name: sv.username, host: sv.domain, user: sv.username, type: /storage/i.test(sv.product) ? 'storage' : 'seedbox' });
    go('/add');
  }
}
async function wizardAction(a) {
  if (a === 'add:next') {
    const A = S.add;
    if (A.step === 1) { A.errors = validateForm(A.form); if (Object.keys(A.errors).length) { render(); return; } A.step = 2; void runDetect(); }
    else if (A.step === 2) { A.step = 3; render(); }
  }
  else if (a === 'add:retry') { void runDetect(); }
  else if (a === 'add:back') { S.add.step = Math.max(1, S.add.step - 1); render(); }
  else if (a === 'add:confirm') { void confirmAdd(); }
  else if (a === 'edit:save') { const A = S.add; A.errors = validateForm(A.form); if (Object.keys(A.errors).length) { render(); return; } void confirmAdd(); }
}
async function runAction(a, act) {
  if (a === 'refresh-all' || a === 'refresh' || a === 'du' || a === 'remove') await boxAction(a);
  else if (a.startsWith('qbt:')) await qbtAction(a, act);
  else if (a.startsWith('tor:')) await torrentAction(a, act);
  else if (a.startsWith('tor-add:')) await torAddAction(a);
  else if (a.startsWith('disc:')) await discoveryAction(a, act);
  else if (a.startsWith('ctr:')) await containerAction(a, act);
  else if (a === 'app:watch') await appWatchAction(act);
  else if (a === 'ack' || a === 'ack-all' || a === 'alert:dismiss' || a === 'alerts:clear-resolved') await alertsAction(a, act);
  else if (a.startsWith('settings:')) await settingsAction(a);
  else if (a.startsWith('svc:')) await servicesAction(a, act);
  else if (a.startsWith('add:') || a === 'edit:save') await wizardAction(a);
}
document.addEventListener('submit', (e) => {
  if (e.target.closest('[data-tor-add]')) e.preventDefault(); // the submit button's click runs tor-add:submit
});
document.addEventListener('change', (e) => {
  const fi = e.target.closest('[data-ta-files]');
  if (fi && S.torAdd) { S.torAdd.files = [...fi.files]; render(); }
});
document.addEventListener('input', (e) => {
  const ta = e.target.closest('[data-ta]');
  if (ta && S.torAdd) { S.torAdd[ta.dataset.ta] = ta.type === 'checkbox' ? ta.checked : ta.value; return; }
  const f = e.target.closest('[data-f]');
  if (f && S.add.form) {
    const k = f.dataset.f;
    S.add.form[k] = f.type === 'checkbox' ? f.checked : f.value;
    // Forgetting the pin makes the next connect trust whatever key answers,
    // and with password auth the saved SSH password goes to it: say so
    // once, at the moment of ticking, rather than refusing.
    if (k === 'forget_host_key' && f.checked) {
      const fm = S.add.form, pw = fm.auth_method === 'password' || (!fm.auth_method && (fm.has_password || fm.password));
      if (pw && !confirm('The next connect will trust and pin whatever host key answers, and the SSH password will be sent to it. Only do this after a reinstall or migration you expect. Forget the pinned key?')) {
        f.checked = false; fm.forget_host_key = false;
      }
    }
    // A host key belongs to host:port; anything detect saw for a previous
    // endpoint must not be carried to a new one (or saved as its pin).
    if (k === 'host' || k === 'port' || k === 'user') { S.add.form.host_key = ''; if (k !== 'user') S.add.pinned = false; }
    if (S.add.errors[k]) { delete S.add.errors[k]; f.classList.remove('err'); }
    if (k === 'auth_method') render();
    return;
  }
  const s = e.target.closest('[data-s]');
  if (s && S.settingsDraft) { S.settingsDraft[s.dataset.s] = s.value; S.settingsTouched[s.dataset.s] = true; return; }
  const fl = e.target.closest('[data-filter]');
  if (fl) {
    S.filter = fl.value;
    const pos = fl.selectionStart;
    render();
    const nf = $('[data-filter]');
    if (nf) { nf.focus(); nf.setSelectionRange(pos, pos); }
  }
});
window.addEventListener('hashchange', route);

/* ---------- boot ---------- */
void (async () => {
  await loadState();
  resetAdd();
  route();
  connectEvents();
  setInterval(() => { if (S.view === 'fleet' || S.view === 'box') renderSide(); }, 15000);
})();
})();
