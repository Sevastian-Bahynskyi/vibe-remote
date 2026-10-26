import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('../internal/server/static/app.js', import.meta.url), 'utf8');

function dashboard(every) {
  let now = 0;
  const events = new Map();
  const nodes = new Map();
  const pending = [];
  let tick;
  const element = () => ({id: 'region', isConnected: true, dataset: {refresh: 'fragment', refreshEvery: String(every)}, contains: () => false, querySelector: () => null, querySelectorAll: () => []});
  nodes.set('region', element());
  const document = {
    visibilityState: 'visible', activeElement: null,
    documentElement: {dataset: {}},
    getElementById: id => nodes.get(id),
    querySelector: () => null,
    querySelectorAll: selector => selector === '[data-refresh]' ? [...nodes.values()] : [],
    addEventListener: (name, fn) => events.set(name, [...(events.get(name) || []), fn]),
  };
  const fire = name => (events.get(name) || []).forEach(fn => fn({}));
  const window = {
    localStorage: {getItem: () => null, removeItem: () => {}, setItem: () => {}},
    addEventListener: document.addEventListener,
    setInterval: fn => {tick = fn;}, setTimeout: () => {},
    matchMedia: () => ({matches: false, addEventListener: () => {}}),
    htmx: {swap: old => {old.isConnected = false; nodes.set('region', element()); fire('htmx:afterSwap');}},
  };
  class DOMParser {
    parseFromString(html) {return {body: {innerHTML: html}, getElementById: () => null, querySelectorAll: () => []};}
  }
  vm.runInNewContext(source, {
    window, document, navigator: {onLine: true}, DOMParser, AbortController,
    Date: {now: () => now},
    fetch: (_url, options) => new Promise(resolve => pending.push({resolve, signal: options.signal})),
  });
  return {
    document, window, pending, fire,
    async at(time) {now = time; tick(); await Promise.resolve();},
    async complete(index = pending.length - 1) {
      pending[index].resolve({ok: true, text: async () => '<section id="region"></section>'});
      for (let i = 0; i < 8; i++) await Promise.resolve();
    },
  };
}

test('polling retains the 20-second cadence across replaced DOM nodes', async () => {
  const page = dashboard(20000);
  await page.at(20000); assert.equal(page.pending.length, 1); await page.complete();
  for (let t = 20500; t < 40000; t += 500) await page.at(t);
  assert.equal(page.pending.length, 1);
  await page.at(40000); assert.equal(page.pending.length, 2);
});

test('slow requests never overlap and navigation rejects obsolete responses', async () => {
  const page = dashboard(1500);
  await page.at(1500);
  await page.at(20000); assert.equal(page.pending.length, 1);
  page.fire('htmx:beforeRequest'); assert.equal(page.pending[0].signal.aborted, true);
  await page.complete(0);
  assert.equal(page.window.vibeRemotePolling().obsoleteResponses, 1);
  assert.equal(page.window.vibeRemotePolling().inFlight, 0);
});

test('hidden pages stop polling and visibility return refreshes once', async () => {
  const page = dashboard(60000);
  page.document.visibilityState = 'hidden'; page.fire('visibilitychange');
  await page.at(120000); assert.equal(page.pending.length, 0);
  page.document.visibilityState = 'visible'; page.fire('visibilitychange');
  assert.equal(page.pending.length, 1);
  await page.complete(); await page.at(120500);
  assert.equal(page.pending.length, 1);
});

test('activity and alert cadences remain separate', async () => {
  for (const cadence of [1500, 2000, 60000]) {
    const page = dashboard(cadence);
    await page.at(cadence - 1); assert.equal(page.pending.length, 0);
    await page.at(cadence); assert.equal(page.pending.length, 1);
  }
});
