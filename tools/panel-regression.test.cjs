const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// Execute the actual shipped functions, not a second implementation of them.
const source = fs.readFileSync(path.join(__dirname, '..', 'panel.go'), 'utf8');
const functions = source.slice(source.indexOf('  var P = "__PROVIDER__";'), source.indexOf('  function esc('));
assert(functions.includes('function adminKey()'));

function panel({ query = '', stored = '', embedded = '', typed = '', blockedStorage = false, fetchImpl } = {}) {
  const storage = new Map([['__PROVIDER__-mgmt-key', stored]]);
  const location = { search: query, pathname: '/panel', hash: '#account', host: 'fixture.invalid' };
  const replacements = [];
  const timers = new Set();
  const window = {
    location, navigator: { userAgent: 'fixture-browser' }, AbortController,
    atob: value => Buffer.from(value, 'base64').toString('binary'),
    history: { replaceState(_state, _title, url) { replacements.push(url); } },
    sessionStorage: {
      getItem(key) { if (blockedStorage) throw Error('blocked'); return storage.get(key); },
      setItem(key, value) { if (blockedStorage) throw Error('blocked'); storage.set(key, value); },
    },
    localStorage: { getItem() { return JSON.stringify({ state: { managementKey: embedded } }); } },
  };
  window.self = window;
  window.top = embedded ? {} : window;
  const context = vm.createContext({
    window, URLSearchParams, TextEncoder, TextDecoder, Uint8Array,
    document: { getElementById() { return { value: typed }; } },
    fetch: fetchImpl || (() => Promise.resolve({ ok: true, text: () => Promise.resolve('{}') })),
    setTimeout(fn, ms) { const id = setTimeout(fn, ms); timers.add(id); return id; },
    clearTimeout(id) { timers.delete(id); clearTimeout(id); },
  });
  vm.runInContext(functions, context);
  return { context, replacements, storage, timers };
}

test('a URL key overrides cached and embedded keys and is removed immediately', () => {
  const p = panel({ query: '?key=fixture-new&mode=dark&key=duplicate', stored: 'fixture-old', embedded: 'fixture-embedded' });
  assert.deepEqual(p.replacements, ['/panel?mode=dark#account']);
  assert.equal(p.context.adminKey(), 'fixture-new');
  assert.equal(p.context.adminKey(), 'fixture-new');
  assert.equal(p.storage.get('__PROVIDER__-mgmt-key'), 'fixture-new');
});

test('typed key wins, but does not prevent URL cleanup', () => {
  const p = panel({ query: '?key=fixture-url', typed: 'fixture-typed' });
  assert.deepEqual(p.replacements, ['/panel#account']);
  assert.equal(p.context.adminKey(), 'fixture-typed');
});

test('a rotated embedded CPA key overrides stale sessionStorage', () => {
  const p = panel({ stored: 'fixture-old', embedded: 'fixture-current' });
  assert.equal(p.context.adminKey(), 'fixture-current');
});

test('URL key survives repeated calls when sessionStorage is blocked', () => {
  const p = panel({ query: '?key=fixture%2Bencoded+space', blockedStorage: true });
  assert.equal(p.context.adminKey(), 'fixture+encoded space');
  assert.equal(p.context.adminKey(), 'fixture+encoded space');
  assert.equal(p.replacements.length, 1);
});

test('an empty URL key is cleaned, then falls back to remembered key', () => {
  const p = panel({ query: '?key=&mode=dark', stored: 'fixture-old' });
  assert.deepEqual(p.replacements, ['/panel?mode=dark#account']);
  assert.equal(p.context.adminKey(), 'fixture-old');
});

test('optional balance request aborts and releases its deadline timer', async () => {
  let aborted = false;
  const p = panel({ fetchImpl: (_url, opts) => new Promise((_resolve, reject) => {
    assert(opts.signal);
    opts.signal.addEventListener('abort', () => {
      aborted = true;
      const error = new Error('aborted');
      error.name = 'AbortError';
      reject(error);
    });
  }) });
  await assert.rejects(p.context.call('/benefit-balance', { timeout: 10 }), { name: 'AbortError' });
  assert(aborted);
  assert.equal(p.timers.size, 0);
});

test('successful optional request cancels its timer', async () => {
  const p = panel();
  await p.context.call('/benefit-balance', { timeout: 5000 });
  assert.equal(p.timers.size, 0);
});

test('the complete dashboard script remains syntactically valid', () => {
  new vm.Script(source.slice(source.indexOf('<script>') + '<script>'.length, source.indexOf('</script>')));
});
