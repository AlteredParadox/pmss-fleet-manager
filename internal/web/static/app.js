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
const BASE = trimSlashes(location.pathname);
function trimSlashes(p) { // trailing slashes off, without a regex that backtracks on a run of them
  let end = p.length;
  while (end > 0 && p[end - 1] === '/') end--;
  return p.slice(0, end);
}

const S = {
  view: 'fleet', fleetView: 'cards', tab: 'overview', selId: null,
  boxes: [], totals: {}, unacked: 0, settings: {}, detail: null,
  snapRev: 0, removed: new Map(), alertsRev: 0, // revision bookkeeping, see static/revisions.js (pfmRevisions)
  alerts: [], filter: '', live: false,
  torAdd: null, // open "add torrent" form: { urls, category, stopped, files: File[], busy }
  torrents: null, // fleet-wide torrent list from /api/torrents: [{id, name, status, status_reason, qbt}]
  pmdb: null, // pmdb.fyi catalogue standing from /api/pmdb, loaded on the settings view
  logView: null, // open log on the logs tab: { kind: 'file'|'ctr', key, label, text, busy, err, at }
  logOpen: new Set(), // directories whose "older / rotated" group the user expanded on the logs tab
  add: { mode: 'add', step: 1, form: null, errors: {}, detect: null, busy: false, editId: null },
  settingsDraft: null, settingsTouched: {},
  torSort: loadTorSort(), // column sort per torrent table ('qbt', 'rtorrent', 'all'): { key, dir } or null for the default order
};
// The torrent tables' sort is a per-browser convenience: kept in
// localStorage when it is available, per tab otherwise.
function loadTorSort() { try { const v = JSON.parse(window.localStorage.getItem('pfm.torSort') || '{}'); return v && typeof v === 'object' ? v : {}; } catch { return {}; } }
function saveTorSort() { try { window.localStorage.setItem('pfm.torSort', JSON.stringify(S.torSort)); } catch { /* storage unavailable: this tab only */ } }

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
// Small shares keep a decimal: a tenant's slice of a whole host is often under 1%.
function fmtPctFine(p) { const n = Number(p) || 0; return `${n.toFixed(n < 10 ? 1 : 0)}%`; }
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
const boxById = (id) => S.boxes.find((b) => b.id === id);
// serviceActive mirrors the server's ServiceActive: Active and Suspended
// services count towards costs and renewals, every other status is history.
const serviceActive = (sv) => !!sv && /^(active|suspended)$/i.test(sv.status || '');
// serviceRenewal mirrors the server's EffectiveRenewal: an active or suspended
// (usually overdue) service's next due date is a renewal; a cancelled one's is history.
function serviceRenewal(sv) { return serviceActive(sv) ? sv.next_due || '' : ''; }
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
  if (method !== 'GET') opts.headers['X-PMSS-Request'] = '1'; // the server's CSRF guard: a form cannot send it
  if (body instanceof FormData) opts.body = body; // multipart: the browser sets the boundary
  else if (body !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const r = await fetch(path, opts);
  let data = null;
  try { data = await r.json(); } catch { /* no body */ }
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
    if (S.tab === 'logs') rescanLogsIfStale(); // a direct or reloaded #/box/<id>/logs never passes the tab click
  } else if (S.view === 'edit') {
    S.selId = Number(parts[1]) || null;
    void startEdit(S.selId);
  } else if (S.view === 'add' && S.add.mode !== 'add') {
    resetAdd();
  }
  if (S.view === 'alerts') void loadAlerts();
  if (S.view === 'torrents') void loadTorrents();
  if (S.view === 'settings') { S.settingsDraft = settingsDraftOf(S.settings); S.settingsTouched = {}; void loadServices(); void loadPMDB(); }
  S.filter = '';
  S.torAdd = null;
  S.logView = null;
  S.logOpen = new Set();
  render();
}
const go = (hash) => { location.hash = hash; };

