/* ============================================================
   FELUCCA CONSOLE — app.test.mjs
   Regression tests for the console's polling and status reporting.

   Run:  node --test ui/app.test.mjs      (node >= 18)

   These cover the two properties an operator's safety depends on and that a
   passing 401-banner test does not prove:

     1. The console never talks to the API without a credential. feluccad's
        brute-force guard counts failed attempts per source address and backs
        the source off for up to a minute; an unattended console on a 3s timer
        generated two failures per poll and locked out its own operator.

     2. The console never shows data the operator could mistake for the live
        fleet. Falling back to MOCK_NODES on any fetch failure — with nothing
        but a small chip to say so — means a reader can believe a fabricated
        node is up.

   app.js is a classic browser script, so it is evaluated in a vm context on a
   small DOM stub and asked for its internals through a trailing export line.
   ============================================================ */

import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const APP_JS = path.join(path.dirname(fileURLToPath(import.meta.url)), 'app.js');

/* --- Minimal DOM ------------------------------------------ */

function makeDom() {
  const byId = new Map();

  class ClassList {
    constructor(el) { this.el = el; this.items = new Set(); }
    add(...c) { c.forEach(x => this.items.add(x)); this.#sync(); }
    remove(...c) { c.forEach(x => this.items.delete(x)); this.#sync(); }
    contains(c) { return this.items.has(c); }
    toggle(c, on) {
      const next = on === undefined ? !this.items.has(c) : !!on;
      if (next) this.items.add(c); else this.items.delete(c);
      this.#sync();
      return next;
    }
    #sync() { this.el._class = [...this.items].join(' '); }
  }

  class El {
    constructor(tag) {
      this.tagName = String(tag).toUpperCase();
      this.children = [];
      this.parent = null;
      this.dataset = {};
      this.style = {};
      this.textContent = '';
      this.innerHTML = '';
      this.value = '';
      this.listeners = {};
      this._id = '';
      this._class = '';
      this.classList = new ClassList(this);
    }
    get id() { return this._id; }
    set id(v) { this._id = v; if (v) byId.set(v, this); }
    get className() { return this._class; }
    set className(v) {
      this._class = String(v);
      this.classList.items = new Set(this._class.split(/\s+/).filter(Boolean));
    }
    appendChild(c) { c.parent = this; this.children.push(c); return c; }
    append(...cs) { cs.forEach(c => this.appendChild(c)); }
    insertBefore(c) { return this.appendChild(c); }
    removeChild(c) {
      const i = this.children.indexOf(c);
      if (i >= 0) this.children.splice(i, 1);
    }
    remove() {
      if (this.parent) this.parent.removeChild(this);
      if (this._id && byId.get(this._id) === this) byId.delete(this._id);
    }
    addEventListener(ev, fn) { (this.listeners[ev] ||= []).push(fn); }
    dispatch(ev, arg) { (this.listeners[ev] || []).forEach(f => f(arg)); }
    querySelectorAll() { return []; }
    querySelector(sel) {
      const cls = sel.startsWith('.') ? sel.slice(1) : null;
      for (const c of this.children) {
        if (cls && c.classList.contains(cls)) return c;
        const deep = c.querySelector(sel);
        if (deep) return deep;
      }
      return null;
    }
    /* All text in this subtree — what the operator would actually read. */
    text() { return this.textContent + this.children.map(c => c.text()).join(''); }
  }

  const body = new El('body');
  const document = {
    body,
    createElement: t => new El(t),
    getElementById: id => byId.get(id) || null,
    querySelector: () => null,
    querySelectorAll: () => [],
    addEventListener: () => {},
  };
  for (const id of ['app', 'header', 'content', 'modals', 'toast-container']) {
    const el = new El('div');
    el.id = id;
    body.appendChild(el);
  }
  return { document, body, byId, El };
}

/* --- Load app.js into a fresh realm ------------------------ */

function loadApp() {
  const dom = makeDom();
  const fetchCalls = [];
  const timers = [];
  let routes = () => { throw new Error('no route configured'); };

  const ctx = {
    document: dom.document,
    window: { location: { search: '', pathname: '/', hash: '' } },
    history: { replaceState: () => {} },
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    sessionStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    URLSearchParams,
    CSS: { escape: s => String(s) },
    console,
    AbortSignal: { timeout: () => ({}) },
    /* Timers are recorded, never fired: a test asserts on whether the console
       scheduled another poll at all. */
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    clearTimeout: id => { if (timers[id - 1]) timers[id - 1].cleared = true; },
    setInterval: () => 0,
    clearInterval: () => {},
    fetch: async (url, init) => {
      fetchCalls.push({ url, init });
      return routes(url);
    },
  };
  ctx.globalThis = ctx;
  vm.createContext(ctx);

  const src = fs.readFileSync(APP_JS, 'utf8') +
    '\n;globalThis.__felucca = { state, poll, setToken, getToken, MOCK_NODES, render };\n';
  vm.runInContext(src, ctx, { filename: 'app.js' });

  const api = ctx.__felucca;
  return {
    ...api,
    ctx,
    dom,
    fetchCalls,
    /* Pending (uncleared) timer callbacks — i.e. "did it schedule another poll?" */
    pending: () => timers.filter(t => !t.cleared),
    route(fn) { routes = fn; },
    banner: () => dom.document.getElementById('auth-banner'),
    bannerText: () => dom.document.getElementById('auth-banner')?.text() ?? '',
    badge: () => dom.document.getElementById('mock-badge'),
  };
}

/* An HTTP response good enough for app.js's three call sites. */
function reply(status, body, headers = {}) {
  const lower = Object.fromEntries(
    Object.entries(headers).map(([k, v]) => [k.toLowerCase(), String(v)]));
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get: k => lower[String(k).toLowerCase()] ?? null },
    json: async () => body,
    text: async () => JSON.stringify(body),
  };
}

