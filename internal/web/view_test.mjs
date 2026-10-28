// Node test (node --test, run by `make jscheck`) for the pure rendering
// helpers in static/view.js: the overview's host-in-grey, you-in-blue bars.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const src = readFileSync(new URL('./static/view.js', import.meta.url), 'utf8');
vm.runInThisContext(src); // defines globalThis.pfmView, as the page's script tag does
const V = globalThis.pfmView;

const width = (html, nth = 0) => Number(html.match(/width:([\d.]+)%/g)[nth].match(/[\d.]+/)[0]);

test('an unknown host share is null, never 0 (fred review on #110)', () => {
  // A valid tenant sample with nonzero quota-relative usage but no host core
  // count: host_pct is then a zero-initialised field, not a measurement.
  assert.equal(V.hostShare({ ok: true, pct: 37, host_pct: 0, quota_cpus: 2, host_cpus: 0 }), null);
  assert.equal(V.hostShare({ ok: false, host_cpus: 8, host_pct: 0 }), null, 'no tenant sample');
  assert.equal(V.hostShare(undefined), null);
  assert.equal(V.hostShare({ ok: true, host_cpus: 8, host_pct: 0.52 }), 0.52);
});

test('ram share of the host needs both a cgroup sample and a host reading', () => {
  assert.equal(V.ramHostShare({ ok: true, rss_bytes: 15, used_bytes: 40 }, { mem_total: 100, mem_avail: 40 }), 15);
  assert.equal(V.ramHostShare({ ok: true, rss_bytes: 0, used_bytes: 30 }, { mem_total: 100 }), 30, 'charged memory when rss is unknown');
  assert.equal(V.ramHostShare({ ok: false, rss_bytes: 0, used_bytes: 0 }, { mem_total: 100 }), null, 'zeros without a sample are not 0%');
  assert.equal(V.ramHostShare({ ok: true, rss_bytes: 15 }, { mem_total: 0 }), null);
  assert.equal(V.ramHostShare({ ok: true, rss_bytes: 15 }, undefined), null);
});

test('the cpu meter is usage against the quota, empty and dashed when unknown', () => {
  const ok = V.cpuQuotaMeter({ ok: true, pct: 37, quota_cpus: 2 });
  assert.equal(width(ok), 37);
  assert.match(ok, /title="37% of your 2\.0-core quota"/);
  assert.match(ok, /class="meter thick"/);
  assert.match(V.cpuQuotaMeter({ ok: true, pct: 12, quota_cpus: 0 }), /12% of your host cores/);
  const unknown = V.cpuQuotaMeter({ ok: false, pct: 0, quota_cpus: 2 });
  assert.equal(width(unknown), 0);
  assert.match(unknown, /title="— of your 2\.0-core quota"/);
  assert.equal(width(V.cpuQuotaMeter({ ok: true, pct: 250 })), 100, 'capped at the bar');
});

test('the ram meter is usage against the limit and keeps the near-limit colour', () => {
  const bad = V.ramLimitMeter({ ok: true, pct: 96 }, 'bad');
  assert.equal(width(bad), 96);
  assert.match(bad, /class="meter" title="96% of your memory limit"><i class="bad"/);
  const unknown = V.ramLimitMeter({ ok: false, pct: 0 }, '');
  assert.equal(width(unknown), 0);
  assert.match(unknown, /— of your memory limit/);
  assert.doesNotMatch(unknown, /0% of your/);
});

test('pctFine keeps a decimal under ten percent', () => {
  assert.equal(V.pctFine(0.52), '0.5%');
  assert.equal(V.pctFine(12.4), '12%');
  assert.equal(V.pctFine(undefined), '0.0%');
});