/* ---------- data ---------- */
// Per-resource request sequences: only the newest-started request may set
// the fields that carry no revision of their own (settings, totals, the
// services and alerts lists). A successful save bumps the sequence too, so
// a GET that started before the save cannot land after it with the old
// values. Boxes and the alert badge carry revisions and merge on their own.
let stateSeq = 0, servicesSeq = 0, alertsSeq = 0, pmdbSeq = 0;
async function loadState() {
  const seq = ++stateSeq;
  try {
    const d = await api('GET', BASE + '/api/state');
    if (d.instance && S.instance && d.instance !== S.instance) { location.reload(); return; } // server was rebuilt: pick up new JS/CSS
    S.instance = d.instance || S.instance;
    S.sshKey = d.ssh_key || ''; // the -ssh-key default, named in the edit form's key path hint
    S.monitorOnly = !!d.monitor_only; // -monitor-only: the server refuses box-changing actions, so their controls are not drawn
    // Loads overlap (a resync, a settings event and a services event can
    // all be in flight): one taken before a snapshot that already landed
    // is superseded and must not put back a box removed between the two.
    const r = pfmRevisions.acceptSnapshot(S, d.boxes || [], d.rev || 0);
    pfmRevisions.acceptAlerts(S, d.alerts_unacked, d.alerts_rev || 0);
    if (!r.accepted || seq !== stateSeq) return true; // superseded: the boxes and badge merged by revision, the rest is stale
    S.version = d.version; S.sourceUrl = d.source_url;
    S.totals = d.totals || {};
    if (r.kept) recomputeTotals(); // the list is newer than the snapshot's totals
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
    if (S.detail && S.detail.id === id && (S.detail.rev || 0) > (d.state.rev || 0)) return; // a newer detail already landed
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
// The keys never come back from the server (has_* says whether one is
// stored), so the draft starts them blank and a save sends them only when
// typed; SECRET_SETTINGS are the fields a settings event must not overwrite
// in an open draft.
const SECRET_SETTINGS = new Set(['pm_api_key', 'pmdb_upload_key']);
function settingsDraftOf(settings) {
  return { ...settings, pm_api_key: '', clear_pm_api_key: false, pmdb_upload_key: '', clear_pmdb_upload_key: false };
}
async function loadPMDB() {
  const seq = ++pmdbSeq;
  let next;
  try { next = await api('GET', BASE + '/api/pmdb'); } catch (e) { next = { enabled: false, hosts: [], error: e.message }; }
  if (seq !== pmdbSeq) return;
  S.pmdb = next;
  if (S.view === 'settings') render();
}
async function loadServices() {
  const seq = ++servicesSeq;
  let next;
  try { next = await api('GET', BASE + '/api/services'); } catch (e) { next = { configured: false, services: [], error: e.message }; }
  if (seq !== servicesSeq) return; // a newer load or a sync result landed meanwhile
  S.services = next;
  if (S.view === 'settings') render();
}
async function loadAlerts() {
  const seq = ++alertsSeq;
  try {
    const d = await api('GET', BASE + '/api/alerts');
    if (seq !== alertsSeq) return;
    S.alerts = d; render();
  } catch (e) { toast(`alerts: ${e.message}`, 'bad'); }
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
  let resyncGen = 0;
  // Events missed while disconnected (a box removed during a server restart,
  // say) are only recovered by a full reload, and nothing later will repeat
  // them: keep trying until one lands or the stream drops again.
  const resync = async () => {
    const gen = ++resyncGen;
    let wait = 2000;
    while (S.live && gen === resyncGen) { // one attempt at a time, by design: each retry waits for the last
      if (await loadState()) {
        if (gen !== resyncGen) return;
        if (S.view === 'box' && S.selId) void loadDetail(S.selId); else if (S.view === 'alerts') void loadAlerts(); else if (S.view === 'torrents') void loadTorrents();
        render();
        return;
      }
      await new Promise((r) => setTimeout(r, wait));
      wait = Math.min(wait * 2, 30000);
    }
  };
  es.onopen = () => {
    S.live = true;
    // Always: whatever changed between the last snapshot and this
    // subscription was never sent, and the server subscribes before it
    // answers, so a snapshot taken now plus the stream misses nothing
    // (mergeSnapshot keeps whichever of the two is newer per box).
    void resync();
  };
  es.onerror = () => { S.live = false; resyncGen++; renderSide(); };
  es.addEventListener('state', (ev) => {
    const e = JSON.parse(ev.data);
    pfmRevisions.acceptAlerts(S, e.alerts_unacked, e.alerts_rev || 0);
    if (!pfmRevisions.applyState(S, e)) { renderSide(); return; } // stale: a snapshot or a later event already covered it
    recomputeTotals();
    if (S.view === 'box' && S.selId === e.box_id) void loadDetail(e.box_id);
    else if (S.view === 'torrents') loadTorrentsSoon();
    else if (S.view === 'fleet') renderSoon(); else renderSide();
  });
  es.addEventListener('removed', (ev) => {
    const e = JSON.parse(ev.data);
    pfmRevisions.acceptAlerts(S, e.alerts_unacked, e.alerts_rev || 0);
    if (!pfmRevisions.applyRemoved(S, e)) { renderSide(); return; }
    if (S.detail && S.detail.id === e.box_id) S.detail = null;
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
      for (const k of Object.keys(S.settings)) if (!S.settingsTouched[k] && !SECRET_SETTINGS.has(k)) S.settingsDraft[k] = S.settings[k];
      applyTheme(S.settingsDraft.theme);
      void loadPMDB(); // the other tab may have switched the catalogue off or changed its key: the panel follows
      render();
    });
  });
  es.addEventListener('services', async (ev) => {
    const e = JSON.parse(ev.data);
    pfmRevisions.acceptAlerts(S, e.alerts_unacked, e.alerts_rev || 0);
    await loadState();
    if (S.view === 'settings') void loadServices();
    else if (S.view === 'box' && S.selId) void loadDetail(S.selId);
    else render();
  });
  es.addEventListener('pmdb', (ev) => {
    const e = JSON.parse(ev.data);
    pfmRevisions.acceptAlerts(S, e.alerts_unacked, e.alerts_rev || 0);
    if (S.view === 'settings') void loadPMDB();
  });
  es.addEventListener('alerts', (ev) => {
    const e = JSON.parse(ev.data);
    pfmRevisions.acceptAlerts(S, e.alerts_unacked, e.alerts_rev || 0);
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
    const ct = pfmView.clientTotals(b); // both clients, like the server's totals (fred review on #124)
    t.torrents += ct.torrents; t.seeding += ct.seeding; t.containers += b.docker.running;
    const r = renewalOf(b);
    if (r && (!t.next_renewal || r < t.next_renewal)) t.next_renewal = r;
    if (serviceActive(b.service) && !seenSvc.has(b.service.id)) { seenSvc.add(b.service.id); t.monthly_cost += b.service.monthly || 0; t.currency = b.service.currency; }
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
      ${S.monitorOnly ? '<div class="muted" title="started with -monitor-only: torrent, client, discovery and container actions are refused; monitoring runs as usual">monitor-only</div>' : ''}
    </div>`;
}

function renderMain() {
  const m = $('#main');
  // The log viewer and the log list are rebuilt on every render (an SSE
  // event, a keystroke in the filter): keep where the reader was.
  const prevLog = $('#logview'), prevList = $('.loglist');
  const logTop = prevLog ? prevLog.scrollTop : null, listTop = prevList ? prevList.scrollTop : null;
  switch (S.view) {
    case 'box': m.innerHTML = viewBox(); break;
    case 'torrents': m.innerHTML = viewTorrents(); break;
    case 'add': case 'edit': m.innerHTML = viewAdd(); break;
    case 'alerts': m.innerHTML = viewAlerts(); break;
    case 'settings': m.innerHTML = viewSettings(); break;
    default: m.innerHTML = viewFleet();
  }
  const ll = $('.loglist');
  if (ll && listTop !== null) ll.scrollTop = listTop;
  const lv = $('#logview');
  if (lv && S.logView && !S.logView.busy) {
    if (!S.logView.scrolled) { lv.scrollTop = lv.scrollHeight; S.logView.scrolled = true; } // a freshly read log opens at its end, like tail
    else if (logTop !== null) lv.scrollTop = logTop;
  }
  if (S.view === 'box') {
    const b = curBox();
    if (b?.status === 'disabled') {
      // Polling is off: the server refuses every action, so say so up front.
      m.querySelectorAll('button[data-act^="qbt:"], button[data-act^="tor:"], button[data-act^="tor-add:"], button[data-act^="disc:"], button[data-act^="ctr:"], button[data-act^="log:"], button[data-act="du"]').forEach((el) => {
        el.disabled = true;
        el.title = 'polling is disabled for this box; enable it to run actions';
      });
    } else if (b && !b.ready) {
      // No complete probe of this box since it was added, moved or the
      // dashboard started: the server refuses everything that would act
      // on facts read from it (its UID, container IDs, the log list).
      m.querySelectorAll('button[data-act^="qbt:"], button[data-act^="tor:"], button[data-act^="tor-add:"], button[data-act^="disc:"], button[data-act^="ctr:"], button[data-act^="log:"]').forEach((el) => {
        el.disabled = true;
        el.title = 'waiting for the first poll of this box before running actions';
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
// Torrent count across the monitored clients; the ratio is qBittorrent's
// (rTorrent reports none fleet-wide). Empty when no client is monitored.
function torrentsCell(b) {
  const ct = pfmView.clientTotals(b);
  if (!ct.clients) return '';
  const ratio = b.qbt.enabled ? ` <span class="muted">r${b.qbt.global_ratio ? b.qbt.global_ratio.toFixed(2) : '—'}</span>` : '';
  return `${ct.torrents}${ratio}`;
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
        <div class="tags"><span class="tag">${esc(b.type)}</span>${planOf(b) ? `<span class="tag" title="plan">${esc(planOf(b))}</span>` : ''}${b.host_sys.cpu_short ? `<span class="tag" title="${esc(b.host_sys.cpu_model)}">${esc(b.host_sys.cpu_short)}${b.host_sys.hw_encode === 'QSV' || b.host_sys.hw_encode === 'VAAPI' || b.host_sys.hw_encode === 'NVENC' ? ' · ' + esc(b.host_sys.hw_encode) : ''}</span>` : ''}${b.qbt.enabled && b.qbt.version ? `<span class="tag" title="qBittorrent${b.qbt.update_available ? ', update available' : ''}">qbt ${esc(b.qbt.version)}${b.qbt.update_available ? ' ↑' : ''}</span>` : ''}${b.rtorrent?.enabled && b.rtorrent.version ? `<span class="tag" title="rTorrent">rt ${esc(b.rtorrent.version)}</span>` : ''}${discoveryTag(b.qbt)}</div>
      </div>
      <div style="display:grid;gap:8px">
        <div><div class="kv"><span>disk</span><b>${b.disk.ok || b.disk.quota_bytes ? `${fmtBytes(b.disk.used_bytes, 1)} / ${fmtBytes(b.disk.quota_bytes, 1)}` : 'n/a'}</b></div>
          <div class="meter"><i class="${levelClass(b.disk.pct, dt)}" style="width:${Math.min(100, b.disk.pct)}%"></i></div></div>
        <div><div class="kv"><span>upload 30d${b.traffic.throttled ? ' <span class="bad">· throttled</span>' : ''}</span><b>${trafficLabel(b)}</b></div>
          <div class="meter"><i class="${tl ? levelClass(b.traffic.pct, tt) : 'dim'}" style="width:${trafficPct(b)}%"></i></div></div>
      </div>
      <div class="card-foot">
        <div><div class="l">up/down</div><div><span class="up">↑</span>${fmtBits(b.rates.up_bps)} <span class="down">↓</span>${fmtBits(b.rates.down_bps)}</div></div>
        <div><div class="l">torrents</div><div>${torrentsCell(b) || (b.docker.enabled ? `${b.docker.running} <span class="muted">containers</span>` : '—')}</div></div>
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
      <span class="num">${torrentsCell(b) || '—'}</span>
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
  if (pfmView.torrentsTab(b)) tabs.push(['torrents', 'torrents']); // the stopped-on-purpose note and a removal notice live there too
  if (b.docker.enabled || b.docker.socket_up) tabs.push(['containers', 'containers']);
  tabs.push(['services', 'services'], ['logs', 'logs'], ['storage', 'storage'], ['host', 'host']);
  if (!tabs.some(([k]) => k === S.tab)) S.tab = 'overview';
  const renewDays = daysUntil(renewalOf(b));
  let body = '';
  switch (S.tab) {
    case 'traffic': body = tabTraffic(b); break;
    case 'torrents': body = tabTorrents(b); break;
    case 'containers': body = tabContainers(b); break;
    case 'services': body = tabServices(b); break;
    case 'logs': body = tabLogs(b); break;
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
// A panel-managed client is started by Pulsed Media's watchdog: restart only stops it, and there is no start.
function boxPowerBtn(b, q) {
  if (S.monitorOnly) return '';
  const again = q.pmss_managed ? 'then Pulsed Media\'s watchdog starts it again' : 'then started again with the box\'s start command (default: ~/bin/qbt-start when present)';
  return q.enabled ? (q.process_up
    ? `<button data-act="qbt:restart" data-confirm="Restart qbittorrent-nox on ${esc(b.name)}? It is asked to exit cleanly, ${again}">${q.restart_needed ? '<span class="warn">restart client</span>' : 'restart client'}</button><button data-act="qbt:stop" data-confirm="Stop qbittorrent-nox on ${esc(b.name)}? Torrents stop seeding until it is started again (the cron helper restarts it within 5 min if installed).">stop client</button>`
    : q.pmss_managed ? '' : `<button data-act="qbt:start">start client</button>`) : '';
}
function boxInstallBtn(b, q) {
  if (q.pmss_managed || S.monitorOnly) return '';
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
  const rateNote = pfmView.rateSourceLabel(b.rates.source);
  // PMSS's cap_mbit is the upload rate the account is held to while it is
  // over its traffic limit, not a standing cap: only meter against it then.
  const cap = tr.throttled && tr.cap_mbit ? tr.cap_mbit : 0;
  return `<div class="panel"><div class="tile-label">bandwidth now</div>
      <div class="tile-value"><span class="nw"><span class="up">↑</span> ${fmtBits(b.rates.up_bps)}</span> <small class="nw"><span class="down">↓</span> ${fmtBits(b.rates.down_bps)}</small></div>
      ${cap ? `<div class="meter thick" title="upload vs. the ${cap} Mbit/s it is throttled to"><i style="width:${Math.min(100, (b.rates.up_bps / (cap * 1e6)) * 100)}%"></i></div>` : ''}
      <div class="note">${rateNote}${cap ? ` · throttled to ${cap} Mbit/s` : ''}</div></div>`;
}
function ovCpuRamPanel(b) {
  // The bars are your usage against your own quota and limit (static/view.js);
  // your share of the whole host is text only. The host tab has the host.
  const share = pfmView.hostShare(b.cpu);
  const ramShare = pfmView.ramHostShare(b.ram, b.host_sys);
  return `<div class="panel"><div class="tile-label">cpu / ram</div>
      <div class="tile-value">${b.cpu.ok ? fmtPct(b.cpu.pct) : '—'} <small>cpu${b.cpu.quota_cpus ? ` · of ${b.cpu.quota_cpus.toFixed(1)} cores` : b.cpu.host_cpus ? ` · of ${b.cpu.host_cpus} host cores` : ''}${share !== null && b.cpu.quota_cpus ? ` · ${fmtPctFine(share)} of the host` : ''}</small></div>
      ${pfmView.cpuQuotaMeter(b.cpu)}
      <div class="note" style="display:flex;justify-content:space-between;gap:8px"><span title="process memory (rss) against the slice limit; page cache is charged to the slice too but is reclaimed, not OOM-killed">ram ${b.ram.ok ? fmtPct(b.ram.pct) : '—'} · ${b.ram.rss_bytes ? `${fmtBytes(b.ram.rss_bytes, 1)} proc + ${fmtBytes(b.ram.cache_bytes, 1)} cache` : fmtBytes(b.ram.used_bytes, 1)} / ${b.ram.limit_bytes ? fmtBytes(b.ram.limit_bytes, 1) : '?'}${b.ram.last_oom && Date.now() - Date.parse(b.ram.last_oom) < 864e5 ? ` · <span class="bad" title="kernel OOM kill noticed ${esc(b.ram.last_oom)}">OOM kill</span>` : ''}</span><span>${ramShare !== null ? `${fmtPctFine(ramShare)} of the host` : ''}${b.ram.pids ? `${ramShare !== null ? ' · ' : ''}${b.ram.pids} proc` : ''}</span></div>
      <div style="margin-top:4px">${pfmView.ramLimitMeter(b.ram, levelClass(b.ram.pct, 95))}</div></div>`;
}
/* overview "box" panel rows */
function ovHostStatusRows(b) {
  return `<div><span>status</span><span class="${b.status === 'online' ? 'ok' : b.status === 'offline' ? 'bad' : 'warn'}">${esc(statusLabel(b.status))}</span></div>
      <div><span>last poll</span><span>${fmtAgo(b.last_poll)}${b.poll_ms ? ` <span class="muted">· ${b.poll_ms} ms</span>` : ''}</span></div>`;
}
function ovClientRows(q, r) {
  if (!q.enabled && !q.installed && r?.enabled) return ''; // rTorrent alone: its own row says it all
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
  const stale = q.start_helper_stale ? ' <span class="warn" title="~/bin/qbt-start checks for a running client with $USER, which is empty under cron, so every cron run starts another copy. Reapply rewrites the helper (the old one is saved to ~/.config/pmss-fleet-manager/qbt-start.bak).">helper needs a rewrite</span>' : '';
  return `<div><span>autostart</span><span>${q.start_helper && q.cron_installed ? 'qbt-start + cron' : q.start_helper ? 'qbt-start present, <span class="warn">no cron</span>' : '<span class="warn">not set up</span>'}${stale}${S.monitorOnly ? '' : ` <button class="small ghost" data-act="qbt:autostart" data-confirm="Write ~/bin/qbt-start (if missing, or if it keys on $USER, which is empty under cron) and set the @reboot and */5 crontab entries on ${esc(b.name)}? Other crontab lines are kept; a replaced helper is saved first.">${q.start_helper && q.cron_installed ? 'reapply' : 'set up'}</button>`}</span></div>`;
}
// binary + autostart rows: shown whenever the client is monitored or at least present on the box.
function ovAutostartRows(b, q) {
  return (q.enabled || q.installed) ? `${ovBinaryRow(q)}
      ${ovAutostartRow(b, q)}` : '';
}
function ovWebuiAuthRow(q) {
  const bypass = q.local_bypass ? 'localhost bypass on' : q.subnet_bypass ? `subnet bypass on (${esc(q.subnet_list || '')})` : '';
  return q.enabled ? `<div><span>webui auth</span><span class="muted">${bypass ? `<span class="bad" title="qBittorrent accepts WebUI requests from the box's loopback without a password. On a shared host every other tenant can reach that port: turn the bypass off in qBittorrent and give the dashboard the WebUI login instead.">${bypass} — unsafe on a shared host</span>` : 'login required'}${q.has_creds ? ' · credentials set' : ''}${q.conf_user ? ` · user ${esc(q.conf_user)}` : ''}${q.auth_hold ? ' · <span class="warn" title="A login was rejected. To avoid qBittorrent banning the box\'s loopback IP, no further attempts are made until you re-enter the password under edit or restart the client.">login suspended</span>' : ''}</span></div>` : '';
}
function ovTorrentsRow(q, both) {
  return q.enabled ? `<div><span>torrents${both ? ' <span class="muted">(qBittorrent)</span>' : ''}</span><span>${q.total} total · ${q.seeding} seeding · ratio ${q.global_ratio ? q.global_ratio.toFixed(2) : '—'}</span></div>` : '';
}
// rTorrent is PMSS's own client: no binary, autostart or WebUI rows, just
// whether it answers on its socket and what it holds.
function ovRtRows(r, both) {
  if (!r) return '';
  if (!r.enabled) {
    if (r.process_up) return `<div><span>rTorrent</span><span>running, monitoring off</span></div>`;
    if (r.disabled) return `<div><span>rTorrent</span><span class="muted" title="~/.rtorrentDisable is set: the Pulsed Media watchdog leaves it stopped">stopped on purpose · monitoring off</span></div>`;
    return '';
  }
  const state = r.reachable ? '' : r.disabled ? ' <span class="muted" title="~/.rtorrentDisable is set: the Pulsed Media watchdog leaves it stopped">(stopped on purpose)</span>' : ' <span class="warn">(not responding)</span>';
  return `<div><span>rTorrent</span><span>${esc(r.version || '')}${r.library ? ` <span class="muted">· libtorrent ${esc(r.library)}</span>` : ''}${state}${r.endpoint ? ` <span class="muted" title="SCGI endpoint">· ${esc(r.endpoint.replace(/^unix:/, ''))}</span>` : ''}</span></div>
      ${r.enabled && !r.reachable && r.error && !r.disabled ? `<div><span>rTorrent error</span><span class="warn" style="text-align:right">${esc(r.error)}</span></div>` : ''}
      ${r.reachable && r.listen_port ? `<div><span>rTorrent port</span><span>${r.listen_port}${pfmView.rtDiscoveryOn(r).labels.length ? ` <span class="warn" title="${esc(pfmView.rtDiscoveryOn(r).title)}">· ${pfmView.rtDiscoveryOn(r).labels.join(' · ')} on</span>` : ' · <span title="DHT is not running; PeX is off as the default for new torrents (torrents already loaded keep their own setting)">DHT off · PeX default off</span>'}</span></div>` : ''}
      ${r.reachable ? `<div><span>torrents${both ? ' <span class="muted">(rTorrent)</span>' : ''}</span><span>${r.total} total · ${r.seeding} seeding · ${r.leeching} leeching</span></div>` : ''}`;
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
      ${ovClientRows(q, b.rtorrent)}
      ${ovConnectionRow(q)}
      ${ovAutostartRows(b, q)}
      ${ovWebuiAuthRow(q)}
      ${ovTorrentsRow(q, q.enabled && b.rtorrent?.enabled)}
      ${ovRtRows(b.rtorrent, q.enabled && b.rtorrent?.enabled)}
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
// The torrent clients monitored on a box, in display order. Each entry
// carries what the tables need: the API segment, the display name and the
// client's state.
const CLIENT_LABEL = { qbt: 'qBittorrent', rtorrent: 'rTorrent' };
function boxClients(b) {
  const out = [];
  if (pfmView.showClient(b, 'qbt')) out.push({ key: 'qbt', label: 'qBittorrent', st: b.qbt });
  if (pfmView.showClient(b, 'rtorrent')) out.push({ key: 'rtorrent', label: 'rTorrent', st: b.rtorrent }); // monitored, or carrying a removal notice
  return out;
}
function tabTorrents(b) {
  const clients = boxClients(b);
  // rTorrent stopped on purpose (~/.rtorrentDisable) with monitoring off:
  // say so rather than hiding the client, since that is the useful fact.
  const rtOffNote = !b.rtorrent?.enabled && b.rtorrent?.disabled ? `<div class="empty" style="margin-bottom:12px">rTorrent is stopped on purpose on this box (<code>~/.rtorrentDisable</code>); monitoring is off.</div>` : '';
  if (clients.length === 0) return `${rtOffNote}<div class="empty">no torrent client is monitored on this box — enable qBittorrent or rTorrent under <a href="#/edit/${b.id}">edit</a>.</div>`;
  const f = S.filter.trim().toLowerCase();
  // Each client gets a table of its own, with its own header, counts,
  // add button and column sort; the filter (in the first) spans all.
  return `${rtOffNote}${clients.map((c, i) => `<div class="table tor-table" style="overflow:hidden${i ? ';margin-top:14px' : ''}">${clientSection(b, c, f, i === 0)}</div>`).join('')}`;
}
// One client's header line, add form and table.
function clientSection(b, c, f, first) {
  const q = c.st;
  let list = q.torrents || [];
  if (f) list = list.filter((t) => torrentMatches(t, f));
  list = pfmView.sortTorrents(list, S.torSort[c.key]);
  const shown = list.slice(0, 300);
  const notes = c.key === 'qbt' ? `${discoveryNote(q, b)}${connectionNote(q)}` : rtNote(q, b);
  const avail = pfmView.clientAvailability(q); // stopped on purpose is expected: no warning, nothing to add to
  if (avail.stopped && S.torAdd && (S.torAdd.client || 'qbt') === c.key && !S.torAdd.busy) S.torAdd = null; // an open draft for a client that just stopped is dropped
  const addOpen = !avail.stopped && S.torAdd && (S.torAdd.client || 'qbt') === c.key;
  // A remove-with-data that failed after its request had ended: the job
  // recorded it on the box (it also raised an alert); shown here until
  // dismissed, since a later success says nothing about these files. The
  // button names the notice it shows (its time), so it cannot dismiss a
  // newer one that replaced it meanwhile (daybreak blue re-review of #123).
  const removeFailed = c.key === 'rtorrent' && q.remove_failed ? `<div class="meta"><span class="bad" title="${q.remove_failed_at && !String(q.remove_failed_at).startsWith('1970') ? esc(fmtAgo(q.remove_failed_at)) + '. ' : ''}The torrent was removed from rTorrent but its files were not (all) deleted; check the download folder on the box.">remove with files failed: ${esc(q.remove_failed)}</span> <button class="small ghost" data-act="rt:dismiss-failure" data-at="${esc(q.remove_failed_at || '')}" title="remove this notice (the alert resolves with it)">dismiss</button></div>` : '';
  if (!q.enabled) {
    // Monitoring is off: only the notice is left to show (and dismiss); no
    // counts, no controls, nothing to add to.
    return `<div class="thead-tools">
      <div style="font-size:12px"><b>${c.label}</b> <span class="muted">· monitoring is off</span>${removeFailed}</div>
    </div>`;
  }
  return `<div class="thead-tools">
      <div style="font-size:12px"><b>${c.label} ${esc(q.version || '')}</b> <span class="muted">· ${q.total} torrents · ${q.seeding} seeding · ${q.leeching} leeching · ${q.paused} paused${q.errored ? ` · <span class="bad">${q.errored} error</span>` : ''}${transferNote(q.torrents || [])}${notes}${avail.down ? ` · <span class="warn">not responding${q.error ? ': ' + esc(q.error) : ''}</span>` : ''}</span>${removeFailed}</div>
      <div class="actions">
        ${first ? `<input class="filter" placeholder="filter…" data-filter value="${esc(S.filter)}">` : ''}
        ${c.key === 'qbt' && !S.monitorOnly ? `<button class="small" data-act="qbt:pause" title="stop every torrent in qBittorrent" data-confirm="Stop all torrents in qBittorrent on ${esc(b.name)}?">stop all</button>
        <button class="small" data-act="qbt:resume" title="start every torrent, including any stopped on purpose">start all</button>` : ''}
        ${avail.stopped || S.monitorOnly ? '' : `<button class="small" data-act="tor-add:toggle" data-client="${c.key}">${addOpen ? 'close' : 'add'}</button>`}
      </div>
    </div>
    ${addOpen ? torAddForm(b, q.torrents || [], c) : ''}
    <div style="overflow:auto">
      ${TOR_HEAD(c.key)}
      ${shown.length === 0 ? `<div class="trow"><span class="muted">${emptyTorrents(q.total, q.torrents, b.status)}</span></div>` : shown.map((t) => torrentRow(b, t)).join('')}
      ${list.length > shown.length ? `<div class="trow"><span class="muted">showing ${shown.length} of ${list.length} — refine the filter</span></div>` : ''}
    </div>`;
}
// rTorrent's header notes: its listen port, the DHT / PeX toggles, and
// the on-purpose stop.
function rtNote(r, b) {
  if (!r.reachable) return r.disabled ? ' · <span class="muted" title="~/.rtorrentDisable is set">stopped on purpose</span>' : '';
  // Where "+ files" may delete: rTorrent's download folder as read from the
  // box's own config (never from the client). Said up front so a refusal
  // for data kept elsewhere is no surprise.
  const fence = r.data_dir ? ` · <span class="muted" title="Remove with files deletes only inside rTorrent's download folder (${esc(r.data_dir)}, from directory.default in ~/.rtorrent.rc on the box). Data kept anywhere else, or a torrent that never started, is refused and the torrent left in place; plain remove always works. To move the fence, set directory.default.set in ~/.rtorrent.rc.custom.">files: ${esc(r.data_dir)}</span>` : '';
  return `${r.listen_port ? ` · port ${r.listen_port}` : ''}${rtDiscoveryNote(r, b)}${fence}`;
}
// rTorrent's DHT / PeX toggles, styled like qBittorrent's. A click writes
// the setting to ~/.rtorrent.rc.custom and applies it live; the button
// shows what the file says, with ↻ while the running client still differs
// (a change it could not take live waits for a restart). rTorrent has no
// local service discovery, so LSD is shown as not applicable.
function rtDiscoveryNote(r, b) {
  const list = pfmView.rtDiscovery(r);
  if (!list.length) return '';
  return ' · ' + list.map((d) => {
    if (S.monitorOnly) return discState(`${d.label} ${d.on ? 'on' : 'off'}${d.pending ? ' ↻' : ''}`, d.warn, pfmView.rtDiscoveryState(d));
    return `<button class="disc ${d.warn ? 'warn' : ''}" data-act="disc:${d.k}" data-client="rtorrent" data-on="${d.on ? '1' : ''}" title="${esc(pfmView.rtDiscoveryHint(d))}"${d.on ? '' : ` data-confirm="Enable ${d.label} for rTorrent on ${esc(b.name)}? It is saved to ~/.rtorrent.rc.custom. Private trackers usually forbid it; torrents from them may be flagged or banned."`}>${d.label} ${d.on ? 'on' : 'off'}${d.pending ? ' ↻' : ''}</button>`;
  }).join(' · ') + ' · <span class="disc na" title="rTorrent has no local service discovery">LSD n/a</span>';
}

const KIND_COLOR = { seeding: 'var(--ok)', leeching: 'var(--accent)', paused: 'var(--dim)', checking: 'var(--warn)', error: 'var(--bad)', unknown: 'var(--dim)' };
// Column headings shared by the torrent tables, each a sort button for its
// table ('qbt', 'rtorrent' or 'all'); the fleet-wide list adds the client
// and the seedbox after the peers.
const TOR_COLS = [['name', 'name'], ['size', 'size'], ['progress', 'progress'], ['up', 'up'], ['down', 'down'], ['uploaded', 'uploaded', 'lifetime uploaded'], ['downloaded', 'downloaded', 'lifetime downloaded'], ['ratio', 'ratio'], ['seeds', 'seeds', 'connected seeds (swarm total)'], ['peers', 'peers', 'connected peers (swarm total)']];
function sortHead(table, key, label, hint) {
  const s = S.torSort[table], on = s?.key === key;
  return `<button class="th-sort${on ? ' on' : ''}" data-act="sort:tor" data-table="${table}" data-key="${key}" title="${hint ? hint + ' — ' : ''}sort by ${label}${on ? ' (click again to reverse, a third time for the default order)' : ''}">${label}${on ? (s.dir === 'asc' ? ' ▲' : ' ▼') : ''}</button>`;
}
const TOR_HEAD = (table, fleet = false) => `<div class="trow head">${TOR_COLS.map(([k, l, h]) => sortHead(table, k, l, h) + (fleet && k === 'peers' ? sortHead(table, 'client', 'client') + sortHead(table, 'box', 'seedbox') : '')).join('')}<span></span></div>`;
// Lifetime transfer summed over a list, for the table headers.
function transferNote(list) {
  let up = 0, down = 0;
  for (const t of list) { up += t.uploaded || 0; down += t.downloaded || 0; }
  return list.length ? ` · ↑ ${fmtBytes(up, 1)} ↓ ${fmtBytes(down, 1)} lifetime` : '';
}
// "3 <swarm>/120</swarm>": connected count, then the swarm size when known.
const swarmCell = (n, swarm) => `<span class="num">${n}${swarm ? `<span class="muted swarm">/${swarm}</span>` : ''}</span>`;
const torrentMatches = (t, f) => t.name.toLowerCase().includes(f) || (t.category || '').toLowerCase().includes(f) || t.kind.includes(f);
// One torrent row, shared by the per-box tab and the fleet-wide list; the
// fleet list adds the client and the seedbox after the peers, so a box
// running both clients reads unambiguously and both columns sort.
function torrentRow(b, t, fleet = false) {
  const color = KIND_COLOR[t.kind] || 'var(--dim)';
  const hint = t.kind === 'error' && t.state && t.state !== t.kind ? ` — ${esc(t.state)}` : '';
  const where = fleet ? `<span class="ellip muted">${CLIENT_LABEL[t.client] || esc(t.client || 'qBittorrent')}</span>${boxCell(b, t)}` : '';
  return `
      <div class="trow">
        <div style="min-width:0;display:flex;align-items:center;gap:8px" title="${esc(t.name)}${hint}"><span class="led" style="width:6px;height:6px;background:${color}"></span><span class="ellip">${esc(t.name)}</span></div>
        <span class="muted num">${fmtBytes(t.size, 1)}</span>
        <div><div style="display:flex;justify-content:space-between;font-size:10.5px;color:var(--muted)"><span>${esc(t.kind === 'unknown' ? t.state : t.kind)}</span><span>${(t.progress * 100).toFixed(t.progress < 1 ? 1 : 0)}%</span></div><div class="meter" style="height:3px;margin-top:3px"><i style="width:${t.progress * 100}%;background:${color}"></i></div></div>
        <span class="num ${t.up_bps ? 'up' : 'dim'}">${fmtBps(t.up_bps)}</span>
        <span class="num ${t.down_bps ? 'down' : 'dim'}">${fmtBps(t.down_bps)}</span>
        <span class="num ${t.uploaded ? '' : 'dim'}">${fmtBytes(t.uploaded || 0, 1)}</span>
        <span class="num ${t.downloaded ? '' : 'dim'}">${fmtBytes(t.downloaded || 0, 1)}</span>
        <span class="num">${t.ratio >= 0 ? t.ratio.toFixed(2) : '∞'}</span>
        ${swarmCell(t.seeds || 0, t.seeds_swarm)}
        ${swarmCell(t.peers || 0, t.peers_swarm)}
        ${where}
        <div class="actions">${torrentButtons(b, t, fleet)}</div>
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
    for (const c of boxClients(b)) {
      const q = c.st;
      for (const k of Object.keys(sum)) sum[k] += q[k] || 0;
      if (pfmView.clientAvailability(q).down) notes.push(`<span class="warn">${esc(b.name)} (${c.label}): ${unreachableNote(b, c)}</span>`);
      for (const t of q.torrents || []) if (!f || torrentMatches(t, f) || b.name.toLowerCase().includes(f) || c.label.toLowerCase().includes(f)) rows.push([b, t]);
    }
  }
  // Same order as the per-box tab (active first, busiest first), then by
  // box so one box's idle torrents stay together.
  rows.sort(([ba, a], [bb, c]) => {
    const ra = a.up_bps + a.down_bps, rb = c.up_bps + c.down_bps;
    if ((ra > 0) !== (rb > 0)) return ra > 0 ? -1 : 1;
    if (ra !== rb) return rb - ra;
    return ba.name.localeCompare(bb.name) || ba.id - bb.id;
  });
  const shown = pfmView.sortTorrents(rows, S.torSort.all, (r) => r[1], (r) => r[0].name).slice(0, 500);
  const qbtBoxes = boxes.filter((b) => boxClients(b).length).length;
  const allTorrents = (list) => list.flatMap((b) => boxClients(b).flatMap((c) => c.st.torrents || []));
  return `<div class="head"><div><div class="eyebrow">PMSS</div><h1>Torrents</h1></div></div>
  <div class="table tor-table tor-all" style="overflow:hidden;margin-top:16px">
    <div class="thead-tools">
      <div style="font-size:12px"><b>${sum.total} torrents</b> <span class="muted">· ${qbtBoxes} seedbox${qbtBoxes === 1 ? '' : 'es'} · ${sum.seeding} seeding · ${sum.leeching} leeching · ${sum.paused} paused${sum.errored ? ` · <span class="bad">${sum.errored} error</span>` : ''}${transferNote(allTorrents(boxes))}${notes.length ? ' · ' + notes.join(' · ') : ''}</span></div>
      <div class="actions"><input class="filter" placeholder="filter name, label, state, client or box…" data-filter value="${esc(S.filter)}" style="max-width:260px"></div>
    </div>
    <div style="overflow:auto">
      ${TOR_HEAD('all', true)}
      ${qbtBoxes === 0 ? `<div class="trow"><span class="muted">no box has a torrent client monitored</span></div>`
        : shown.length === 0 ? `<div class="trow"><span class="muted">${emptyTorrents(sum.total, allTorrents(S.torrents), boxes.some((b) => b.status !== 'disabled' && boxClients(b).some((c) => c.st.total)) ? '' : 'disabled')}</span></div>`
        : shown.map(([b, t]) => torrentRow(b, t, true)).join('')}
      ${rows.length > shown.length ? `<div class="trow"><span class="muted">showing ${shown.length} of ${rows.length} — refine the filter</span></div>` : ''}
    </div>
  </div>`;
}
// The seedbox cell: a link to the box's own torrents tab, with the enabled
// discovery mechanisms in parentheses so private-tracker placement is
// visible per row.
function boxCell(b, t) {
  const rt = t?.client === 'rtorrent' ? pfmView.rtDiscoveryOn(b.rtorrent) : null;
  const on = rt ? rt.labels : discoveryOn(b.qbt);
  return `<div class="ellip" title="${esc(b.name)}${on.length ? ' · ' + esc(rt ? rt.title : on.join(', ') + ' enabled') : ''}"><a href="#/box/${b.id}/torrents">${esc(b.name)}</a>${on.length ? ` <span class="warn">(${esc(on.join(' · '))})</span>` : ''}</div>`;
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
// A discovery setting as plain text, for monitor-only mode: the state
// stays visible, the toggle goes.
function discState(text, warn, title) {
  return `<span class="disc ${warn ? 'warn' : ''}" title="${esc(title)}">${esc(text)}</span>`;
}
function discoveryNote(q, b) {
  if (!q.discovery?.known) return q.reachable ? ' · <span class="muted">discovery settings not read</span>' : '';
  return ' · ' + DISCOVERY.map(([k, l]) => {
    const on = q.discovery[k];
    if (S.monitorOnly) return discState(`${l} ${on ? 'on' : 'off'}`, on, `${l} ${on ? 'enabled' : 'disabled'}`);
    return `<button class="disc ${on ? 'warn' : ''}" data-act="disc:${k}" data-on="${on ? '1' : ''}" title="${l} ${on ? 'enabled — click to disable' : 'disabled — click to enable'}"${on ? '' : ` data-confirm="Enable ${l} on ${esc(b.name)}? Private trackers usually forbid it; torrents from them may be flagged or banned."`}>${l} ${on ? 'on' : 'off'}</button>`;
  }).join(' · ');
}
// Toast after an add. qBittorrent 5.2+ reports per-entry counts; older
// clients only say that at least one entry was accepted.
// The add's outcome, worded by the pure helper (tested in view_test.mjs).
function addToast(r, stopped, client) { return pfmView.addSummary(r, stopped, client); }
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
function unreachableNote(b, c) {
  if (b.status === 'disabled') return 'polling disabled';
  const q = c?.st || b.qbt || {}, reason = b.status_reason || '';
  const clientDown = b.status === 'online' || /qbittorrent|rtorrent/i.test(reason);
  const detail = clientDown && q.error ? ` — ${esc(q.error)}` : '';
  if (b.status === 'online') return `client not responding${detail}`;
  return `box ${esc(b.status || 'unknown')}${reason ? ` (${esc(reason)})` : ''}${detail}`;
}
// The add-torrent form. Its draft lives in S.torAdd because every state
// event re-renders the tab; File objects are kept there too, since a file
// input cannot be refilled, and the chosen names are listed beside it.
function torAddForm(b, list, c) {
  const a = S.torAdd;
  const rt = c?.key === 'rtorrent';
  const cats = [...new Set(list.map((t) => t.category).filter(Boolean))].sort((x, y) => x.localeCompare(y));
  return `<form class="tor-add" data-tor-add>
    <label class="f" style="grid-column:1/-1">magnet links or .torrent URLs, one per line
      <textarea rows="3" data-ta="urls" placeholder="magnet:?xt=urn:btih:…" spellcheck="false">${esc(a.urls)}</textarea></label>
    <label class="f">.torrent files
      <input type="file" accept=".torrent,application/x-bittorrent" multiple data-ta-files>
      ${a.files.length ? `<span class="hint">${a.files.length} chosen: ${esc(a.files.map((f) => f.name).join(', '))}</span>` : ''}</label>
    <label class="f">${rt ? 'label' : 'category'} <span class="muted">(optional)</span>
      <input data-ta="category" list="tor-cats" value="${esc(a.category)}" placeholder="${cats.length ? 'existing or new' : 'new'}">
      <datalist id="tor-cats">${cats.map((c) => `<option value="${esc(c)}">`).join('')}</datalist></label>
    <label class="row" style="align-self:end"><input type="checkbox" data-ta="stopped" ${a.stopped ? 'checked' : ''}> add stopped</label>
    <div class="actions" style="align-self:end">
      <button type="submit" class="small" data-act="tor-add:submit" ${a.busy ? 'disabled' : ''}>${a.busy ? 'adding…' : `add to ${c ? c.label + ' on ' : ''}${esc(b.name)}`}</button>
      <button type="button" class="small ghost" data-act="tor-add:toggle" data-client="${esc(c?.key || 'qbt')}">cancel</button>
    </div>
  </form>`;
}
// Per-torrent controls. Removal always confirms; removing the data as well
// names the size so the two cannot be mistaken for each other.
function torrentButtons(b, t, fleet = false) {
  if (!t.hash || S.monitorOnly) return '';
  const client = t.client || 'qbt', label = esc(CLIENT_LABEL[client] || client); // a closed set today; escaped so the attribute sink stays safe if another client is added
  const off = b.status === 'disabled' ? ' disabled title="polling is disabled for this box; enable it to run actions"'
    : !pfmView.clientActionable(b, client) ? ` disabled title="${label} is stopped on purpose on this box (~/.rtorrentDisable); this row is from before it stopped"` : '';
  const h = `data-hash="${esc(t.hash)}" data-box="${b.id}" data-rev="${Number(b.box_rev) || 0}" data-client="${esc(client)}"${off}`;
  const name = esc(t.name);
  const files = client === 'rtorrent'
    ? `Remove ${name} from rTorrent AND DELETE its ${fmtBytes(t.size, 1)} of files on ${esc(b.name)}? The box first checks that the path lies inside rTorrent's download directory and refuses otherwise, leaving the torrent in place. If it passes, the torrent is removed and then its files are deleted; should the deletion fail at that point, the torrent is already gone and files may remain. This cannot be undone.`
    : `Remove ${name} AND DELETE its ${fmtBytes(t.size, 1)} of files on ${esc(b.name)}? This cannot be undone.`;
  // Icons; the hover text names each. A disabled button's own title (in h)
  // comes first and so is the one shown; one that does not apply to the
  // torrent's state is disabled and says why. Pause is rTorrent's alone
  // (qBittorrent has none), so it is offered only in rTorrent's own table;
  // the fleet list mixes both and keeps start / stop, where start also
  // resumes a paused rTorrent torrent.
  const can = pfmView.torrentControls(client, t.kind, t.state);
  const btn = (act, ok, label, why, icon, extra = '') => `<button class="icon" data-act="tor:${act}" ${h}${ok ? '' : ' disabled'} title="${ok ? label : why}" aria-label="${label}"${extra}>${icon}</button>`;
  return `${btn('start', can.start, client === 'rtorrent' && t.state === 'paused' ? 'resume' : 'start', 'already running', ICON.start)}${can.pause === null || fleet ? '' : btn('pause', can.pause, 'pause (stays open; start resumes it)', 'not running', ICON.pause)}${btn('stop', can.stop, 'stop', 'already stopped', ICON.stop, ` data-confirm="Stop ${name} on ${esc(b.name)}?"`)}<button class="icon" data-act="tor:remove" ${h} title="remove (keeps the files)" aria-label="remove, keeping the files" data-confirm="Remove ${name} from ${label} on ${esc(b.name)}? The downloaded files stay on disk.">${ICON.remove}</button><button class="icon danger" data-act="tor:remove-data" ${h} title="remove and delete the files" aria-label="remove and delete the files" data-confirm="${files}">${ICON.trash}</button>`;
}
// Torrent row icons (16-unit grid, drawn in the button's colour).
const ICON = {
  start: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M5 3.2v9.6L12.5 8z" fill="currentColor"/></svg>',
  pause: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M5.5 3.5v9M10.5 3.5v9"/></svg>',
  stop: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M5.5 1.5h5l4 4v5l-4 4h-5l-4-4v-5z"/><path d="M5 8h6"/></svg>',
  remove: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg>',
  trash: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M2.5 4.5h11M6.2 4.5V2.5h3.6v2M4 4.5l.8 9h6.4l.8-9M6.6 7v4.5M9.4 7v4.5"/></svg>',
};
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
          ${S.monitorOnly ? '' : run ? `<button class="small" data-act="ctr:restart" data-cid="${esc(c.id)}" data-confirm="Restart ${esc(c.name)}?">restart</button><button class="small" data-act="ctr:stop" data-cid="${esc(c.id)}" data-confirm="Stop ${esc(c.name)}?">stop</button>` : `<button class="small" data-act="ctr:start" data-cid="${esc(c.id)}">start</button>`}
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
  const why = { offline: ' (box unreachable)', pending: ' yet', disabled: ' (polling is disabled)' }[b.status] || ' (ps printed nothing on the box)';
  const reliable = A.ok && A.args_known && !A.truncated; // as the server's AppsState.Reliable: no "not running" verdict from a cut list
  const note = !A.ok ? `<span class="warn">process list not read${why}</span>`
    : `${A.procs} process${A.procs === 1 ? '' : 'es'} · ${fmtBytes(A.rss, 1)} resident${A.args_known ? '' : ' · <span class="warn" title="the command-line listing was cut this poll: applications run by an interpreter (python, mono, dotnet, node, java, php) cannot be told apart, so no app alert is raised or resolved">command lines not read</span>'}${A.truncated ? ' · <span class="warn" title="the account runs more processes than the probe keeps, so this list is a prefix: an application missing from it may still be running, and no app alert is raised or resolved">list incomplete</span>' : ''}`;
  return `<div class="table app-table" style="overflow:hidden">
    <div class="thead-tools">
      <div style="font-size:12px"><b>services</b> <span class="muted">· ${note}</span></div>
      <div class="actions"><input class="filter" placeholder="filter…" data-filter value="${esc(S.filter)}"></div>
    </div>
    <div style="overflow:auto">
      <div class="trow head"><span>application</span><span>kind</span><span>state</span><span>cpu</span><span>memory</span><span>up for</span><span title="listening TCP ports held by its processes">ports</span><span>link</span><span title="raise an alert when this application is not running">watch</span></div>
      ${apps.length === 0 ? `<div class="trow"><span class="muted">${A.ok ? 'nothing matches' : '—'}</span></div>` : apps.map((a) => appRow(b, a, reliable)).join('')}
    </div>
  </div>
  <div class="hint" style="margin-top:8px">Everything the account runs on the shared host, as its own uid sees it (other tenants' processes are hidden by PMSS). CPU is the share of one core since the previous poll. Watch an application to be alerted when it is not running; a watched application that is down stays listed.</div>
  <div class="table proc-table" style="overflow:hidden;margin-top:14px">
    <div class="thead-tools"><div style="font-size:12px"><b>processes</b> <span class="muted">· largest first${A.procs > all.length ? ` · the ${all.length} using the most memory of ${A.procs}` : ''}${procs.length < all.length ? ` · showing ${procs.length} of ${all.length}` : ''}</span></div></div>
    <div style="overflow:auto">
      <div class="trow head"><span>pid</span><span>name</span><span>application</span><span>cpu</span><span>rss</span><span>up for</span><span>command</span></div>
      ${procs.length === 0 ? `<div class="trow"><span class="muted">${A.ok ? 'nothing matches' : '—'}</span></div>` : procs.map((p) => `<div class="trow"><span class="num muted">${p.pid}</span><span class="ellip" title="${esc(p.name)}">${esc(p.name)}</span><span class="ellip muted">${esc(appName(A, p.app))}</span><span class="num">${p.cpu_known ? p.cpu_pct.toFixed(1) + '%' : '—'}</span><span class="num">${fmtBytes(p.rss, 1)}</span><span class="muted">${fmtDur(p.elapsed_sec)}</span><span class="ellip muted" title="${esc(p.args || '')}">${esc(p.args || '')}</span></div>`).join('')}
    </div>
  </div>`;
}
// known is false when the process list or the command lines could not be
// read this poll: a watched app's row is then listed (the list is the
// user's) but its state is unknown, not "not running".
function appRow(b, a, known) {
  const url = appUrl(b, a);
  const state = a.running ? `running · ${a.procs} proc${a.procs === 1 ? '' : 's'}` : known ? 'not running' : 'unknown';
  return `<div class="trow">
    <div class="ellip" title="${esc(a.key)}"><span class="led ${a.running ? 'online' : known ? 'offline' : 'pending'}" style="width:6px;height:6px;margin-right:8px"></span>${esc(a.name)}</div>
    <span><span class="tag">${esc(a.category)}</span></span>
    <span class="${a.running || !known ? '' : 'bad'}">${state}</span>
    <span class="num">${a.cpu_known ? a.cpu_pct.toFixed(1) + '%' : '—'}</span>
    <span class="num">${a.running ? fmtBytes(a.rss, 1) : '—'}</span>
    <span class="muted">${a.running ? fmtDur(a.uptime_sec) : '—'}</span>
    <span class="num muted ellip">${(a.ports || []).join(', ') || '—'}</span>
    <span>${url ? `<a href="${esc(url)}" target="_blank" rel="noopener" title="${esc(url)}">open ↗</a>` : '—'}</span>
    <span><button class="small ${a.watched ? 'good' : ''}" data-act="app:watch" data-key="${esc(a.key)}" data-on="${a.watched ? '1' : ''}" title="${a.watched ? 'watched: an alert is raised when it is not running; click to stop watching' : 'alert when this application is not running'}">${a.watched ? 'watching' : 'watch'}</button></span>
  </div>`;
}
/* ----- logs: the log files found under the home, and container output, read on demand ----- */
function logDir(p) { const i = p.lastIndexOf('/'); return i < 0 ? '~' : p.slice(0, i); }
function logBase(p) { return p.slice(p.lastIndexOf('/') + 1); }
// Rotated copies (qbittorrent.log.bak12, error.log.1, rtorrent.log.1792210287, *.gz) fold under "older".
const rotated = (p) => /\.(bak\d*|\d+|\d+\.gz|gz)$/i.test(p);
function logItem(f, sel) {
  return `<button class="logitem ${sel('file', f.path)}" data-act="log:file" data-path="${esc(f.path)}" title="${esc(f.path)}"><span class="ellip">${esc(logBase(f.path))}</span><span class="muted num">${fmtBytes(f.size, 0)} · ${fmtAgo(f.mtime)}</span></button>`;
}
function logGroup(d, fs, sel) {
  const cur = fs.filter((f) => !rotated(f.path)), old = fs.filter((f) => rotated(f.path));
  const openOld = S.logOpen.has(d) || old.some((f) => sel('file', f.path));
  return `<div class="loggroup"><div class="tile-label ellip" title="~/${esc(d)}">~/${esc(d)}</div>${cur.map((f) => logItem(f, sel)).join('')}${old.length ? `<details class="adv" data-logdir="${esc(d)}"${openOld ? ' open' : ''}><summary>${old.length} older / rotated</summary><div style="gap:2px;margin-top:4px">${old.map((f) => logItem(f, sel)).join('')}</div></details>` : ''}</div>`;
}
function tabLogs(b) {
  const L = b.logs || {};
  const files = L.files || [];
  const groups = new Map();
  for (const f of files) {
    const d = logDir(f.path);
    if (!groups.has(d)) groups.set(d, []);
    groups.get(d).push(f);
  }
  const ctrs = b.docker.enabled ? (b.docker.containers || []) : [];
  const V = S.logView;
  const sel = (kind, key) => V && V.kind === kind && V.key === key ? 'on' : '';
  const list = `${[...groups].map(([d, fs]) => logGroup(d, fs, sel)).join('')}
    ${ctrs.length ? `<div class="loggroup"><div class="tile-label">containers</div>${ctrs.map((c) => `<button class="logitem ${sel('ctr', c.id)}" data-act="log:ctr" data-cid="${esc(c.id)}" data-name="${esc(c.name)}"><span class="ellip">${esc(c.name)}</span><span class="muted">${c.state === 'running' ? 'running' : esc(c.state)}</span></button>`).join('')}</div>` : ''}`;
  const empty = !L.ok ? `<div class="hint">log listing not read${b.status === 'offline' ? ' (box unreachable)' : b.status === 'pending' ? ' yet' : ''}.</div>` : files.length === 0 && ctrs.length === 0 ? `<div class="hint">no log files in the usual places (~/.logs, qBittorrent, the *arr apps, Jellyfin, SABnzbd, Deluge, rclone, FlexGet, pyLoad, Cloudplow, lighttpd) and no containers.</div>` : '';
  // The listing comes from the inventory poll (every 15 minutes), not every poll.
  const when = L.ok && L.at && !L.at.startsWith('0001') ? `<div class="hint" style="display:flex;justify-content:space-between;align-items:center">listed ${fmtAgo(L.at)}<button class="small" data-act="rescan-logs" ${b.status === 'disabled' ? 'disabled' : ''}>rescan</button></div>` : '';
  return `<div class="logs">
    <div class="panel loglist">${when}${empty}${list}</div>
    <div class="panel logpane">${logViewer(V)}</div>
  </div>`;
}
// Opening the logs tab on a listing older than ten minutes asks the box
// for a fresh one (a refresh runs the inventory sections); once per box
// per ten minutes, so flipping tabs does not queue probes.
const LOGS_STALE_MS = 10 * 60 * 1000;
const logsRescanAt = new Map();
function rescanLogsIfStale() {
  // The id is the routed one (a number or null, see route); the box it
  // names is looked up only to read the listing's age. The request is built
  // from the routed id, like every other per-box action on this page.
  const id = S.selId;
  const b = S.detail && S.detail.id === id ? S.detail : boxById(id);
  if (!id || !b || b.status === 'disabled' || !b.logs?.ok) return;
  const at = new Date(b.logs.at || 0).getTime();
  const last = logsRescanAt.get(id) || 0;
  if (Date.now() - at < LOGS_STALE_MS || Date.now() - last < LOGS_STALE_MS) return;
  logsRescanAt.set(id, Date.now());
  api('POST', `${BASE}/api/boxes/${id}/refresh`).catch(() => {});
}
function logViewer(V) {
  if (!V) return `<div class="empty" style="border:0">pick a log on the left: the last ${300} lines are fetched from the box each time, nothing is streamed or stored.</div>`;
  const head = `<div class="thead-tools" style="padding:0 0 10px;border:0"><div style="font-size:12px"><b>${esc(V.label)}</b> <span class="muted">· ${V.busy ? '<span class="pulse">reading…</span>' : V.err ? '' : `last ${V.lines || 300} lines · read ${fmtAgo(V.at)}`}</span></div><div class="actions"><input class="filter" placeholder="filter lines…" data-filter value="${esc(S.filter)}"><button class="small" data-act="log:refresh" ${V.busy ? 'disabled' : ''}>refresh</button></div></div>`;
  if (V.err) return `${head}<div class="err-box">${esc(V.err)}</div>`;
  if (V.busy && V.text == null) return `${head}<div class="hint">reading…</div>`; // no #logview yet: the scroll-to-end waits for the content
  const f = S.filter.trim().toLowerCase();
  let lines = V.text ? V.text.replace(/\n$/, '').split('\n') : []; // '' would split into one empty line and hide the placeholder
  if (f) lines = lines.filter((l) => l.toLowerCase().includes(f));
  const body = lines.length ? esc(lines.join('\n')) : `<span class="muted">${V.text ? 'nothing matches' : '(empty)'}</span>`;
  return `${head}<pre class="logview" id="logview">${body}</pre>`;
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
    <div class="panel"><div class="tile-label">host cpu</div>
      <div class="tile-value">${h.cpu_ok ? fmtPct(h.cpu_busy_pct) : '—'} <small>busy${h.ncpu ? ` · ${h.ncpu} cores` : ''}${h.cpu_ok ? ` · iowait ${fmtPct(h.cpu_iowait_pct)}${h.cpu_steal_pct >= 0.5 ? ` · steal ${fmtPct(h.cpu_steal_pct)}` : ''}` : ''}</small></div>
      <div class="meter thick split" title="busy (every tenant's cpu time, steal included) and, lighter, iowait: cpus waiting on the disks"><i class="${levelClass(h.cpu_ok ? h.cpu_busy_pct : 0, 100)}" style="width:${h.cpu_ok ? Math.min(100, h.cpu_busy_pct) : 0}%"></i><i class="dim" style="width:${h.cpu_ok ? Math.min(100 - Math.min(100, h.cpu_busy_pct), h.cpu_iowait_pct) : 0}%"></i></div>
      <div class="note">${h.cpu_ok ? `you ${pfmView.hostShare(b.cpu) === null ? '—' : fmtPctFine(b.cpu.host_pct)} of it` : 'measured from the second poll on'} · load ${h.load1?.toFixed(2) ?? '—'} / ${h.load5?.toFixed(2) ?? '—'} / ${h.load15?.toFixed(2) ?? '—'} · a busy neighbour shows up here, not in your own cpu meter</div></div>
  </div>${liveTrendPanel(b)}`;
}

/* "node vs. you": PMSS's own live-stats rings (five-minute samples, 24 h),
   summarised server-side exactly as its panel does; this only draws them.
   The share cell and the trend drawing follow PMSS's etc/skel/www/statsLive.php
   (pmssStatsLiveCell, pmssStatsLiveSvg): Copyright (C) MagnaCapax / the PMSS
   Team, GPL-3.0-only, https://github.com/MagnaCapax/PMSS. GPLv3 section 13
   permits combining that work with this AGPL program; these three functions
   remain under GPL-3.0, the rest of this file under the licence at the top. */
function liveCell(p, unit) {
  const f = (v) => unit === '%' ? fmtPctFine(v) : `${(Number(v) || 0).toFixed(v < 10 ? 1 : 0)} Mbit/s`;
  const nodeW = unit === '%' ? Math.min(100, p.node) : 100;
  const youW = p.node > 0 ? Math.min(100, p.you / p.node * 100) : 0;
  return `<td><div class="lvbar"><i style="width:${nodeW}%"><i style="width:${youW}%"></i></i></div><div class="note">node ${f(p.node)} · <span class="lvyou">you ${f(p.you)}</span></div></td>`;
}
function liveSvg(points, max) {
  if (!points || points.length < 2) return '';
  const n = points.length;
  const line = (i, cls) => `<polyline class="${cls}" points="${points.map((p, x) => `${(x * 300 / (n - 1)).toFixed(1)},${(38 - Math.min(1, p[i] / max) * 36).toFixed(1)}`).join(' ')}"/>`;
  return `<svg class="lvsvg" viewBox="0 0 300 40" preserveAspectRatio="none" role="img">${line(0, 'n')}${line(1, 'y')}</svg>`;
}
function liveTrendPanel(b) {
  const L = b.live || {};
  if (!L.ok) {
    return `<div class="panel" style="margin-top:14px"><div class="tile-label">node vs. you · 24 h</div>
      <div class="note">PMSS's live statistics are not readable on this host yet (its 2026-10 release writes them every five minutes; the first samples take about ten minutes)</div></div>`;
  }
  const metrics = [['cpu', 'CPU', '%'], ['ram', 'RAM', '%'], ['tx', 'network out', 'Mbit/s'], ['rx', 'network in', 'Mbit/s']];
  const S = L.series || {};
  return `<div class="panel" style="margin-top:14px"><div class="tile-label">node vs. you · 24 h <small class="muted">as PMSS counts it · five-minute samples · ${fmtAgo(L.at)}</small></div>
    <div class="lvwrap"><table class="lvtable"><tr><th></th>${L.windows.map((w) => `<th>${esc(w.label)}</th>`).join('')}<th>24 h trend</th></tr>
    ${metrics.map(([k, name, unit]) => `<tr><th>${name}</th>${L.windows.map((w) => w.samples ? liveCell(w[k], unit) : '<td class="muted">n/a</td>').join('')}<td>${liveSvg(S[k], unit === '%' ? 100 : Math.max(1, ...(S[k] || [[1, 0]]).map((p) => p[0])))}</td></tr>`).join('')}
    </table></div>
    <div class="note"><span class="lvnode">■ node</span> = the whole server · <span class="lvyou">■ you</span> = your account, cpu as a share of every host core; ram counts your page cache as yours but the node's reclaimable cache as free, so you can exceed the node there · you see the node's totals and your own numbers only</div></div>`;
}

