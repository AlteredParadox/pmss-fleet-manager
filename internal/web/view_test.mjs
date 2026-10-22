// Node test (node --test, run by `make jscheck`) for the pure rendering
// helpers in static/view.js: the overview's host-in-grey, you-in-blue bars.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const src = readFileSync(new URL('./static/view.js', import.meta.url), 'utf8');
vm.runInThisContext(src); // defines globalThis.pfmView, as the page's script tag does
const V = globalThis.pfmView;

const width = (html, nth) => Number(html.match(/width:([\d.]+)%/g)[nth].match(/[\d.]+/)[0]);

test('an unknown host share is a dash, never 0% (fred review on #110)', () => {
  // A valid host reading and valid, nonzero quota-relative tenant usage, but
  // no host core count: host_pct is then a zero-initialised field.
  const cpu = { ok: true, pct: 37, host_pct: 0, quota_cpus: 2, host_cpus: 0 };
  const host = { cpu_ok: true, cpu_busy_pct: 40 };
  assert.equal(V.hostShare(cpu), null);
  const out = V.ovCpuBar(cpu, host);
  assert.match(out.html, /you — of the host/);
  assert.doesNotMatch(out.html, /you 0/);
  assert.equal(width(out.html, 0), 40);
  assert.equal(width(out.html, 1), 0);
  assert.equal(out.caption, 'host 40% busy');
  assert.equal(V.hostShare({ ok: false, host_cpus: 8, host_pct: 0 }), null, 'no tenant sample');
  assert.equal(V.hostShare(undefined), null);
});

test('a known share draws blue inside grey on one scale', () => {
  const out = V.ovCpuBar({ ok: true, pct: 2.1, host_pct: 0.52, quota_cpus: 2, host_cpus: 8 }, { cpu_ok: true, cpu_busy_pct: 40 });
  assert.match(out.html, /host 40% busy · you 0\.5% of the host/);
  assert.equal(width(out.html, 0), 40);
  assert.equal(width(out.html, 1), 1.3); // 0.52 of 40, so the blue part is 0.52% of the whole bar
  assert.equal(V.hostShare({ ok: true, host_cpus: 8, host_pct: 0.52 }), 0.52);
});

test('without a host cpu reading the bar is the quota meter', () => {
  const out = V.ovCpuBar({ ok: true, pct: 37, host_pct: 0.5, host_cpus: 8 }, { cpu_ok: false });
  assert.match(out.html, /class="meter thick"/);
  assert.equal(width(out.html, 0), 37);
  assert.equal(out.caption, '');
  assert.equal(width(V.ovCpuBar({ ok: true, pct: 250 }, undefined).html, 0), 100, 'capped at the bar');
});

test('ram: host used in grey, your processes in blue, limit colour kept', () => {
  const out = V.ovRamBar({ ok: true, pct: 96, rss_bytes: 15, used_bytes: 40 }, { mem_total: 100, mem_avail: 40 }, 'bad');
  assert.equal(width(out.html, 0), 60);
  assert.equal(width(out.html, 1), 25); // 15 of 60
  assert.match(out.html, /class="bad"/);
  assert.match(out.html, /host memory 60% used · your processes 15% of it/);
  assert.equal(out.caption, 'host 60%');
  const noRss = V.ovRamBar({ ok: true, pct: 10, rss_bytes: 0, used_bytes: 30 }, { mem_total: 100, mem_avail: 50 });
  assert.equal(width(noRss.html, 1), 60, 'falls back to charged memory when rss is unknown');
  const noHost = V.ovRamBar({ ok: true, pct: 42 }, { mem_total: 0 }, 'warn');
  assert.match(noHost.html, /class="meter"/);
  assert.equal(width(noHost.html, 0), 42);
});

test('youInHost clamps and escapes', () => {
  const html = V.youInHost(30, 50, 'a "quoted" <title>', 'warn');
  assert.equal(width(html, 1), 100, 'more than the host is drawn as all of it');
  assert.match(html, /title="a &quot;quoted&quot; &lt;title&gt;"/);
  assert.equal(width(V.youInHost(0, 5, 't'), 1), 0, 'no host usage, nothing to be inside of');
  assert.equal(width(V.youInHost(-5, NaN, 't'), 0), 0);
});