const FLEET = url =>
  reply(200, url.includes('/nodes')
    ? { nodes: [{ id: 'nd-1', hostname: 'w0', status: 'ready', cpus: 4, mem_total_mib: 8192, mem_free_mib: 4096, vm_count: 0, last_heartbeat: new Date().toISOString() }] }
    : { sandboxes: [] });

/* --- 1. No token: nothing is sent ------------------------- */

test('poll() sends no request at all when no token is set', async () => {
  const app = loadApp();
  app.route(FLEET);

  await app.poll();

  assert.equal(app.fetchCalls.length, 0,
    'an unattended console must not spend feluccad auth attempts on a 3s timer');
  assert.equal(app.state.dataMode, 'no-token');
});

test('no token stops the loop rather than rescheduling failures', async () => {
  const app = loadApp();
  app.route(FLEET);

  await app.poll();

  assert.equal(app.pending().length, 0,
    'nothing to poll without a credential — the Apply button restarts the loop');
});

test('no token puts the paste-in banner and a badge on screen', async () => {
  const app = loadApp();
  app.route(FLEET);

  // render() is deliberately NOT stubbed here: the no-token path now re-renders
  // to clear the skeleton, so this also proves that path does not throw.
  await app.poll();

  assert.match(app.bannerText(), /token/i);
  assert.ok(app.dom.document.getElementById('auth-token-input'),
    'the banner must offer the token box, since nothing else will');
  assert.notEqual(app.dom.document.getElementById('content').innerHTML, '',
    'the real render must have run and repainted the content area');
});

test('applying a token in the banner starts polling', async () => {
  const app = loadApp();
  app.route(FLEET);
  await app.poll();

  const input = app.dom.document.getElementById('auth-token-input');
  input.value = '  ' + 'a'.repeat(64) + '  ';
  app.dom.document.getElementById('auth-token-apply').dispatch('click');
  await new Promise(r => setImmediate(r));

  assert.equal(app.getToken(), 'a'.repeat(64), 'the pasted token is trimmed and held');
  assert.equal(app.fetchCalls.length, 2, 'applying a token polls immediately');
});

test('the loading skeleton is cleared, not left shimmering, when a poll cannot run', async () => {
  for (const [name, token, route] of [
    ['no token', null, FLEET],
    ['401', 'x'.repeat(64), () => reply(401, {})],
    ['429', 'x'.repeat(64), () => reply(429, {}, { 'Retry-After': '5' })],
  ]) {
    const app = loadApp();
    let rendered = 0;
    app.ctx.render = () => { rendered++; };
    assert.equal(app.state.loading, true, 'precondition: the console boots in the skeleton');

    if (token) app.setToken(token);
    app.route(route);
    await app.poll();

    assert.equal(app.state.loading, false, `${name}: loading must be cleared`);
    assert.ok(rendered > 0,
      `${name}: clearing loading without a re-render leaves fake skeleton rows on screen`);
  }
});

/* --- 2. 429 is reported, not swallowed -------------------- */

test('429 reports the rate limit instead of switching to mock data', async () => {
  const app = loadApp();
  app.setToken('t'.repeat(64));
  app.route(() => reply(429, { error: 'too many failed attempts' }, { 'Retry-After': '17' }));

  await app.poll();

  assert.equal(app.state.dataMode, 'rate-limited');
  assert.equal(app.state.isMock, false,
    'a throttled console must not quietly present a fabricated fleet');
  assert.match(app.bannerText(), /rate limited/i);
  assert.match(app.bannerText(), /17s/,
    "the operator needs to know how long the wait is, not just that something broke");
});

