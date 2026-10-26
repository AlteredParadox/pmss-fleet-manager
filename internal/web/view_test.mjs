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

test('the row action is client-aware: rTorrent error rows are running and get stop (daybreak blue review of #124)', () => {
  assert.equal(V.torrentAction('qbt', 'paused'), 'start');
  assert.equal(V.torrentAction('qbt', 'error'), 'start', 'qBittorrent error/missingFiles are stopped torrents');
  assert.equal(V.torrentAction('qbt', 'seeding'), 'stop');
  assert.equal(V.torrentAction('qbt', 'checking'), 'stop');
  assert.equal(V.torrentAction('rtorrent', 'paused'), 'start');
  assert.equal(V.torrentAction('rtorrent', 'error'), 'stop', 'an rTorrent error row is active with a tracker message');
  assert.equal(V.torrentAction('rtorrent', 'leeching'), 'stop');
  assert.equal(V.torrentAction(undefined, 'error'), 'start', 'rows without a client are qBittorrent\'s');
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
  assert.match(app, /pfmView\.torrentAction\(client, t\.kind\)/, 'the start/stop choice goes through the helper');
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
  assert.equal(V.addSummary({ added: 2, stopped_early: 'connection reset' }, false, 'rtorrent'), '2 added to rTorrent; then stopped, the rest were not sent: connection reset', 'a partial batch reports what landed');
  assert.doesNotMatch(V.addSummary({ added: 1, unlabelled: 1, deferred: 1 }, false, 'rtorrent'), /metadata|deferred|applied when/, 'no deferred promise survives the backend simplification');
  assert.equal(V.addSummary({ links: 2, files: 1 }, false, 'qbt'), '3 torrents sent to qBittorrent');
  assert.equal(V.addSummary({ counted: true, added: 1, pending: 1, failed: 1 }, true, 'qbt'), '1 added (stopped), 1 still being fetched, 1 not added (already present or not a torrent)');
});