/* ----- add / edit ----- */
function blankForm() {
  return { name: '', host: '', port: 22, user: '', auth_method: '', key_path: '', password: '', type: 'seedbox', plan: '', datacenter: '', renewal: '',
    qbt_enabled: true, qbt_port: 0, qbt_user: '', qbt_pass: '', qbt_start_cmd: '', rt_enabled: false, rt_scgi: '', docker_enabled: false, du_enabled: true, enabled: true, host_key: '' };
}
function resetAdd() { S.add = { mode: 'add', step: 1, form: blankForm(), errors: {}, detect: null, busy: false, editId: null, service: null }; }
async function startEdit(id) {
  try {
    S.add = { mode: 'edit', step: 1, form: null, errors: {}, detect: null, busy: false, editId: id, service: boxById(id)?.service || null };
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
        <label class="f">key path <span class="hint">(empty = ${S.sshKey ? `${esc(S.sshKey)}, then ` : ''}~/.ssh/id_*)</span><input data-f="key_path" value="${esc(f.key_path)}" placeholder="~/.ssh/id_ed25519" ${f.auth_method === 'password' || f.auth_method === 'agent' ? 'disabled' : ''}></label>
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
function addRtFields(f) {
  const det = S.add.detect?.facts?.rt_endpoint;
  return `<label class="check"><input type="checkbox" data-f="rt_enabled" ${f.rt_enabled ? 'checked' : ''}> monitor rTorrent via its SCGI socket (over SSH; the Pulsed Media client, started by the panel)</label>
        <label class="f">SCGI endpoint <span class="hint">(blank = from ~/.rtorrent.rc${det ? `; detected ${esc(det)}` : ''}; unix:/path inside the home recommended (one outside it is not used). tcp:127.0.0.1:port is unauthenticated: on a shared host every tenant can reach it)</span><input data-f="rt_scgi" value="${esc(f.rt_scgi || '')}" placeholder="unix:/home/${esc(f.user || 'user')}/.rtorrent.socket" spellcheck="false"></label>`;
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
      <details class="adv" ${edit ? 'open' : ''}><summary>torrent clients, Docker and measurement options</summary><div>
        ${addQbtFields(f, edit)}
        ${addRtFields(f)}
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
  const clients = [];
  if (pb.qbt_enabled) clients.push(`qBittorrent ${esc(F.qbt_version || '(not responding)')} on port ${F.qbt_port}`);
  if (pb.rt_enabled) clients.push(`rTorrent ${esc(F.rt_version || '(not responding)')}${F.rt_endpoint ? ` on ${esc(F.rt_endpoint)}` : ''}`);
  return `<div><span>client${clients.length > 1 ? 's' : ''}</span><span>${clients.join(' · ') || 'none'}</span></div>
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
  p.host = p.host.trim(); p.user = p.user.trim(); p.rt_scgi = (p.rt_scgi || '').trim();
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
      A.form.rt_enabled = b.rt_enabled;
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
      try {
        await api('PUT', `${BASE}/api/boxes/${A.editId}`, payload(A.form));
      } catch (e) {
        if (e.status === 409) {
          // Another tab or a script changed the box since this form was
          // loaded: nothing was written. Reload the record so the next
          // save is based on it.
          toast('this box was changed elsewhere since the form was loaded; nothing was saved — the form has been reloaded, apply your change again', 'warn');
          await startEdit(A.editId);
          return;
        }
        throw e;
      }
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
    <div class="panel"><div class="tile-label">pmdb.fyi hardware catalogue</div>
      <label class="check"><input type="checkbox" data-s="pmdb_enabled" ${s.pmdb_enabled ? 'checked' : ''}> submit the hosts my boxes run on to the community hardware catalogue when they are missing from it</label>
      <label class="row">upload key<input data-s="pmdb_upload_key" type="password" autocomplete="off" value="${esc(s.pmdb_upload_key || '')}" placeholder="${s.clear_pmdb_upload_key ? 'will be removed on save' : S.settings.has_pmdb_upload_key ? '•••••••• (stored)' : 'paste key'}"></label>
      <div class="hint"><a href="https://pmdb.fyi" target="_blank" rel="noopener">pmdb.fyi ↗</a> is a community-run, public table of Pulsed Media host hardware (<a href="https://github.com/schmintendo/pmdb" target="_blank" rel="noopener">source ↗</a>); the upload key is a shared passphrase handed out by its maintainers. <b>What leaves this dashboard when enabled:</b> a request for the public list, and for each host not in it, one submission with the key carrying the host's hostname, CPU model, core count, nominal clock, RAM, swap, home filesystem size and whether an Intel GPU is present. Nothing about your account, username, usage or torrents is sent, and an existing row is never replaced; as with any web request, the service sees your IP address and the client name (pmss-fleet-manager). Checked after each box's quarter-hourly inventory and every six hours. Stored in the local database; leave the key blank to keep the current one, and removing it switches submission off.${S.settings.has_pmdb_upload_key && !s.clear_pmdb_upload_key ? ` <button class="small ghost" data-act="settings:clear-pmdb-key">remove key</button>` : ''}</div>
      ${viewPMDB()}
    </div>
    <div class="form-foot" style="justify-content:flex-start"><button class="primary" data-act="settings:save">save settings</button></div>
    <div class="panel"><div class="tile-label">about &amp; licensing</div>
      <div class="row"><span>version</span><span>${esc(S.version || 'dev')}${S.monitorOnly ? ' · <span class="muted" title="started with -monitor-only: torrent, client, discovery and container actions are refused; monitoring runs as usual">monitor-only</span>' : ''}</span></div>
      <div class="hint">pmss-fleet-manager is free software under the GNU Affero General Public License v3 or later. Because you are using it over a network, you are entitled to its source code.</div>
      <div class="row" style="justify-content:flex-start;gap:16px"><a href="${esc(BASE)}/license" target="_blank" rel="noopener">license ↗</a><a href="${esc(BASE)}/third-party-licenses" target="_blank" rel="noopener">third-party notices ↗</a><a href="${esc(S.sourceUrl || BASE + '/source')}" target="_blank" rel="noopener">source code ↗</a></div>
    </div>
  </div>
  ${viewServices()}`;
}
// viewPMDB lists each distinct host and its standing with the catalogue,
// below the settings that enable the integration.
function viewPMDB() {
  const v = S.pmdb;
  if (!v?.enabled) return '';
  const label = { listed: 'in the catalogue', submitted: 'submitted', pending: 'not listed, submission pending', error: 'submission failed' };
  const rows = v.hosts.map((h) => `<div class="row"><span>${esc(h.hostname)}${h.box_ids.length > 1 ? ` <span class="muted">· ${h.box_ids.length} boxes</span>` : ''}</span><span class="${h.status === 'error' ? 'bad' : 'muted'}" title="${esc(h.detail || '')}">${label[h.status] || esc(h.status)}${h.detail ? `: ${esc(h.detail)}` : ''}</span></div>`).join('');
  const status = v.error ? `<span class="bad">${esc(v.error)}</span>` : v.checked_at ? `checked ${fmtAgo(v.checked_at)}` : v.configured ? 'not checked yet' : 'save an upload key to start';
  return `<div class="row" style="margin-top:8px"><span class="muted">${status}</span>${v.configured ? '<button class="small ghost" data-act="settings:pmdb-sync">check now</button>' : ''}</div>${rows || (v.configured ? '<div class="hint">No host has reported its inventory yet.</div>' : '')}`;
}
function viewServices() {
  const v = S.services;
  if (!v?.configured) return '';
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
    api('PUT', BASE + '/api/settings', { card_min_px: S.settings.card_min_px }).then((d) => { stateSeq++; S.settings = d; }).catch((err) => toast(err.message, 'bad'));
    return;
  }
  const fv = e.target.closest('[data-fleetview]');
  if (fv) { S.fleetView = fv.dataset.fleetview; S.fleetViewTouched = true; render(); return; }
  const tab = e.target.closest('[data-tab]');
  if (tab) {
    S.tab = tab.dataset.tab; S.filter = ''; S.torAdd = null; S.logView = null; S.logOpen = new Set();
    history.replaceState(null, '', `#/box/${S.selId}/${S.tab}`);
    if (S.tab === 'logs') rescanLogsIfStale();
    render();
    return;
  }
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
  else if (a === 'rescan-logs') { await api('POST', `${BASE}/api/boxes/${S.selId}/refresh`); toast('rescanning logs with the next poll'); }
  else if (a === 'du') { const id = S.selId; await api('POST', `${BASE}/api/boxes/${id}/du`); toast('measuring directories at low priority…'); if (S.detail && S.detail.id === id) { S.detail.dirs.running = true; render(); } }
  else if (a === 'remove') { S.removingId = S.selId; await api('DELETE', `${BASE}/api/boxes/${S.selId}`); toast('box removed'); await loadState(); go('/fleet'); }
}
// ?rev= for a box-changing request: the box record revision the page shows,
// so an action drawn before an edit is refused (409) rather than run on the
// edited box.
function revQ(id) {
  const b = S.detail && S.detail.id === id ? S.detail : S.boxes.find((x) => x.id === id);
  return `?rev=${b ? Number(b.box_rev) || 0 : ''}`;
}
async function qbtAction(a, act) {
  const what = a.slice(4);
  act.disabled = true;
  if (what === 'install') toast('downloading qbittorrent-nox on the box…');
  const r = await api('POST', `${BASE}/api/boxes/${S.selId}/qbt/${what}${revQ(S.selId)}`);
  toast(`qBittorrent ${{ pause: 'stop all', resume: 'start all' }[what] || what}: ok${r?.output ? ' — ' + r.output.split('\n').slice(0, 2).join(' · ') : ''}`, 'ok');
}
async function torrentAction(a, act) {
  const what = a.slice(4);
  act.disabled = true;
  const seg = act.dataset.client === 'rtorrent' ? 'rtorrent' : 'qbt';
  await api('POST', `${BASE}/api/boxes/${act.dataset.box || S.selId}/${seg}/torrents/${encodeURIComponent(act.dataset.hash)}/${what}?rev=${encodeURIComponent(act.dataset.rev || '')}`);
  toast(`torrent ${{ stop: 'stopped', start: 'started', pause: 'paused', remove: 'removed', 'remove-data': 'removed with its files' }[what]}`, 'ok');
  if (S.view === 'torrents') void loadTorrents();
}
async function discoveryAction(a, act) {
  const k = a.slice(5), on = !act.dataset.on, rt = act.dataset.client === 'rtorrent';
  const label = { dht: 'DHT', pex: 'PeX', lsd: 'LSD' }[k] || esc(k);
  act.disabled = true;
  // rTorrent: saved to ~/.rtorrent.rc.custom, then applied live; the
  // answer says when the running client could not follow (restart).
  const r = await api('POST', `${BASE}/api/boxes/${S.selId}/${rt ? 'rtorrent' : 'qbt'}/discovery${revQ(S.selId)}`, { [k]: on });
  if (rt) { const m = pfmView.rtDiscoveryToast(k, on, r); toast(m.text, m.kind); }
  else toast(`${label} ${on ? 'enabled' : 'disabled'}`, 'ok');
  await loadDetail(S.selId);
}
async function torAddAction(a, act) {
  if (a === 'tor-add:toggle') {
    // One draft at a time; the button says which client it is for, and a
    // click on another client's button moves the (empty) form there.
    const client = act?.dataset.client || 'qbt';
    S.torAdd = S.torAdd && (S.torAdd.client || 'qbt') === client ? null : { urls: '', category: '', stopped: false, files: [], busy: false, client };
    render(); return;
  }
  const d = S.torAdd;
  if (!d || d.busy) return;
  const fd = new FormData();
  fd.append('urls', d.urls);
  fd.append('category', d.category);
  fd.append('stopped', String(d.stopped));
  d.files.forEach((f) => fd.append('torrents', f, f.name));
  d.busy = true; render();
  try {
    const seg = d.client === 'rtorrent' ? 'rtorrent' : 'qbt';
    const r = await api('POST', `${BASE}/api/boxes/${S.selId}/${seg}/torrents${revQ(S.selId)}`, fd);
    toast(addToast(r, d.stopped, d.client), r?.failed || r?.unlabelled || r?.stopped_early ? 'warn' : 'ok');
    if (S.torAdd === d) S.torAdd = null; // not a draft the user opened meanwhile
  } finally {
    if (S.torAdd) S.torAdd.busy = false;
    render();
  }
}
async function logAction(a, act) {
  let V = S.logView;
  if (a === 'log:file') V = { kind: 'file', key: act.dataset.path, label: act.dataset.path };
  else if (a === 'log:ctr') V = { kind: 'ctr', key: act.dataset.cid, label: `container ${act.dataset.name || act.dataset.cid.slice(0, 12)}` };
  else if (a !== 'log:refresh' || !V) return;
  V = { ...V, busy: true, err: '', text: null, scrolled: false }; // a (re)read opens at its end again, like tail
  S.logView = V; S.filter = ''; render();
  try {
    const r = V.kind === 'file'
      ? await api('POST', `${BASE}/api/boxes/${S.selId}/logs/read`, { path: V.key })
      : await api('POST', `${BASE}/api/boxes/${S.selId}/containers/${encodeURIComponent(V.key)}/logs`);
    if (S.logView !== V) return; // another log was picked meanwhile
    S.logView = { ...V, busy: false, text: r.text || '', lines: r.lines, at: new Date().toISOString() };
  } catch (e) {
    if (S.logView !== V) return;
    S.logView = { ...V, busy: false, err: e.message };
  }
  render();
}
async function appWatchAction(act) {
  const on = !act.dataset.on;
  act.disabled = true;
  await api('POST', `${BASE}/api/boxes/${S.selId}/apps/watch`, { key: act.dataset.key, on });
  toast(on ? 'watching: an alert is raised when it is not running' : 'no longer watched', 'ok');
  await loadDetail(S.selId);
}
async function containerAction(a, act) {
  act.disabled = true; await api('POST', `${BASE}/api/boxes/${S.selId}/containers/${act.dataset.cid}/${a.slice(4)}${revQ(S.selId)}`); toast(`container ${a.slice(4)} ok`, 'ok');
}
async function alertsAction(a, act) {
  if (a === 'ack') { act.disabled = true; await api('POST', `${BASE}/api/alerts/${act.dataset.id}/ack`); toast('acknowledged', 'ok'); await loadAlerts(); }
  else if (a === 'ack-all') { await api('POST', BASE + '/api/alerts/ack-all'); toast('all alerts acknowledged', 'ok'); await loadAlerts(); }
  else if (a === 'alert:dismiss') { act.disabled = true; await api('DELETE', `${BASE}/api/alerts/${act.dataset.id}`); await loadAlerts(); }
  else if (a === 'alerts:clear-resolved') { await api('POST', BASE + '/api/alerts/clear-resolved'); toast('resolved alerts cleared', 'ok'); await loadAlerts(); }
}
async function settingsAction(a) {
  if (a === 'settings:save') {
    // Only the fields touched in this tab go up (plus the API-key
    // operation): the server merges them onto its current settings, so a
    // save here cannot revert a change another tab made meanwhile, even one
    // whose settings event has not reached this tab yet.
    const d = settingsPatch(S.settingsDraft, S.settingsTouched);
    const keyChanged = !!(d.pm_api_key || d.clear_pm_api_key);
    const pmdbChanged = 'pmdb_enabled' in d || !!(d.pmdb_upload_key || d.clear_pmdb_upload_key);
    const saved = await api('PUT', BASE + '/api/settings', d);
    stateSeq++; // a state load started before the save must not put the old values back
    S.settings = saved; S.settingsDraft = settingsDraftOf(S.settings); S.settingsTouched = {}; applyTheme(S.settings.theme); S.fleetViewTouched = false; S.fleetView = S.settings.fleet_view; toast('settings saved', 'ok'); render();
    if (keyChanged) { S.services = null; servicesSeq++; setTimeout(loadServices, 1500); }
    if (pmdbChanged) void loadPMDB(); // the first check runs a few seconds later and announces itself with a pmdb event
  }
  else if (a === 'settings:clear-key') { S.settingsDraft.clear_pm_api_key = true; S.settingsDraft.pm_api_key = ''; S.settingsTouched.clear_pm_api_key = true; render(); }
  else if (a === 'settings:clear-pmdb-key') { S.settingsDraft.clear_pmdb_upload_key = true; S.settingsDraft.pmdb_upload_key = ''; S.settingsTouched.clear_pmdb_upload_key = true; render(); }
  else if (a === 'settings:pmdb-sync') { toast('checking pmdb.fyi…'); const v = await api('POST', BASE + '/api/pmdb/sync'); pmdbSeq++; S.pmdb = v; toast(v.error ? v.error : `checked ${v.hosts.length} hosts`, v.error ? 'bad' : 'ok'); render(); }
}
// settingsPatch is the settings save body: the touched fields, numbers as
// numbers, the API key only when typed and the clear flag only when set
// (typing a key after "remove" cancels the removal).
function settingsPatch(draft, touched) {
  const d = {};
  for (const k of Object.keys(touched)) if (k in draft) d[k] = draft[k];
  for (const k of ['poll_interval_sec', 'disk_threshold_pct', 'traffic_threshold_pct', 'renewal_warn_days', 'du_interval_hours', 'card_min_px']) if (k in d) d[k] = Number(d[k]);
  if (draft.pm_api_key) { d.pm_api_key = draft.pm_api_key; delete d.clear_pm_api_key; }
  else { delete d.pm_api_key; if (draft.clear_pm_api_key) d.clear_pm_api_key = true; else delete d.clear_pm_api_key; }
  if (draft.pmdb_upload_key) { d.pmdb_upload_key = draft.pmdb_upload_key; delete d.clear_pmdb_upload_key; }
  else { delete d.pmdb_upload_key; if (draft.clear_pmdb_upload_key) d.clear_pmdb_upload_key = true; else delete d.clear_pmdb_upload_key; }
  if ('pmdb_enabled' in d) d.pmdb_enabled = !!d.pmdb_enabled;
  return d;
}
async function servicesAction(a, act) {
  if (a === 'svc:sync') { act.disabled = true; toast('syncing services from pulsedmedia.com…'); const synced = await api('POST', BASE + '/api/services/sync'); servicesSeq++; S.services = synced; await loadState(); toast(`synced ${S.services.services.length} services`, 'ok'); render(); }
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
  if (a === 'refresh-all' || a === 'refresh' || a === 'rescan-logs' || a === 'du' || a === 'remove') await boxAction(a);
  else if (a.startsWith('qbt:')) await qbtAction(a, act);
  else if (a.startsWith('tor:')) await torrentAction(a, act);
  else if (a === 'rt:dismiss-failure') { act.disabled = true; await api('POST', `${BASE}/api/boxes/${S.selId}/rtorrent/remove-failure/dismiss`, { at: act.dataset.at }); toast('notice dismissed', 'ok'); await loadDetail(S.selId); }
  else if (a.startsWith('tor-add:')) await torAddAction(a, act);
  else if (a.startsWith('disc:')) await discoveryAction(a, act);
  else if (a === 'sort:tor') { const tb = act.dataset.table; S.torSort[tb] = pfmView.nextSort(S.torSort[tb], act.dataset.key); saveTorSort(); render(); }
  else if (a.startsWith('ctr:')) await containerAction(a, act);
  else if (a === 'app:watch') await appWatchAction(act);
  else if (a.startsWith('log:')) await logAction(a, act);
  else if (a === 'ack' || a === 'ack-all' || a === 'alert:dismiss' || a === 'alerts:clear-resolved') await alertsAction(a, act);
  else if (a.startsWith('settings:')) await settingsAction(a);
  else if (a.startsWith('svc:')) await servicesAction(a, act);
  else if (a.startsWith('add:') || a === 'edit:save') await wizardAction(a);
}
document.addEventListener('submit', (e) => {
  if (e.target.closest('[data-tor-add]')) e.preventDefault(); // the submit button's click runs tor-add:submit
});
document.addEventListener('toggle', (e) => {
  const d = e.target.closest?.('details[data-logdir]');
  if (d) { if (d.open) S.logOpen.add(d.dataset.logdir); else S.logOpen.delete(d.dataset.logdir); }
}, true);
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
  if (s && S.settingsDraft) {
    S.settingsDraft[s.dataset.s] = s.type === 'checkbox' ? s.checked : s.value; S.settingsTouched[s.dataset.s] = true;
    if (s.dataset.s === 'pm_api_key' && s.value) S.settingsDraft.clear_pm_api_key = false; // a key typed after "remove" replaces instead
    if (s.dataset.s === 'pmdb_upload_key' && s.value) S.settingsDraft.clear_pmdb_upload_key = false;
    return;
  }
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
