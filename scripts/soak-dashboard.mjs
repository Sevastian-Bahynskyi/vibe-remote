import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { mkdir, writeFile } from 'node:fs/promises';
import { setTimeout as sleep } from 'node:timers/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const { chromium } = createRequire(import.meta.url)('playwright');
const base = process.env.SOAK_URL || 'http://127.0.0.1:47174/';
assert.equal(new URL(base).hostname, '127.0.0.1', 'Use the disposable loopback fixture only.');
const duration = Number(process.env.SOAK_SECONDS || 1800) * 1000;
const warmup = Number(process.env.SOAK_WARMUP_SECONDS || 60) * 1000;
const output = process.env.SOAK_OUTPUT || 'docs/audits/2026-10-26-resource-soak.json';
const execute = promisify(execFile);
const {stdout: listener} = await execute('/usr/sbin/lsof', ['-t', '-nP', '-iTCP:' + new URL(base).port, '-sTCP:LISTEN']);
const daemonPid = Number(listener.trim());
assert.ok(Number.isSafeInteger(daemonPid) && daemonPid > 1);
const rss = async pid => Number((await execute('/bin/ps', ['-p', String(pid), '-o', 'rss='])).stdout.trim());
const browser = await chromium.launch({channel: 'chrome', headless: true, args: ['--disable-background-networking', '--disable-component-update']});
const context = await browser.newContext({viewport: {width: 1280, height: 900}});
const page = await context.newPage();
const errors = [];
const counts = {};
const active = new Map();
const maximum = {};
const samples = [];
const started = Date.now();
let complete = false;

await context.addInitScript(() => {
  if (!localStorage.getItem('vibe-remote-history-cleared-v1')) localStorage.setItem('htmx-history-cache', 'VIBE_REMOTE_SYNTHETIC_HISTORY_MARKER');
});
page.on('pageerror', error => errors.push(error.message));
page.on('request', request => {
  const path = new URL(request.url()).pathname;
  if (!path.includes('/ui/fragments/') && !path.includes('/console')) return;
  counts[path] = (counts[path] || 0) + 1;
  active.set(request, path);
  const concurrent = [...active.values()].filter(value => value === path).length;
  maximum[path] = Math.max(maximum[path] || 0, concurrent);
});
for (const event of ['requestfinished', 'requestfailed']) page.on(event, request => active.delete(request));

const home = async () => {
  await page.locator('.topbar-home').click();
  await page.locator('#session-list').waitFor();
};
const detail = async () => {
  await page.locator('a.card-open[href*="id=synthetic"]').click();
  await page.locator('#session-detail').waitFor();
};
const assertClean = async () => {
  const state = await page.evaluate(() => ({
    history: localStorage.getItem('htmx-history-cache'),
    consoles: window.vibeRemoteConsoles(),
    polling: window.vibeRemotePolling(),
  }));
  assert.equal(state.history, null, 'HTML/terminal data persisted to htmx history.');
  assert.equal(state.consoles.terminals, 0, 'A closed terminal remained active.');
  assert.equal(state.consoles.records, 0, 'A detached console remained registered.');
  return state;
};

