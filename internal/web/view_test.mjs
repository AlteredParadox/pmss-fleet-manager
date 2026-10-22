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