test('429 waits out Retry-After instead of feeding the backoff', async () => {
  const app = loadApp();
  app.setToken('t'.repeat(64));
  app.route(() => reply(429, {}, { 'Retry-After': '17' }));

  await app.poll();
  const next = app.pending();

  assert.equal(next.length, 1, 'the loop continues, but later');
  assert.ok(next[0].ms >= 17000,
    `next poll must be after feluccad's Retry-After, got ${next[0].ms}ms`);
});

test('a token applied during a lockout is retried at once', async () => {
  const app = loadApp();
  app.setToken('bad-token');
  app.route(() => reply(429, {}, { 'Retry-After': '60' }));
  await app.poll();
  assert.equal(app.state.dataMode, 'rate-limited');

  const before = app.fetchCalls.length;
  app.route(FLEET);
  app.dom.document.getElementById('mock-badge')?.dispatch('click'); // reopen banner
  const input = app.dom.document.getElementById('auth-token-input');
  assert.ok(input, 'the rate-limit banner must lead back to the token box');
  input.value = 'g'.repeat(64);
  app.dom.document.getElementById('auth-token-apply').dispatch('click');
  await new Promise(r => setImmediate(r));

  assert.ok(app.fetchCalls.length > before,
    'a correct token must not sit behind the backoff the console itself caused');
});

/* --- 3. 401 does not hammer the throttle ------------------ */

test('401 pauses the loop and asks for a token', async () => {
  const app = loadApp();
  app.setToken('wrong-token');
  app.route(() => reply(401, { error: 'unauthorized' }));

  await app.poll();

  assert.equal(app.state.dataMode, 'unauthorized');
  assert.equal(app.pending().length, 0,
    'replaying a rejected token every 3s is what walks the backoff to a minute');
  assert.ok(app.dom.document.getElementById('auth-token-input'));
});

/* --- 4. Mock/degraded is never mistakable for live -------- */

test('an unreachable control plane keeps the last real fleet and marks it stale', async () => {
  const app = loadApp();
  app.setToken('t'.repeat(64));
  app.route(FLEET);
  await app.poll();
  assert.equal(app.state.dataMode, 'live');
  const live = app.state.nodes;

  app.route(() => { throw new Error('ECONNREFUSED'); });
  await app.poll();

  assert.equal(app.state.dataMode, 'unreachable');
  assert.equal(app.state.isMock, false);
  assert.deepEqual(app.state.nodes, live,
    'a fleet that was real must not be replaced by MOCK_NODES behind the operator');
  assert.match(app.bannerText(), /unreachable/i);
});

test('mock data is announced in a banner, not just a chip', async () => {
  const app = loadApp();
  app.setToken('t'.repeat(64));
  app.route(() => { throw new Error('ECONNREFUSED'); });
  app.ctx.render = () => {}; // the full render needs a real DOM; not under test here

  await app.poll();

  assert.equal(app.state.dataMode, 'mock');
  assert.equal(app.state.nodes.length, app.MOCK_NODES.length);
  assert.match(app.bannerText(), /mock data/i,
    'a chip in the header is not enough to stop an operator reading fake nodes as real');
  assert.match(app.bannerText(), /nothing below is real/i);
});

test('every non-live mode labels the header badge and the LIVE indicator', async () => {
  for (const [route, token, badge, live] of [
    [FLEET, null, 'No token', 'NO TOKEN'],
    [() => reply(401, {}), 'x'.repeat(64), 'Unauthorized', 'NO AUTH'],
    [() => reply(429, {}, { 'Retry-After': '5' }), 'x'.repeat(64), 'Rate limited', 'THROTTLED'],
  ]) {
    const app = loadApp();
    // The badge and indicator live in the header, which render() builds.
    const b = app.dom.document.createElement('div'); b.id = 'mock-badge';
    const t = app.dom.document.createElement('span'); t.id = 'live-text';
    const d = app.dom.document.createElement('div'); d.id = 'live-dot';
    app.dom.body.append(b, t, d);

    if (token) app.setToken(token);
    app.route(route);
    await app.poll();

    assert.equal(b.textContent, badge);
    assert.equal(b.classList.contains('hidden'), false, `${badge} badge must be visible`);
    assert.equal(t.textContent, live);
    assert.ok(d.classList.contains('stale'), `${live} must not read as a healthy live dot`);
  }
});

/* --- 5. CSP: the console may not grow inline script -------- */

test('no inline event-handler attributes are emitted', () => {
  const src = fs.readFileSync(APP_JS, 'utf8');
  const hits = src.match(/\son(click|error|load|input|change|mouse[a-z]+)\s*=/g);
  assert.equal(hits, null,
    `script-src 'self' has no 'unsafe-inline': ${hits && hits.join(', ')}`);
});