try {
  await page.goto(base + '?screen=session&id=synthetic');
  await page.getByRole('link', {name: 'Open in Claude', exact: true}).waitFor();
  await mkdir('docs/audits/assets', {recursive: true});
  await page.screenshot({path: 'docs/audits/assets/dashboard-desktop.png', fullPage: true});
  const mobile = await context.newPage();
  await mobile.setViewportSize({width: 390, height: 844});
  await mobile.goto(base + '?screen=session&id=synthetic');
  assert.equal(await mobile.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await mobile.screenshot({path: 'docs/audits/assets/dashboard-mobile.png', fullPage: true});
  await mobile.close();
  await page.bringToFront();

  await page.getByText('Approval console', {exact: true}).click();
  await page.locator('.console-status').filter({hasText: 'Connected to this session.'}).waitFor();
  const originalConsole = await page.locator('[data-console]').getAttribute('id');
  await sleep(5000);
  assert.equal(await page.locator('[data-console]').getAttribute('id'), originalConsole);
  assert.equal(await page.evaluate(() => window.vibeRemoteConsoles().terminals), 1);
  await home(); await assertClean();

  await page.getByRole('link', {name: /^Accounts/}).click();
  await page.getByLabel('Add a Claude account', {exact: true}).fill('unsaved-fixture@example.com');
  await page.getByRole('button', {name: 'Refresh', exact: true}).click();
  await sleep(750);
  assert.equal(await page.getByLabel('Add a Claude account', {exact: true}).inputValue(), 'unsaved-fixture@example.com');
  await home();
  await page.goBack();
  await page.locator('#account-panel').waitFor();
  await home(); await assertClean();

  let delayOnce = true;
  await page.route('**/ui/fragments/sessions', async route => {
    if (delayOnce) {delayOnce = false; await sleep(3500);}
    await route.continue().catch(() => {});
  });
  await page.evaluate(() => window.dispatchEvent(new Event('online')));
  await sleep(250);
  await page.getByRole('link', {name: /^Accounts/}).click();
  await sleep(4000);
  assert.equal(await page.locator('#session-list').count(), 0, 'Obsolete home response replaced the Accounts screen.');
  await page.unroute('**/ui/fragments/sessions');
  await home(); await assertClean();

  await sleep(warmup);
  const measuredStart = Date.now();
  const requestBaseline = {...counts};
  while (Date.now() - measuredStart < duration) {
    const cycleStart = Date.now();
    await detail();
    await page.getByText('Approval console', {exact: true}).click();
    await page.locator('.console-status').filter({hasText: 'Connected to this session.'}).waitFor();
    await sleep(6000);
    await page.getByText('Approval console', {exact: true}).click();
    await page.waitForFunction(() => window.vibeRemoteConsoles().terminals === 0);
    await home(); await assertClean();
    await page.goBack(); await page.locator('#session-detail').waitFor();
    await home(); const state = await assertClean();
    const client = await context.newCDPSession(page);
    await client.send('HeapProfiler.collectGarbage');
    const heap = await client.send('Runtime.getHeapUsage');
    const dom = await client.send('Memory.getDOMCounters');
    const detached = await client.send('DOM.getDetachedDomNodes');
    await client.detach();
    const daemon = await (await context.request.get(base + 'api/diagnostics', {headers: {'X-Vibe-Remote': '1'}})).json();
    const fixture = await (await context.request.get(base + 'api/state')).json();
    assert.equal(fixture.remoteSessions.length, 1);
    assert.equal(fixture.remoteSessions[0].id, 'synthetic');
    const processes = {daemonRssKiB: await rss(daemonPid), workerRssKiB: await rss(fixture.remoteSessions[0].worker.pid)};
    samples.push({elapsedSeconds: (Date.now() - measuredStart) / 1000, heap, dom, detachedTrees: detached.detachedNodes.length, state, daemon, processes});
    if (samples.length % 2 === 0) console.log(JSON.stringify({elapsedSeconds: Math.round((Date.now() - measuredStart) / 1000), samples: samples.length, heapUsedBytes: heap.usedSize, detachedTrees: detached.detachedNodes.length, terminals: state.consoles.terminals, regions: state.polling.regions}));
    await writeFile(output, JSON.stringify({complete: false, durationSeconds: (Date.now() - measuredStart) / 1000, browserVersion: browser.version(), samples, counts, maximum, errors}, null, 2));
    await sleep(Math.max(0, 30000 - (Date.now() - cycleStart)));
  }
  const measuredSeconds = (Date.now() - measuredStart) / 1000;
  const retained = samples.map(sample => sample.heap.usedSize);
  const detached = samples.map(sample => sample.detachedTrees);
  assert.equal(errors.length, 0, 'Browser errors occurred.');
  assert.ok(samples.every(sample => sample.state.consoles.terminals === 0 && sample.state.consoles.records === 0));
  assert.ok(Object.entries(maximum).filter(([path]) => !path.includes('/console')).every(([,value]) => value <= 1), 'Region requests overlapped.');
  complete = true;
  await writeFile(output, JSON.stringify({complete, startedAt: new Date(started).toISOString(), durationSeconds: measuredSeconds, warmupSeconds: warmup / 1000, browserVersion: browser.version(), retainedHeap: {first: retained[0], last: retained.at(-1), minimum: Math.min(...retained), maximum: Math.max(...retained)}, detachedTrees: {first: detached[0], last: detached.at(-1), maximum: Math.max(...detached)}, samples, requestBaseline, counts, maximum, errors}, null, 2));
  console.log(JSON.stringify({complete, durationSeconds: measuredSeconds, samples: samples.length, output}));
} finally {
  if (!complete) await writeFile(output, JSON.stringify({complete: false, browserVersion: browser.version(), samples, counts, maximum, errors}, null, 2));
  await browser.close();
}