test('fleet torrent totals count every monitored client, from a snapshot or an SSE state (fred review on #124)', () => {
  const both = { qbt: { enabled: true, total: 55, seeding: 50 }, rtorrent: { enabled: true, total: 2, seeding: 1 } };
  assert.deepEqual(V.clientTotals(both), { torrents: 57, seeding: 51, clients: 2 });
  // rTorrent only: not zero.
  assert.deepEqual(V.clientTotals({ qbt: { enabled: false, total: 0 }, rtorrent: { enabled: true, total: 2, seeding: 2 } }), { torrents: 2, seeding: 2, clients: 1 });
  // An older event without the rtorrent block, or without qbt, still counts what it has.
  assert.deepEqual(V.clientTotals({ qbt: { enabled: true, total: 3, seeding: 3 } }), { torrents: 3, seeding: 3, clients: 1 });
  assert.deepEqual(V.clientTotals({ rtorrent: { enabled: true, total: 4, seeding: 0 } }), { torrents: 4, seeding: 0, clients: 1 });
  // A monitored client that is down keeps its carried counts; a client switched off contributes nothing; junk is 0.
  assert.deepEqual(V.clientTotals({ qbt: { enabled: true, reachable: false, total: 7, seeding: 1 }, rtorrent: { enabled: false, total: 9 } }), { torrents: 7, seeding: 1, clients: 1 });
  assert.deepEqual(V.clientTotals({ qbt: { enabled: true, total: 'x' } }), { torrents: 0, seeding: 0, clients: 1 });
  assert.deepEqual(V.clientTotals(undefined), { torrents: 0, seeding: 0, clients: 0 });
});

test('the row controls are client-aware: rTorrent error rows are running (daybreak blue review of #124)', () => {
  assert.deepEqual(V.torrentControls('qbt', 'paused'), { start: true, pause: null, stop: false });
  assert.deepEqual(V.torrentControls('qbt', 'error'), { start: true, pause: null, stop: false }, 'qBittorrent error/missingFiles are stopped torrents');
  assert.deepEqual(V.torrentControls('qbt', 'seeding'), { start: false, pause: null, stop: true });
  assert.deepEqual(V.torrentControls('qbt', 'checking'), { start: false, pause: null, stop: true });
  assert.deepEqual(V.torrentControls(undefined, 'error'), { start: true, pause: null, stop: false }, 'rows without a client are qBittorrent\'s');
  // rTorrent: paused (started, inactive) resumes or stops; stopped/closed only starts.
  assert.deepEqual(V.torrentControls('rtorrent', 'paused', 'paused'), { start: true, pause: false, stop: true });
  assert.deepEqual(V.torrentControls('rtorrent', 'paused', 'stopped'), { start: true, pause: false, stop: false });
  assert.deepEqual(V.torrentControls('rtorrent', 'paused', 'closed'), { start: true, pause: false, stop: false });
  assert.deepEqual(V.torrentControls('rtorrent', 'error', 'downloading'), { start: false, pause: true, stop: true }, 'an rTorrent error row is active with a tracker message');
  assert.deepEqual(V.torrentControls('rtorrent', 'seeding', 'seeding'), { start: false, pause: true, stop: true });
  assert.deepEqual(V.torrentControls('rtorrent', 'checking', 'checking'), { start: false, pause: true, stop: true });
});

test('every documented rate source has a label', () => {
  assert.equal(V.rateSourceLabel('qbt'), 'live from qBittorrent');
  assert.equal(V.rateSourceLabel('rtorrent'), 'live from rTorrent');
  assert.equal(V.rateSourceLabel('clients'), 'live from qBittorrent + rTorrent');
  assert.match(V.rateSourceLabel('pmss-15min'), /15-min/);
  assert.equal(V.rateSourceLabel(''), 'no rate source');
  assert.equal(V.rateSourceLabel(undefined), 'no rate source');
});

test('a client stopped on purpose is neither down nor a place to add torrents', () => {
  assert.deepEqual(V.clientAvailability({ enabled: true, reachable: false, disabled: true }), { down: false, stopped: true });
  assert.deepEqual(V.clientAvailability({ enabled: true, reachable: false, disabled: false }), { down: true, stopped: false });
  assert.deepEqual(V.clientAvailability({ enabled: true, reachable: true, disabled: true }), { down: false, stopped: false }, 'answering wins over a stale marker');
  assert.deepEqual(V.clientAvailability({ enabled: false, reachable: false, disabled: true }), { down: false, stopped: false }, 'not monitored: nothing to say');
  assert.deepEqual(V.clientAvailability(undefined), { down: false, stopped: false });
});

test('the confirmation and action markup of a row carry the client (snapshot of the pure parts)', () => {
  // The row renderer lives in app.js with the DOM; its client-specific
  // decisions are the helpers above, pinned here, plus the data-client
  // attribute the action handler routes on, checked in the source itself.
  const app = readFileSync(new URL('./static/app.js', import.meta.url), 'utf8');
  assert.match(app, /data-client="\$\{esc\(client\)\}"/, 'row buttons carry data-client');
  assert.match(app, /pfmView\.torrentControls\(client, t\.kind, t\.state\)/, 'the start/pause/stop choice goes through the helper');
  assert.match(app, /refuses otherwise, leaving the torrent in place/, 'the rTorrent remove-with-data confirmation states the real ordering');
  assert.match(app, /\/\$\{seg\}\/torrents\//, 'actions are routed per client');
});

test('an intentionally stopped rTorrent stays visible with monitoring off (Codex on #124)', () => {
  const app = readFileSync(new URL('./static/app.js', import.meta.url), 'utf8');
  assert.match(app, /pfmView\.torrentsTab\(b\)\) tabs\.push\(\['torrents'/, 'the torrents tab visibility is the pure helper');
  assert.equal(V.torrentsTab({ qbt: {}, rtorrent: { enabled: false, disabled: true } }), true, 'the torrents tab shows for the marker alone');
  assert.match(app, /stopped on purpose · monitoring off/, 'the overview names the marker when monitoring is off');
});

test('the rTorrent header names the folder "+ files" may delete under, and refusals reach the user as the server said them', () => {
  const app = readFileSync(new URL('./static/app.js', import.meta.url), 'utf8');
  assert.match(app, /files: \$\{esc\(r\.data_dir\)\}/, 'the fenced folder is shown in the rTorrent section header');
  assert.match(app, /plain remove always works/, 'the note says what to do instead');
  assert.match(app, /throw Object\.assign\(new Error\(\(data\?\.error/, 'an action error carries the server message, which the toast shows');
});

test('a client stopped on purpose offers no live controls, and an open add draft for it is dropped (daybreak blue re-review of #124)', () => {
  const stopped = { qbt: { enabled: true, reachable: true }, rtorrent: { enabled: true, reachable: false, disabled: true } };
  assert.equal(V.clientActionable(stopped, 'rtorrent'), false, 'rTorrent rows carried from before the stop have no live buttons');
  assert.equal(V.clientActionable(stopped, 'qbt'), true, 'the other client is unaffected');
  assert.equal(V.clientActionable({ rtorrent: { enabled: false, disabled: true } }, 'rtorrent'), false, 'unmonitored: nothing to act on');
  assert.equal(V.clientActionable({ rtorrent: { enabled: true, reachable: false, disabled: false } }, 'rtorrent'), true, 'merely down is still actionable (the request will say so)');
  assert.equal(V.clientActionable(undefined, 'qbt'), false);
  const app = readFileSync(new URL('./static/app.js', import.meta.url), 'utf8');
  assert.match(app, /if \(avail\.stopped && S\.torAdd && \(S\.torAdd\.client \|\| 'qbt'\) === c\.key && !S\.torAdd\.busy\) S\.torAdd = null/, 'an open draft is dropped when its client stops on purpose');
  assert.match(app, /!pfmView\.clientActionable\(b, client\) \? ` disabled title=/, 'row buttons are disabled with a reason for a stopped client');
});

test('a removal notice keeps the rTorrent section and the torrents tab reachable with monitoring off (daybreak blue re-review of #124)', () => {
  const off = { qbt: { enabled: false }, rtorrent: { enabled: false, process_up: false, disabled: false, remove_failed: 'Alpha (43519d14…): disk says no' } };
  assert.equal(V.showClient(off, 'rtorrent'), true, 'the notice must stay visible and dismissable');
  assert.equal(V.showClient(off, 'qbt'), false);
  assert.equal(V.torrentsTab(off), true, 'no client monitored, yet the tab must exist for the notice');
  const quiet = { qbt: { enabled: false }, rtorrent: { enabled: false, process_up: false, disabled: false, remove_failed: '' } };
  assert.equal(V.showClient(quiet, 'rtorrent'), false);
  assert.equal(V.torrentsTab(quiet), false);
  assert.equal(V.torrentsTab({ qbt: { enabled: false, installed: true }, rtorrent: {} }), true, 'an installed client gets the tab');
  assert.equal(V.torrentsTab({ qbt: {}, rtorrent: { disabled: true } }), true, 'stopped on purpose is shown there');
  assert.equal(V.showClient({ qbt: { enabled: true }, rtorrent: { enabled: true, remove_failed: 'x' } }, 'qbt'), true);
  assert.equal(V.showClient(undefined, 'rtorrent'), false);
  // With monitoring off the row controls stay off even while the notice shows.
  assert.equal(V.clientActionable(off, 'rtorrent'), false);
});

test('the add toast says what happened: added, refused, label unconfirmed; nothing is promised about labels (daybreak blue re-review of #124)', () => {
  assert.equal(V.addSummary({ added: 2, failed: 0, unlabelled: 0 }, false, 'rtorrent'), '2 added to rTorrent');
  assert.equal(V.addSummary({ added: 2, failed: 0, unlabelled: 1 }, false, 'rtorrent'), '2 added to rTorrent; 1 loaded, label not applied or confirmed');
  assert.equal(V.addSummary({ added: 1, failed: 2, unlabelled: 1 }, true, 'rtorrent'), '1 added to rTorrent (stopped); 2 not added (refused by rTorrent or not a torrent); 1 loaded, label not applied or confirmed');
  assert.equal(V.addSummary({ added: 0, failed: 1 }, false, 'rtorrent'), '0 added to rTorrent; 1 not added (refused by rTorrent or not a torrent)');
  assert.equal(V.addSummary({ added: 2, uncertain: 1, stopped_early: 'no answer in time' }, false, 'rtorrent'), '2 added to rTorrent; then the batch stopped (no answer in time); the entry being sent may have been accepted, so check rTorrent before retrying', 'a lost load reply is reported as uncertain, never as not sent');
  assert.doesNotMatch(V.addSummary({ added: 1, uncertain: 1, stopped_early: 'x' }, false, 'rtorrent'), /not sent/);
  assert.equal(V.addSummary({ added: 2, uncertain: 0, stopped_early: 'connection lost' }, false, 'rtorrent'), '2 added to rTorrent; then the batch stopped (connection lost); the next entry was not sent and the rest were not tried', 'a failure before sending is not called uncertain');
  assert.equal(V.addSummary({ added: 0, uncertain: 1, stopped_early: 'no answer in time' }, false, 'rtorrent'), '0 added to rTorrent; then the batch stopped (no answer in time); the entry being sent may have been accepted, so check rTorrent before retrying', 'a lost reply on the first load is uncertain too');
  assert.doesNotMatch(V.addSummary({ added: 1, unlabelled: 1, deferred: 1 }, false, 'rtorrent'), /metadata|deferred|applied when/, 'no deferred promise survives the backend simplification');
  assert.equal(V.addSummary({ links: 2, files: 1 }, false, 'qbt'), '3 torrents sent to qBittorrent');
  assert.equal(V.addSummary({ counted: true, added: 1, pending: 1, failed: 1 }, true, 'qbt'), '1 added (stopped), 1 still being fetched, 1 not added (already present or not a torrent)');
});

test('torrent columns sort by their value, reverse, then return to the default order', () => {
  const rows = [
    { name: 'b', size: 10, ratio: 1.5, up_bps: 0 },
    { name: 'A', size: 30, ratio: -1, up_bps: 5 },
    { name: 'c', size: 10, ratio: 0.2, up_bps: 9 },
  ];
  const names = (l) => l.map((t) => t.name).join('');
  assert.equal(names(V.sortTorrents(rows, null)), 'bAc', 'no sort keeps the given order');
  assert.equal(names(V.sortTorrents(rows, { key: 'name', dir: 'asc' })), 'Abc', 'text ignores case');
  assert.equal(names(V.sortTorrents(rows, { key: 'size', dir: 'desc' })), 'Abc', 'ties fall back to the name');
  assert.equal(names(V.sortTorrents(rows, { key: 'ratio', dir: 'desc' })), 'Abc', 'an infinite ratio is the largest');
  assert.equal(names(V.sortTorrents(rows, { key: 'up', dir: 'asc' })), 'bAc');
  assert.equal(names(rows), 'bAc', 'the input is not reordered');
  // The fleet list sorts [box, torrent] pairs, by the box name too.
  const pairs = [[{ name: 'pm2' }, rows[0]], [{ name: 'pm1' }, rows[1]]];
  assert.equal(V.sortTorrents(pairs, { key: 'box', dir: 'asc' }, (r) => r[1], (r) => r[0].name)[0][0].name, 'pm1');
  // Clicks: new column (text up, numbers down), reverse, back to default.
  assert.deepEqual(V.nextSort(null, 'name'), { key: 'name', dir: 'asc' });
  assert.deepEqual(V.nextSort(null, 'size'), { key: 'size', dir: 'desc' });
  assert.deepEqual(V.nextSort({ key: 'size', dir: 'desc' }, 'size'), { key: 'size', dir: 'asc' });
  assert.equal(V.nextSort({ key: 'size', dir: 'asc' }, 'size'), null);
  assert.deepEqual(V.nextSort({ key: 'size', dir: 'asc' }, 'name'), { key: 'name', dir: 'asc' });
});

test('rTorrent discovery toggles show the configured value and flag a pending restart', () => {
  assert.deepEqual(V.rtDiscovery({ reachable: false }), [], 'nothing to toggle while unreachable');
  // pm1 after a change it could not apply live: file says on, client off.
  const [dht, pex] = V.rtDiscovery({ reachable: true, dht: false, conf_dht: 'on', pex: false, pex_known: true, conf_pex: 'off' });
  assert.deepEqual(dht, { k: 'dht', label: 'DHT', on: true, live: false, pending: true, warn: true });
  assert.deepEqual(pex, { k: 'pex', label: 'PeX', on: false, live: false, pending: false, warn: false });
  // No config line: the live value; DHT running is orange even when the file says off.
  const [d2] = V.rtDiscovery({ reachable: true, dht: true });
  assert.equal(d2.on, true);
  assert.equal(V.rtDiscovery({ reachable: true, dht: true, conf_dht: 'off' })[0].warn, true);
  // A build without protocol.pex and no config line: no PeX toggle.
  assert.equal(V.rtDiscovery({ reachable: true, dht: false, pex_known: false }).length, 1);
});

test('rTorrent PeX is worded as the default for new torrents, DHT as client-wide (fred on PR 126)', () => {
  const [dht, pex] = V.rtDiscovery({ reachable: true, dht: true, pex: true, pex_known: true, conf_dht: 'on', conf_pex: 'on' });
  assert.match(V.rtDiscoveryHint(pex), /^PeX default for new torrents is on in the rTorrent config; torrents already loaded keep their own PeX setting — click to disable/);
  assert.doesNotMatch(V.rtDiscoveryHint(dht), /new torrents|already loaded/);
  const t = V.rtDiscoveryToast('pex', false, { live: true });
  assert.deepEqual(t, { text: 'PeX default for new torrents disabled (saved to ~/.rtorrent.rc.custom); torrents already loaded keep their own PeX setting', kind: 'ok' });
  assert.equal(V.rtDiscoveryToast('dht', true, { live: false, note: 'saved; DHT changes when rTorrent restarts' }).kind, 'warn');
  assert.doesNotMatch(V.rtDiscoveryToast('dht', true, { live: true }).text, /already loaded/);
  const on = V.rtDiscoveryOn({ dht: true, pex: true });
  assert.deepEqual(on.labels, ['DHT', 'PeX']);
  assert.match(on.title, /DHT is running now; PeX is the default for new torrents \(torrents already loaded keep their own setting\)/);
  assert.deepEqual(V.rtDiscoveryOn({ dht: false, pex: false }), { labels: [], title: '' });
});

test('the fleet list sorts by client name, a row without one being qBittorrent', () => {
  const rows = [[{ name: 'pm1' }, { name: 'a', client: 'rtorrent' }], [{ name: 'pm2' }, { name: 'b' }], [{ name: 'pm3' }, { name: 'c', client: 'qbt' }]];
  const order = (dir) => V.sortTorrents(rows, { key: 'client', dir }, (r) => r[1], (r) => r[0].name).map((r) => r[1].name).join('');
  assert.equal(order('asc'), 'bca', 'qBittorrent before rTorrent, ties by name');
  assert.equal(order('desc'), 'abc');
  assert.deepEqual(V.nextSort(null, 'client'), { key: 'client', dir: 'asc' });
});

test('monitor-only shows the rTorrent discovery state without a click hint, keeping the pending note (fred on PR 134)', () => {
  const [dht] = V.rtDiscovery({ reachable: true, dht: false, conf_dht: 'on' });
  assert.equal(dht.pending, true);
  const s = V.rtDiscoveryState(dht);
  assert.match(s, /until rTorrent restarts/);
  assert.doesNotMatch(s, /click/);
  assert.equal(V.rtDiscoveryHint(dht), `${s} — click to disable (saved to ~/.rtorrent.rc.custom)`);
});

test('a box control sends the revision it was drawn from, not a later one (fred on PR 136)', () => {
  const html = `<button ${V.boxRef({ id: 3, box_rev: 7 })} data-act="qbt:restart">`;
  const ds = Object.fromEntries([...html.matchAll(/data-(box|rev)="([^"]*)"/g)].map((m) => [m[1], m[2]]));
  assert.deepEqual(ds, { box: '3', rev: '7' });
  // The record moves on to revision 8; the control drawn at 7 still says 7.
  assert.equal(V.boxPath(ds, 'qbt/restart'), '/api/boxes/3/qbt/restart?rev=7');
  assert.equal(V.boxPath(ds, 'containers/abc/stop'), '/api/boxes/3/containers/abc/stop?rev=7');
  // A control without a reference sends none, which the server refuses (400).
  assert.equal(V.boxPath({}, 'qbt/stop'), '/api/boxes//qbt/stop?rev=');
  assert.equal(V.boxRef({ id: '<x>', box_rev: '"' }), 'data-box="0" data-rev="0"', 'numbers only in the markup');
});

test('a transfer is over, and succeeded, by its exit code alone', () => {
  const done99 = { phase: 'finished', rc: '0', progress: { pct: 99 } };
  assert.deepEqual(V.transferState(done99), { label: 'copied', kind: 'ok', over: true });
  assert.equal(V.transferMeter(done99), 100, 'a complete copy shows full although rsync said 99%');
  const at100 = { phase: 'running', rc: '', progress: { pct: 100 } };
  assert.equal(V.transferState(at100).over, false, '100% without an exit code is not done');
  assert.equal(V.transferMeter(at100), 99, 'and the bar does not claim it');
  assert.equal(V.transferState({ phase: 'finished', rc: '23', progress: { pct: 100 } }).kind, 'bad');
  assert.equal(V.transferMeter({ phase: 'finished', rc: '23', progress: { pct: 100 } }), 99);
  assert.equal(V.transferState({ phase: 'finished', rc: '24' }).label, 'copied with warnings');
  assert.equal(V.transferState({ phase: 'finished', rc: 'cancelled' }).label, 'cancelled');
  assert.equal(V.transferState({ phase: 'interrupted' }).over, true);
  assert.equal(V.transferState({ phase: 'running', scanning: true }).label, 'scanning');
  assert.equal(V.transferState({ phase: 'unconfirmed' }).over, false);
  assert.equal(V.transferMeter({ phase: 'running' }), 0);
});

test('only a confirmed launch toasts "copy started" (daybreak review)', () => {
  assert.deepEqual(V.startToast({ phase: 'running' }), ['copy started', 'ok']);
  assert.equal(V.startToast({ phase: 'unconfirmed' })[1], 'warn');
  assert.equal(V.startToast({ phase: 'preparing' })[1], 'warn');
  assert.equal(V.startToast(undefined)[1], 'warn');
});

test('a new folder name with surrounding spaces is refused, not trimmed (daybreak review)', () => {
  assert.match(V.newFolderError(' copy'), /space/);
  assert.match(V.newFolderError('copy '), /space/);
  assert.equal(V.newFolderError('my copy'), '');
});

test('"copy completed" heads owed cleanup only after a copy that completed (daybreak review)', () => {
  assert.match(V.transferCleanupLabel({ phase: 'finished', rc: '0' }), /^copy completed/);
  assert.match(V.transferCleanupLabel({ phase: 'finished', rc: '24' }), /^copy completed/);
  for (const v of [{ phase: 'finished', rc: 'cancelled' }, { phase: 'finished', rc: '23' }, { phase: 'interrupted' }, { phase: 'setup_failed' }]) {
    assert.equal(V.transferCleanupLabel(v), 'cleanup owed: ', JSON.stringify(v));
  }
});

test('the start-copy body uses the confirmed paths and the revisions they were listed under (fred on PR 142)', () => {
  const src = { id: 2, name: 'pm1', box_rev: 5 }, dst = { id: 7, name: 'pmstor01', box_rev: 3 };
  const c = V.transferConfirmation(src, { box_rev: 5, path: '/home/u/dl/x', rel: 'dl/x' }, dst, { box_rev: 3, path: '/home/v/in', rel: 'in', dirs: [] }, 'x copy');
  // The destination was edited (rev 4) after it was listed: the body still
  // names rev 3, so the server refuses with 409 instead of copying to the
  // record's new target.
  dst.box_rev = 4;
  assert.deepEqual(V.transferRequest(c), { dst_box: 7, dst_rev: 3, src_path: '/home/u/dl/x', dst_path: '/home/v/in/x copy' });
  assert.equal(c.srcRev, 5);
});

test('the confirmation names both exact paths and says the source stays untouched', () => {
  const c = V.transferConfirmation({ id: 2, name: 'pm1' }, { box_rev: 1, path: '/home/u/dl/x' }, { id: 7, name: 'pmstor01' }, { box_rev: 1, path: '/home/v', dirs: ['taken'] }, 'taken');
  assert.match(c.lines.join('\n'), /from pm1: \/home\/u\/dl\/x\//);
  assert.match(c.lines.join('\n'), /to pmstor01: \/home\/v\/taken\/ \(a new folder/);
  assert.match(c.lines.join('\n'), /source stays untouched: nothing in \/home\/u\/dl\/x on pm1 is moved, changed or deleted/);
  assert.equal(c.exists, true, 'a name already listed in the parent is flagged before the server refuses it');
});

test('picker paths stay relative to the home and climb no higher', () => {
  assert.equal(V.homeLabel(''), '~');
  assert.equal(V.homeLabel('a/b'), '~/a/b');
  assert.equal(V.joinPath('', 'x'), 'x');
  assert.equal(V.joinPath('a/b/', 'x'), 'a/b/x');
  assert.equal(V.parentRel('a/b'), 'a');
  assert.equal(V.parentRel('a'), '');
  assert.equal(V.parentRel(''), null);
});

test('a new folder name is one plain name', () => {
  assert.equal(V.newFolderError('from pm1'), '');
  for (const bad of ['', ' ', '.', '..', 'a/b', "it's", 'a"b', 'a\\b', 'a,b', '-x', 'a\nb', 'x'.repeat(256)]) {
    assert.notEqual(V.newFolderError(bad), '', JSON.stringify(bad));
  }
});

test('a reconnect reloads the transfers list on the transfers tab (fred on PR 142)', () => {
  assert.deepEqual(V.resyncLoads('box', 'transfers', 2), ['detail', 'transfers']);
  assert.deepEqual(V.resyncLoads('box', 'overview', 2), ['detail']);
  assert.deepEqual(V.resyncLoads('alerts', '', null), ['alerts']);
  assert.deepEqual(V.resyncLoads('fleet', '', null), []);
});

test('a start that answers late resets only its own draft (fred on PR 143)', () => {
  const a = { box: 2, confirm: {} }, blankA = { box: 2 };
  assert.equal(V.draftAfterStart(a, a, blankA), blankA, 'the draft that started is cleared');
  const b = { box: 1, src: 'typed meanwhile' };
  assert.equal(V.draftAfterStart(b, a, blankA), b, 'another box\'s draft opened while the start ran survives');
  assert.equal(V.draftAfterStart(null, a, blankA), null);
});
