const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const uiDir = path.join(__dirname, '../internal/opendesign/ui');
const source = name => fs.readFileSync(path.join(uiDir, name), 'utf8');
const html = source('console.html');
const order = ['auth.js', 'core.js', 'api.js', 'console.js'];

function pageHTML() {
  // Mirror internal/opendesign/ui.go's placeholder substitution.
  let page = html.replace('/*__OPEN_DESIGN_AUTH__*/', source('auth.js'));
  page = page.replace('/*__OPEN_DESIGN_CORE__*/', source('core.js'));
  page = page.replace('/*__OPEN_DESIGN_API__*/', source('api.js'));
  return page.replace('/*__OPEN_DESIGN_CONSOLE__*/', source('console.js'));
}

// `node.children` is an HTMLCollection in a browser: length, index access and
// iteration only. Wrapping the backing array makes the fake DOM reject calls
// like `children.find(...)`, which is exactly the class of bug a real browser
// surfaces (FakeNode used to expose a plain Array and hid the failure).
const ARRAY_ONLY = new Set([
  'find', 'findIndex', 'findLast', 'findLastIndex', 'map', 'filter', 'some', 'every', 'forEach',
  'reduce', 'reduceRight', 'slice', 'concat', 'indexOf', 'lastIndexOf', 'includes', 'join', 'sort',
  'reverse', 'flat', 'flatMap', 'at', 'pop', 'push', 'shift', 'unshift', 'splice', 'fill', 'copyWithin',
  'toSorted', 'toReversed', 'toSpliced', 'with',
]);

function htmlCollection(nodes) {
  return new Proxy(nodes, {
    get(target, prop, receiver) {
      if (typeof prop === 'string' && ARRAY_ONLY.has(prop)) {
        throw new TypeError(`children.${prop} is not a function`);
      }
      if (prop === 'item') return index => target[index] ?? null;
      if (prop === 'namedItem') return () => null;
      return Reflect.get(target, prop, receiver);
    },
  });
}

class FakeNode {
  constructor(tag) {
    this.tagName = String(tag || 'div').toUpperCase();
    this._children = [];
    this.attributes = {};
    this.listeners = new Map();
    this.value = '';
    this.checked = false;
    this.disabled = false;
    this.hidden = false;
    this._text = '';
    this.className = '';
  }
  get children() {
    return htmlCollection(this._children);
  }
  set textContent(value) {
    this._text = String(value ?? '');
    this._children = [];
  }
  get textContent() {
    return this._text + this._children.map(child => child.textContent).join('');
  }
  set innerHTML(value) {
    throw new Error('tests reject innerHTML rendering');
  }
  append(...nodes) {
    for (const node of nodes) if (node) this._children.push(node);
  }
  replaceChildren(...nodes) {
    this._children = nodes.filter(Boolean);
  }
  addEventListener(type, handler) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(handler);
  }
  async emit(type, event = {}) {
    for (const handler of this.listeners.get(type) || []) await handler(event);
  }
  setAttribute(name, value) {
    this.attributes[name] = String(value);
    if (name === 'value') this.value = String(value);
    if (name === 'type') this.type = String(value);
  }
  getAttribute(name) {
    return this.attributes[name] ?? null;
  }
  focus() {}
  scrollIntoView() {}
}

function storage(initial = {}) {
  const map = new Map(Object.entries(initial));
  return {
    map,
    getItem: name => (map.has(name) ? map.get(name) : null),
    setItem: (name, value) => map.set(name, String(value)),
    removeItem: name => map.delete(name),
    clear: () => map.clear(),
    keys: () => [...map.keys()],
  };
}

function fakePage({ url = 'https://cpa.example/v0/resource/plugins/open-design/console', persistent = {}, session = {}, fetchImpl, confirmImpl } = {}) {
  const nodes = new Map();
  const document = {
    documentElement: new FakeNode('html'),
    readyState: 'complete',
    getElementById(id) {
      if (!nodes.has(id)) nodes.set(id, new FakeNode(id === 'accountList' || id === 'upstreamList' || id === 'mappingList' ? 'div' : 'input'));
      return nodes.get(id);
    },
    createElement(tag) { return new FakeNode(tag); },
    querySelector(selector) {
      if (selector === 'meta[name="cpa-api-base"]') return { content: '/v0/management/open-design' };
      return null;
    },
  };
  const localStorage = storage(persistent);
  const sessionStorage = storage(session);
  const calls = [];
  const fetchMock = async (url, init = {}) => {
    calls.push({ url: String(url), init });
    const result = fetchImpl ? await fetchImpl(String(url), init) : { status: 200, body: {} };
    const status = result.status ?? 200;
    const body = result.body;
    const raw = typeof body === 'string' ? body : JSON.stringify(body ?? {});
    return { ok: status >= 200 && status < 300, status, text: async () => raw };
  };
  const context = vm.createContext({
    location: new URL(url),
    navigator: { userAgent: 'OpenDesign UI test' },
    document,
    localStorage,
    sessionStorage,
    URL,
    URLSearchParams,
    AbortSignal,
    Uint8Array,
    TextEncoder,
    TextDecoder,
    atob,
    fetch: fetchMock,
    console,
    confirm: confirmImpl || (() => true),
    setTimeout,
    clearTimeout,
    __OPEN_DESIGN_NO_BOOTSTRAP__: true,
  });
  context.window = context;
  context.globalThis = context;
  for (const file of order) vm.runInContext(source(file), context, { filename: file });
  return { context, nodes, localStorage, sessionStorage, calls, document, fetch: fetchMock };
}

const key = 'fixture-open-design-key-123456';
const managementKey = 'fixture-management-key';
const statusBody = (overrides = {}) => ({
  version: '0.1.0',
  base_url: 'https://amr-link.open-design.ai/v1',
  credentials: [{ id: 'cred-1', label: '主账户', disabled: false }],
  models: [{ id: 'od-flash', upstream_id: 'open-design/flash' }],
  ...overrides,
});

function respond(status, body) {
  return { status, body };
}

test('page is self-contained, uses no Cline branding and renders safely with textContent', () => {
  const page = pageHTML();
  assert.ok(!page.includes('/*__OPEN_DESIGN_'));
  assert.ok(page.includes('OpenDesign Bridge'));
  assert.ok(page.includes('OpenDesign Cloud'));
  assert.ok(!/cline/i.test(page), 'the console must not reuse Cline branding');
  // Quota display is implemented against /tmp/open-design-quota-contract.md. The
  // only permitted money value is the "≈ $N" equivalent derived from the
  // operator-supplied credits-per-dollar rate; token rates, per-token lists and
  // "N days left" claims must never appear.
  assert.ok(!/\$\s*\d+\s*(per|\/)\s*(1k|1m|million|token)|还能用\s*\d+\s*天/i.test(page),
    'unimplemented per-token rates and "N days left" claims must not appear');
  assert.ok(!/<script[^>]+src=/i.test(page), 'no external scripts');
  assert.ok(!/<img/i.test(page), 'no logos or remote images');
  assert.ok(!source('console.js').includes('.innerHTML'), 'console rendering must not assign HTML');
  assert.ok(!source('console.js').includes('insertAdjacentHTML'));
  assert.ok(!source('core.js').includes('innerHTML'));
});

test('management URL resolution works for both console routes and ignores foreign bases', () => {
  for (const url of [
    'https://cpa.example/v0/resource/plugins/open-design/console',
    'https://cpa.example/team/cpa/v0/resource/plugins/open-design/console/',
    'https://cpa.example/v8/resource/plugins/open-design/console',
    'https://cpa.example/v0/management/open-design/console.html',
    'https://cpa.example/team/cpa/v8/management/open-design/console.html',
  ]) {
    const view = fakePage({ url });
    const endpoint = view.context.OpenDesignAuth.endpoint();
    assert.ok(endpoint, url);
    assert.equal(endpoint.origin, 'https://cpa.example');
    assert.equal(endpoint.apiPath, `${endpoint.root}/v0/management/open-design`);
  }
  const foreign = fakePage({ url: 'https://cpa.example/custom-console' });
  assert.deepEqual({ ...foreign.context.OpenDesignAuth.endpoint() }, {
    origin: 'https://cpa.example',
    root: '',
    apiPath: '/v0/management/open-design',
  });
  const external = fakePage({ url: 'https://other.example/v0/resource/plugins/open-design/console' });
  assert.equal(external.context.OpenDesignAuth.endpoint().origin, 'https://other.example');
});

test('CPA saved login is reused only for the same origin and deployment path', () => {
  const saved = key => ({ state: { apiBase: 'https://cpa.example', managementKey: key, rememberPassword: true } });
  const same = fakePage({ persistent: { isLoggedIn: 'true', 'cli-proxy-auth': JSON.stringify(saved(managementKey)) } });
  assert.deepEqual(JSON.parse(JSON.stringify(same.context.OpenDesignAuth.resolveHeaders())), {
    headers: { Authorization: `Bearer ${managementKey}` },
    source: 'cpa-login',
  });
  for (const [origin, apiBase, loggedIn, remember] of [
    ['https://other.example', 'https://cpa.example', 'true', true],
    ['https://cpa.example', 'https://cpa.example/other', 'true', true],
    ['https://cpa.example', 'https://cpa.example', 'false', true],
    ['https://cpa.example', 'https://cpa.example', 'true', false],
  ]) {
    const view = fakePage({
      url: `${origin}/v0/resource/plugins/open-design/console`,
      persistent: { isLoggedIn: loggedIn, 'cli-proxy-auth': JSON.stringify({ state: { apiBase, managementKey, rememberPassword: remember } }) },
    });
    assert.deepEqual(JSON.parse(JSON.stringify(view.context.OpenDesignAuth.resolveHeaders())), { headers: {}, source: '' });
  }
});

test('stored management keys are endpoint scoped and can be cleared by rejection', () => {
  const first = fakePage({ url: 'https://cpa.example/team/v0/resource/plugins/open-design/console' });
  assert.equal(first.context.OpenDesignAuth.saveAuth(managementKey, 'x-management-key', true), true);
  assert.equal(first.localStorage.keys().length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(first.context.OpenDesignAuth.resolveHeaders())), {
    headers: { 'X-Management-Key': managementKey },
    source: 'session',
  });
  const sibling = fakePage({ url: 'https://cpa.example/other/v0/resource/plugins/open-design/console', persistent: Object.fromEntries(first.localStorage.map) });
  assert.deepEqual(JSON.parse(JSON.stringify(sibling.context.OpenDesignAuth.resolveHeaders())), { headers: {}, source: '' });
  first.context.OpenDesignAuth.clearAuth({ 'X-Management-Key': 'different-key' });
  assert.equal(first.sessionStorage.keys().length, 1, 'a late rejection for another key must not clear the current one');
  first.context.OpenDesignAuth.clearAuth({ 'X-Management-Key': managementKey });
  assert.equal(first.sessionStorage.keys().length, 0);
  assert.equal(first.localStorage.keys().length, 0);
});

test('management API sends auth in headers, never in URLs, and parses both error shapes', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      if (url.endsWith('/status')) return respond(200, statusBody());
      if (url.includes('/upstream-models')) return respond(200, { data: [{ id: 'open-design/flash', owned_by: 'opendesign' }] });
      if (url.includes('/credentials?') || url.endsWith('/credentials')) return respond(400, { error: { message: '凭据不存在', code: 'missing' } });
      if (url.includes('/models')) return respond(500, { error: '上游保存失败' });
      return respond(200, { models: [] });
    },
  });
  const client = view.context.OpenDesignAPI.createClient({
    base: '/team/v0/management/open-design',
    resolveAuth: () => ({ headers: { Authorization: `Bearer ${managementKey}` } }),
    fetchImpl: view.fetch,
  });
  assert.equal((await client.status()).version, '0.1.0');
  assert.deepEqual(calls[0].url, '/team/v0/management/open-design/status');
  assert.equal(calls[0].init.headers.Authorization, `Bearer ${managementKey}`);
  assert.ok(!calls.some(call => call.url.includes(managementKey)), 'management key must not appear in a URL');
  await client.upstreamModels('cred/1');
  assert.match(calls[1].url, /\/upstream-models\?credential_id=cred%2F1$/);
  await assert.rejects(client.deleteCredential('bad'), /HTTP 400：凭据不存在/);
  const removal = calls[2];
  assert.equal(removal.init.method, 'DELETE');
  assert.equal(removal.url, '/team/v0/management/open-design/credentials?id=bad');
  assert.ok(!removal.url.includes('/credentials/bad'), 'credential ids must not be path segments');
  await assert.rejects(client.saveModels([]), /HTTP 500：上游保存失败/);
  assert.ok(!calls.some(call => call.url.includes(managementKey)));
});

test('adding an account never persists or echoes the OpenDesign API key', async () => {
  const seen = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      seen.push({ url, init });
      if (url.endsWith('/status')) return respond(200, statusBody());
      if (url.endsWith('/credentials')) return respond(201, { id: 'cred-2', label: '新账户', enabled: true });
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.state.status = view.context.OpenDesignCore.normalizeStatus(statusBody());
  view.document.getElementById('accountLabel').value = '<img src=x onerror=alert(1)>';
  view.document.getElementById('accountKey').value = key;
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  const add = seen.find(call => call.url.endsWith('/credentials'));
  assert.deepEqual(JSON.parse(add.init.body), { label: '<img src=x onerror=alert(1)>', api_key: key });
  assert.equal(view.document.getElementById('accountKey').value, '');
  assert.equal(view.localStorage.keys().length, 0);
  assert.equal(view.sessionStorage.keys().length, 0);
  assert.ok(!JSON.stringify(consoleApi.state).includes(key), 'the API key must not remain in page state');
  assert.ok(!seen.some(call => call.url.includes(key)), 'the API key must not be placed in a URL');
});

test('account labels are rendered as text, never as trusted HTML', () => {
  const view = fakePage();
  const consoleApi = view.context.OpenDesignConsole;
  const label = '<img src=x onerror="globalThis.pwned=1"><b>bold</b>';
  consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label, disabled: true }] }));
  const list = view.document.getElementById('accountList');
  const row = list.children[0];
  assert.equal(row.children[0].children[0].textContent, label);
  assert.equal(row.children[0].children[1].textContent, '已停用');
  assert.ok(!Array.from(row.children).some(child => child.tagName === 'IMG' || child.tagName === 'B'));
  assert.equal(view.context.pwned, undefined);
});


const plain = value => JSON.parse(JSON.stringify(value));

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function slowModelPage() {
  const pending = [];
  const view = fakePage({
    fetchImpl: (url) => {
      if (url.endsWith('/status')) return respond(200, statusBody({
        credentials: [
          { id: 'cred-1', label: '主账户', disabled: false },
          { id: 'cred-2', label: '备用账户', disabled: false },
        ],
      }));
      const request = deferred();
      pending.push({ url, request });
      return request.promise;
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.state.status = view.context.OpenDesignCore.normalizeStatus(statusBody({
    credentials: [
      { id: 'cred-1', label: '主账户', disabled: false },
      { id: 'cred-2', label: '备用账户', disabled: false },
    ],
  }));
  consoleApi.state.mappings = [];
  view.document.getElementById('modelCredential').replaceChildren();
  consoleApi.render.renderCredentialOptions();
  return { view, consoleApi, pending };
}

test('row action buttons are wired to real click listeners', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      calls.push({ url, method: init.method || 'GET', body: init.body });
      if (url.endsWith('/status')) return respond(200, statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false }] }));
      if (url.includes('/credentials/cred-1')) return respond(200, { id: 'cred-1', label: '主账户', disabled: true });
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.state.status = view.context.OpenDesignCore.normalizeStatus(statusBody());
  consoleApi.render.renderAccounts();
  const row = view.document.getElementById('accountList').children[0];
  const actions = Array.from(row.children[1].children);
  const toggle = actions.find(node => node.getAttribute('data-action') === 'toggle-account');
  const remove = actions.find(node => node.getAttribute('data-action') === 'delete-account');
  assert.ok(toggle && remove);
  await toggle.emit('click');
  const update = calls.find(call => call.url.endsWith('/credentials') && call.method === 'POST');
  assert.ok(update, 'toggle button must issue the credential update request');
  assert.equal(update.method, 'POST');
  assert.deepEqual(JSON.parse(update.body), { disabled: true, id: 'cred-1' });
  const before = calls.length;
  await remove.emit('click');
  assert.ok(calls.length > before, 'delete button must issue a request');
});


test('a slow model list for account A never overwrites the list for account B', async () => {
  const { view, consoleApi, pending } = slowModelPage();
  consoleApi.handlers.selectCredential('cred-1');
  const first = consoleApi.handlers.fetchModels();
  await new Promise(setImmediate);
  assert.equal(pending.length, 1);
  assert.equal(pending[0].url.includes('credential_id=cred-1'), true);
  assert.equal(consoleApi.state.busy, true);
  assert.equal(view.document.getElementById('modelCredential').disabled, true, 'picker is disabled while a fetch is in flight');

  // The operator switches to B (also reachable programmatically, bypassing the
  // disabled control) and starts a second request.
  consoleApi.handlers.selectCredential('cred-2');
  assert.equal(consoleApi.state.busy, false, 'switching selection releases the old request slot');
  const second = consoleApi.handlers.fetchModels();
  await new Promise(setImmediate);
  assert.equal(pending.length, 2);
  assert.equal(pending[1].url.includes('credential_id=cred-2'), true);

  // B answers first, then the late A response arrives.
  pending[1].request.resolve(respond(200, { data: [{ id: 'open-design/b-model' }] }));
  await second;
  assert.equal(consoleApi.state.upstreamFor, 'cred-2');
  assert.deepEqual(plain(consoleApi.state.upstream.map(item => item.id)), ['open-design/b-model']);

  pending[0].request.resolve(respond(200, { data: [{ id: 'open-design/a-model' }] }));
  await first;
  assert.equal(consoleApi.state.upstreamFor, 'cred-2', 'late A response must not retarget the list');
  assert.deepEqual(plain(consoleApi.state.upstream.map(item => item.id)), ['open-design/b-model']);
  const rendered = [...view.document.getElementById('upstreamList').children].map(row => row.textContent).join(' ');
  assert.ok(rendered.includes('open-design/b-model'));
  assert.ok(!rendered.includes('open-design/a-model'), 'A models must not be shown under B');
  assert.equal(consoleApi.state.busy, false);
});

test('a late A success does not clobber a newer A request either', async () => {
  const { consoleApi, pending } = slowModelPage();
  consoleApi.handlers.selectCredential('cred-1');
  const stale = consoleApi.handlers.fetchModels();
  await new Promise(setImmediate);
  // Re-selecting the same credential still invalidates the older generation.
  consoleApi.handlers.selectCredential('cred-1');
  const fresh = consoleApi.handlers.fetchModels();
  await new Promise(setImmediate);
  assert.equal(pending.length, 2);
  pending[0].request.resolve(respond(200, { data: [{ id: 'stale-model' }] }));
  await stale;
  assert.deepEqual(plain(consoleApi.state.upstream), [], 'stale generation result is discarded');
  pending[1].request.resolve(respond(200, { data: [{ id: 'fresh-model' }] }));
  await fresh;
  assert.deepEqual(plain(consoleApi.state.upstream.map(item => item.id)), ['fresh-model']);
});

test('a late A failure never shows an error under account B', async () => {
  const { view, consoleApi, pending } = slowModelPage();
  consoleApi.handlers.selectCredential('cred-1');
  const first = consoleApi.handlers.fetchModels();
  await new Promise(setImmediate);
  consoleApi.handlers.selectCredential('cred-2');
  const second = consoleApi.handlers.fetchModels();
  await new Promise(setImmediate);
  pending[1].request.resolve(respond(200, { data: [{ id: 'open-design/b-model' }] }));
  await second;
  const settled = view.document.getElementById('modelState').textContent;
  assert.match(settled, /1 个上游模型/);

  pending[0].request.resolve(respond(500, { error: { message: 'A 账户上游失败' } }));
  await first;
  assert.equal(view.document.getElementById('modelState').textContent, settled, 'late failure must not replace B status');
  assert.ok(!view.document.getElementById('modelState').textContent.includes('A 账户上游失败'));
  assert.deepEqual(plain(consoleApi.state.upstream.map(item => item.id)), ['open-design/b-model']);
  assert.equal(consoleApi.state.busy, false);
});

test('logging out, deleting or losing the selected account invalidates an in-flight fetch', async () => {
  const invalidations = {
    logout: (consoleApi) => consoleApi.handlers.clearAuth(),
    // A delete cannot run while the model list owns the busy slot; the row button
    // is disabled too. The credential then disappears through a status refresh.
    deleteRow: async (consoleApi, view) => {
      const before = consoleApi.state.selectedCredential;
      await consoleApi.handlers.deleteAccount({ id: before, label: '主账户', disabled: false });
      assert.equal(consoleApi.state.selectedCredential, before, 'delete is refused while a fetch is in flight');
      consoleApi.render.applyStatus(statusBody({
        credentials: [{ id: 'cred-2', label: '备用账户', disabled: false }],
      }));
      view.document.getElementById('modelCredential').replaceChildren();
      consoleApi.render.renderCredentialOptions();
    },
  };
  for (const [name, invalidate] of Object.entries(invalidations)) {
    const { view, consoleApi, pending } = slowModelPage();
    consoleApi.handlers.selectCredential('cred-1');
    const flight = consoleApi.handlers.fetchModels();
    await new Promise(setImmediate);
    assert.equal(pending.length, 1);
    await invalidate(consoleApi, view);
    assert.equal(consoleApi.state.busy, false, `${name}: pending slot released`);
    pending[0].request.resolve(respond(200, { data: [{ id: 'open-design/stale' }] }));
    await flight;
    assert.deepEqual(plain(consoleApi.state.upstream), [], `${name}: stale success discarded`);
    assert.equal(consoleApi.state.upstreamFor, '');
    assert.ok(!view.document.getElementById('upstreamList').textContent.includes('open-design/stale'));
  }
});

test('the console validates management keys before storing and handles 401 feedback', async () => {
  const view = fakePage({ fetchImpl: async () => respond(401, { error: { message: 'invalid management key' } }) });
  const consoleApi = view.context.OpenDesignConsole;
  const keyInput = view.document.getElementById('managementKey');
  keyInput.value = 'wrong-key';
  await consoleApi.handlers.submitAuth({ preventDefault() {} });
  assert.match(view.document.getElementById('authState').textContent, /401/);
  assert.ok(!view.document.getElementById('authState').textContent.includes('wrong-key'));
  assert.equal(view.sessionStorage.keys().length, 0, 'unverified keys must not be stored');
  assert.equal(view.localStorage.keys().length, 0);
  assert.equal(view.document.getElementById('accountsPanel').hidden, true);

  view.fetch = async (url) => ({ ok: url.endsWith('/status'), status: 200, text: async () => JSON.stringify(statusBody()) });
  // No candidate credential is ever stored on the page: a typed key is scoped to
  // its own verification request, so nothing has to be dropped here.
  assert.equal(consoleApi.state.candidate, undefined);
  const login = fakePage({ fetchImpl: async (url) => respond(200, url.endsWith('/status') ? statusBody() : {}) });
  const loginApi = login.context.OpenDesignConsole;
  login.document.getElementById('managementKey').value = managementKey;
  login.document.getElementById('rememberAuth').checked = true;
  await loginApi.handlers.submitAuth({ preventDefault() {} });
  assert.equal(login.document.getElementById('managementKey').value, '');
  assert.equal(login.sessionStorage.keys().length, 1);
  assert.equal(login.localStorage.keys().length, 1);
  assert.equal(login.document.getElementById('accountsPanel').hidden, false);
  assert.equal(login.document.getElementById('accountList').children.length, 1);
});

test('model mapping validates aliases, deduplicates, and derives a safe payload', () => {
  const core = fakePage().context.OpenDesignCore;
  const credentialView = (overrides = {}) => ({
    id: 'cred-1', label: '主账户', disabled: false, billing_configured: false,
    billing_revision: '', workspace_id: '', credits_per_usd: '',
    // The quota pause is a separate, plugin-owned flag: `disabled` stays the
    // operator's own choice and the two are never conflated.
    quota_paused: false, effective_disabled: false, quota_pause: null,
    ...overrides,
  });
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.normalizeStatus(statusBody()).credentials)),
    [credentialView()],
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.normalizeStatus(statusBody({
      credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
    })).credentials)),
    [credentialView({ billing_configured: true, workspace_id: 'ws-1' })],
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.normalizeUpstreamModels({ data: [{ id: 'a' }, { id: 'a' }, { id: 'b', owned_by: 'x' }, null, {}] }))),
    [{ id: 'a', owned_by: '' }, { id: 'b', owned_by: 'x' }],
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.buildModelsPayload([{ id: ' alias ', upstream_id: ' a ' }, { id: 'pro', upstream_id: 'b' }]))),
    [{ id: 'alias', upstream_id: 'a' }, { id: 'pro', upstream_id: 'b' }],
  );
  assert.throws(() => core.buildModelsPayload([{ id: 'alias', upstream_id: '' }]), /不能为空/);
  assert.throws(() => core.buildModelsPayload([{ id: 'a', upstream_id: 'x' }, { id: 'a', upstream_id: 'y' }]), /重复/);
  // The save body keeps the old shape for an absent token and only adds the CAS
  // field when the server advertised one.
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.buildModelsSaveBody([{ id: ' alias ', upstream_id: ' a ' }]))),
    { models: [{ id: 'alias', upstream_id: 'a' }] },
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.buildModelsSaveBody([{ id: 'alias', upstream_id: 'a' }], 'rev-7'))),
    { models: [{ id: 'alias', upstream_id: 'a' }], expected_models_revision: 'rev-7' },
  );
  assert.equal(core.normalizeStatus(statusBody({ models_revision: 'rev-7' })).models_revision, 'rev-7',
    'the whole-list revision is preserved from status');
  assert.equal(core.normalizeStatus(statusBody()).models_revision, '', 'a missing token stays empty');
  assert.equal(core.aliasForUpstream('m'), 'open-design-m');
  assert.deepEqual(JSON.parse(JSON.stringify(core.addUpstream([], 'm'))), [{ id: 'open-design-m', upstream_id: 'm' }]);
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.addUpstream([{ id: 'custom', upstream_id: 'm' }], 'm'))),
    [{ id: 'custom', upstream_id: 'm' }],
    'an existing alias for the upstream model is kept as-is',
  );
  assert.deepEqual(JSON.parse(JSON.stringify(core.removeUpstream([{ id: 'a', upstream_id: 'm' }], 'm'))), []);
});

test('fetching models and saving mappings follow the documented contract', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      if (url.includes('/upstream-models')) return respond(200, { data: [{ id: 'open-design/flash' }, { id: 'open-design/pro' }] });
      if (url.endsWith('/models')) return respond(200, { models: [{ id: 'flash', upstream_id: 'open-design/flash' }] });
      if (url.endsWith('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { 'X-Management-Key': managementKey }, source: 'manual' };
  consoleApi.state.selectedCredential = 'cred-1';
  view.document.getElementById('modelCredential').value = 'cred-1';
  await consoleApi.handlers.fetchModels();
  assert.match(calls[0].url, /\/upstream-models\?credential_id=cred-1$/);
  assert.equal(view.document.getElementById('upstreamList').children.length, 2);
  assert.match(view.document.getElementById('modelState').textContent, /2 个上游模型/);

  consoleApi.state.mappings = [
    { id: 'flash', upstream_id: 'open-design/flash' },
    { id: 'pro', upstream_id: 'open-design/pro' },
  ];
  await consoleApi.handlers.saveModels();
  const save = calls.find(call => call.url.endsWith('/models'));
  assert.deepEqual(JSON.parse(save.init.body), {
    models: [
      { id: 'flash', upstream_id: 'open-design/flash' },
      { id: 'pro', upstream_id: 'open-design/pro' },
    ],
  });
  assert.match(view.document.getElementById('modelState').textContent, /已保存 1 个模型映射/);
  assert.equal(view.document.getElementById('mappingList').children.length, 1);
});

test('401 while adding or loading models explains the failure without exposing secrets', async () => {
  const view = fakePage({ fetchImpl: async (url) => respond(url.endsWith('/status') ? 200 : 401, { error: { message: 'invalid_api_key' } }) });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.state.selectedCredential = 'cred-1';
  view.document.getElementById('modelCredential').value = 'cred-1';
  await consoleApi.handlers.fetchModels();
  assert.match(view.document.getElementById('modelState').textContent, /401/);
  assert.ok(!view.document.getElementById('modelState').textContent.includes(managementKey));

  view.document.getElementById('accountKey').value = key;
  view.fetch = async (url) => respond(url.endsWith('/status') ? 200 : 401, { error: 'invalid management key' });
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  assert.match(view.document.getElementById('authState').textContent, /401/);
  assert.ok(!view.document.getElementById('authState').textContent.includes(key));
});

// --- account editing, draft protection, upstream-vs-management errors ---

function twoAccountPage(extra = {}) {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init, method: init.method || 'GET', body: init.body });
      return extra.fetchImpl ? extra.fetchImpl(url, init, calls) : respond(200, {});
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  const accounts = [
    { id: 'cred-1', label: '主账户', disabled: false },
    { id: 'cred-2', label: '备用账户', disabled: false },
  ];
  consoleApi.render.applyStatus(statusBody({
    credentials: accounts,
    models: [{ id: 'flash', upstream_id: 'open-design/flash' }],
  }));
  return { view, consoleApi, calls, accounts };
}

const accountRows = view => [...view.document.getElementById('accountList').children];
const actionOf = (row, action) => Array.from(row.children[1].children).find(node => node.getAttribute('data-action') === action);

test('editing an account reuses POST /credentials and omits the key when left empty', async () => {
  const updated = statusBody({
    credentials: [{ id: 'cred-1', label: '主账户（改名）', disabled: false }],
    models: [{ id: 'flash', upstream_id: 'open-design/flash' }],
  });
  const { view, consoleApi, calls } = twoAccountPage({
    fetchImpl: (url, init) => {
      if (url.endsWith('/credentials') && (init.method || 'GET') === 'POST') return respond(200, { id: 'cred-1', label: '主账户（改名）', disabled: false });
      if (url.endsWith('/status')) return respond(200, updated);
      return respond(404, { error: 'not found' });
    },
  });
  const row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'cred-1');
  await actionOf(row, 'edit-account').emit('click');
  assert.match(view.document.getElementById('accountFormTitle').textContent, /编辑账户/);
  assert.equal(view.document.getElementById('accountSubmit').textContent, '保存修改');
  assert.equal(view.document.getElementById('accountCancel').hidden, false);
  assert.equal(view.document.getElementById('accountLabel').value, '主账户');
  assert.equal(view.document.getElementById('accountKey').value, '', 'the original key is never read into the page');
  assert.match(view.document.getElementById('accountKey').getAttribute('placeholder'), /留空/);

  view.document.getElementById('accountLabel').value = '主账户（改名）';
  view.document.getElementById('accountKey').value = '';
  await consoleApi.handlers.addAccount({ preventDefault() {} });

  const update = calls.find(call => call.method === 'POST' && call.url.endsWith('/credentials'));
  const body = JSON.parse(update.body);
  assert.equal(body.id, 'cred-1');
  assert.equal(body.label, '主账户（改名）');
  assert.ok(!('api_key' in body), 'an empty key must not be sent so the stored key is kept');
  assert.equal(calls.some(call => call.url.includes(managementKey)), false);
  assert.equal(view.document.getElementById('accountKey').value, '');
  assert.equal(view.document.getElementById('accountFormTitle').textContent, '添加账户');
  assert.equal(view.document.getElementById('accountCancel').hidden, true);
  assert.match(view.document.getElementById('accountState').textContent, /原 API key 保留/);
});

test('editing with a replacement key sends api_key once and then clears the input', async () => {
  const { view, consoleApi, calls } = twoAccountPage({
    fetchImpl: (url, init) => {
      if (url.endsWith('/credentials') && (init.method || 'GET') === 'POST') return respond(200, { id: 'cred-1', label: '主账户', disabled: false });
      if (url.endsWith('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'cred-1');
  await actionOf(row, 'edit-account').emit('click');
  view.document.getElementById('accountKey').value = 'replacement-key-abcdef';
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  const update = calls.find(call => call.method === 'POST' && call.url.endsWith('/credentials'));
  const body = JSON.parse(update.body);
  assert.equal(body.api_key, 'replacement-key-abcdef');
  assert.equal(view.document.getElementById('accountKey').value, '');
  assert.equal(view.sessionStorage.keys().length, 0, 'an account key is never persisted');
  assert.ok(!JSON.stringify(consoleApi.state).includes('replacement-key-abcdef'), 'the key must not stay in page state');
});

test('cancelling an edit clears the key input and editing ends when the account disappears', async () => {
  const { view, consoleApi } = twoAccountPage();
  let row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'cred-1');
  await actionOf(row, 'edit-account').emit('click');
  view.document.getElementById('accountKey').value = 'half-typed-key';
  consoleApi.handlers.cancelAccountEdit();
  assert.equal(view.document.getElementById('accountKey').value, '');
  assert.equal(consoleApi.state.accountEdit, null);
  assert.equal(view.document.getElementById('accountCancel').hidden, true);
  assert.match(view.document.getElementById('accountState').textContent, /已取消编辑/);

  row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'cred-1');
  await actionOf(row, 'edit-account').emit('click');
  assert.ok(consoleApi.state.accountEdit);
  // A refresh that no longer lists the edited account (for example deleted in
  // another tab) must cancel the edit and drop any typed key.
  view.document.getElementById('accountKey').value = 'stale-typed-key';
  consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-2', label: '备用账户', disabled: false }] }));
  assert.equal(consoleApi.state.accountEdit, null);
  assert.equal(view.document.getElementById('accountKey').value, '');
  assert.match(view.document.getElementById('accountState').textContent, /已取消编辑/);
});

test('clearing management auth also clears account form inputs and selection', async () => {
  const { view, consoleApi } = twoAccountPage();
  const row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'cred-1');
  await actionOf(row, 'edit-account').emit('click');
  view.document.getElementById('accountLabel').value = '暂存标签';
  view.document.getElementById('accountKey').value = 'typed-secret-key';
  consoleApi.state.selectedCredential = 'cred-1';
  consoleApi.handlers.clearAuth();
  assert.equal(view.document.getElementById('accountKey').value, '');
  assert.equal(view.document.getElementById('accountLabel').value, '');
  assert.equal(consoleApi.state.accountEdit, null);
  assert.equal(consoleApi.state.selectedCredential, '');
  assert.equal(consoleApi.state.verified, null);
  assert.equal(view.sessionStorage.keys().length, 0);
});

test('a status refresh keeps unsaved mapping drafts and flags them', () => {
  const { view, consoleApi } = twoAccountPage();
  assert.equal(consoleApi.mappingsDirty(), false);
  // Simulate typing in the alias field of the first mapping row.
  const firstRow = view.document.getElementById('mappingList').children[0];
  const alias = firstRow.children[0].children[1];
  alias.value = 'flash-draft';
  alias.emit('input');
  assert.equal(consoleApi.mappingsDirty(), true);
  assert.equal(view.document.getElementById('mappingDirty').hidden, false);
  assert.equal(view.document.getElementById('revertMappings').hidden, false);

  // Account-side refresh returns a different saved mapping set.
  consoleApi.render.applyStatus(statusBody({
    credentials: [
      { id: 'cred-1', label: '主账户', disabled: false },
      { id: 'cred-2', label: '备用账户', disabled: false },
    ],
    models: [{ id: 'other', upstream_id: 'open-design/other' }],
  }));
  assert.deepEqual(plain(consoleApi.state.mappings.map(row => row.id)), ['flash-draft'], 'the draft survives a refresh');
  assert.deepEqual(plain(consoleApi.state.savedMappings.map(row => row.id)), ['other'], 'the saved baseline still follows the server');
  assert.match(view.document.getElementById('modelState').textContent, /草稿已保留/);
  assert.equal(view.document.getElementById('mappingDirty').hidden, false);

  consoleApi.handlers.revertMappings();
  assert.deepEqual(plain(consoleApi.state.mappings.map(row => row.id)), ['other']);
  assert.equal(consoleApi.mappingsDirty(), false);
  assert.equal(view.document.getElementById('mappingDirty').hidden, true);
});

test('a clean status refresh still replaces the mapping list', () => {
  const { consoleApi } = twoAccountPage();
  consoleApi.render.applyStatus(statusBody({ models: [{ id: 'fresh', upstream_id: 'open-design/fresh' }] }));
  assert.deepEqual(plain(consoleApi.state.mappings.map(row => row.id)), ['fresh']);
  assert.equal(consoleApi.mappingsDirty(), false);
});

async function loadedModels() {
  const { view, consoleApi, calls } = twoAccountPage({
    fetchImpl: async (url) => {
      if (url.includes('/upstream-models')) {
        return respond(200, {
          data: [
            { id: 'alpha-1', owned_by: 'vela' },
            { id: 'beta-2', owned_by: 'vela' },
            { id: 'gamma-3', owned_by: 'other' },
          ],
        });
      }
      return respond(404, { error: 'not found' });
    },
  });
  view.document.getElementById('modelCredential').value = 'cred-1';
  consoleApi.state.mappings = [];
  await consoleApi.handlers.fetchModels();
  return { view, consoleApi, calls };
}

test('upstream search filters the rendered list and all-visible selection adds default aliases', async () => {
  const { view, consoleApi } = await loadedModels();
  const list = () => view.document.getElementById('upstreamList');
  assert.equal(list().children.length, 3);
  assert.match(view.document.getElementById('upstreamCount').textContent, /3 个上游模型/);

  consoleApi.handlers.filterUpstream('beta');
  assert.equal(list().children.length, 1);
  assert.match(list().children[0].textContent, /beta-2/);
  assert.match(view.document.getElementById('upstreamCount').textContent, /显示 1 \/ 3/);
  assert.equal(consoleApi.state.mappings.length, 0, 'filtering must not change the draft');

  consoleApi.handlers.selectVisibleUpstream(true);
  assert.deepEqual(plain(consoleApi.state.mappings), [{ id: 'open-design-beta-2', upstream_id: 'beta-2' }]);
  assert.match(view.document.getElementById('modelState').textContent, /已勾选当前可见的 1 个/);
  assert.equal(view.document.getElementById('mappingDirty').hidden, false);

  consoleApi.handlers.filterUpstream('');
  consoleApi.handlers.selectVisibleUpstream(true);
  assert.deepEqual(plain(consoleApi.state.mappings.map(row => row.id)), [
    'open-design-beta-2',
    'open-design-alpha-1',
    'open-design-gamma-3',
  ], 'existing rows are kept and visible models are appended');

  consoleApi.handlers.filterUpstream('gamma');
  consoleApi.handlers.selectVisibleUpstream(false);
  assert.deepEqual(plain(consoleApi.state.mappings.map(row => row.id)), ['open-design-beta-2', 'open-design-alpha-1']);
  assert.match(view.document.getElementById('modelState').textContent, /已取消当前可见的 1 个/);
});

test('an existing alias for an upstream model is preserved when selecting it again', async () => {
  const { view, consoleApi } = await loadedModels();
  consoleApi.state.mappings = [{ id: 'custom-alias', upstream_id: 'alpha-1' }];
  consoleApi.render.renderModels();
  consoleApi.handlers.selectVisibleUpstream(true);
  const kept = consoleApi.state.mappings.find(row => row.upstream_id === 'alpha-1');
  assert.equal(kept.id, 'custom-alias');
  assert.equal(view.document.getElementById('mappingList').children.length, 3);
});

test('model coverage is explicit, model-scoped, safe, and separate from mapping selection', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      if (url.includes('/upstream-models')) return respond(200, { data: [{ id: 'open-design/alpha/1' }] });
      if (url.includes('/model-coverage')) return respond(200, {
        credential_id: 'cred-1', model_id: 'open-design/alpha/1', billing_configured: true,
        workspace_id: 'ws-1', state: 'available', fetched_at: '2026-11-03T00:00:00Z',
        next_refresh_at: '2026-11-03T00:01:00Z', model_covered: true,
        funding_check: 'available', funding: 'coding_plan', error: null,
      });
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.state.selectedCredential = 'cred-1';
  view.document.getElementById('modelCredential').value = 'cred-1';
  await consoleApi.handlers.fetchModels();
  assert.equal(calls.filter(call => call.url.includes('/model-coverage')).length, 0, 'loading models must not preflight them');

  const before = view.document.getElementById('upstreamList').children[0];
  assert.equal(before.tagName, 'DIV', 'coverage action must not sit inside a checkbox label');
  const beforeCheck = before.children[0];
  const checkButton = Array.from(before.children).find(node => node.getAttribute('data-action') === 'check-model-coverage');
  assert.ok(checkButton);
  assert.equal(beforeCheck.checked, false);
  await checkButton.emit('click', { preventDefault() {}, stopPropagation() {} });

  const coverageCall = calls.find(call => call.url.includes('/model-coverage'));
  assert.ok(coverageCall);
  assert.match(coverageCall.url, /credential_id=cred-1/);
  assert.match(coverageCall.url, /model_id=open-design%2Falpha%2F1/);
  assert.match(coverageCall.url, /refresh=1/);
  const row = view.document.getElementById('upstreamList').children[0];
  assert.equal(row.children[0].checked, false, 'coverage click must not toggle mapping selection');
  assert.match(row.textContent, /套餐覆盖/);
  assert.match(row.textContent, /官方预检：可用/);
  assert.ok(!row.textContent.includes('ws-1'), 'workspace identifiers are not displayed in the model row');
});
test('switching model accounts fences a late coverage response and releases its busy state', async () => {
  let coverageStartedResolve;
  let releaseCoverage;
  const coverageStarted = new Promise(resolve => { coverageStartedResolve = resolve; });
  const coverageResponse = new Promise(resolve => { releaseCoverage = () => resolve(respond(200, {
    credential_id: 'cred-1', model_id: 'alpha-1', state: 'available', model_covered: true,
    funding_check: 'available', funding: 'coding_plan', fetched_at: '2026-11-03T00:00:00Z',
  })); });
  const { view, consoleApi } = twoAccountPage({
    fetchImpl: async (url) => {
      if (url.includes('/upstream-models')) return respond(200, { data: [{ id: 'alpha-1' }] });
      if (url.includes('/model-coverage')) {
        coverageStartedResolve();
        return coverageResponse;
      }
      return respond(404, { error: 'not found' });
    },
  });
  consoleApi.state.selectedCredential = 'cred-1';
  view.document.getElementById('modelCredential').value = 'cred-1';
  await consoleApi.handlers.fetchModels();
  const button = Array.from(view.document.getElementById('upstreamList').children[0].children)
    .find(node => node.getAttribute('data-action') === 'check-model-coverage');
  const pending = button.emit('click', { preventDefault() {}, stopPropagation() {} });
  await coverageStarted;
  assert.equal(Object.keys(consoleApi.state.modelCoverageBusy).length, 1);

  consoleApi.handlers.selectCredential('cred-2');
  assert.deepEqual(Object.keys(consoleApi.state.modelCoverageBusy), []);
  releaseCoverage();
  await pending;
  assert.deepEqual(Object.keys(consoleApi.state.modelCoverage), [], 'late account A data must be discarded');
});
test('a source=upstream 401/403 keeps management auth and blames the account key', async () => {
  for (const [status, code, expected] of [
    [401, 'invalid_api_key', /OpenDesign Key 被上游拒绝（invalid_api_key）/],
    [403, 'insufficient_scope', /上游拒绝读取该账户的模型列表（403）（insufficient_scope）/],
  ]) {
    const { view, consoleApi } = twoAccountPage({
      fetchImpl: async (url) => {
        if (url.includes('/upstream-models')) {
          return respond(status, { error: { message: 'upstream rejected the key', source: 'upstream', code } });
        }
        return respond(404, { error: 'not found' });
      },
    });
    view.document.getElementById('modelCredential').value = 'cred-1';
    view.document.getElementById('authBadge').textContent = '已验证';
    await consoleApi.handlers.fetchModels();
    const state = view.document.getElementById('modelState').textContent;
    assert.match(state, expected, `status ${status}`);
    assert.match(state, /管理认证仍有效/);
    assert.ok(!state.includes('管理密钥无效'), 'an upstream rejection must not blame the management key');
    assert.equal(view.document.getElementById('authBadge').textContent, '已验证');
    assert.equal(view.document.getElementById('accountsPanel').hidden, false);
    assert.ok(consoleApi.state.verified, 'management auth survives an upstream rejection');
    assert.equal(view.context.OpenDesignAuth.resolveHeaders().source, '', 'no stored management key was cleared');
  }
});

test('a management 401 without source (old backend) keeps the previous combined wording', async () => {
  const { view, consoleApi } = twoAccountPage({
    fetchImpl: async (url) => {
      if (url.includes('/upstream-models')) return respond(401, { error: 'invalid management key' });
      return respond(404, { error: 'not found' });
    },
  });
  view.document.getElementById('modelCredential').value = 'cred-1';
  await consoleApi.handlers.fetchModels();
  // Old backends cannot say who rejected the key, so the original message (check
  // management key first, then the account key) is preserved unchanged.
  assert.match(view.document.getElementById('modelState').textContent, /管理认证失效，或该 OpenDesign API key 无效/);
  assert.ok(!view.document.getElementById('modelState').textContent.includes('管理认证仍有效'));
  assert.ok(consoleApi.state.verified, 'fetchModels never tears down the page on its own');
});

test('a source that is not exactly upstream is never blamed on the account key', async () => {
  const { view, consoleApi } = twoAccountPage({
    fetchImpl: async (url) => {
      if (url.includes('/upstream-models')) {
        return respond(401, { error: { message: 'management key rejected', source: 'management', code: 'unauthorized' } });
      }
      return respond(404, { error: 'not found' });
    },
  });
  view.document.getElementById('modelCredential').value = 'cred-1';
  await consoleApi.handlers.fetchModels();
  const text = view.document.getElementById('modelState').textContent;
  assert.ok(!text.includes('OpenDesign Key 被上游拒绝'), 'only source=upstream may blame the account key');
  assert.match(text, /管理认证失效/);
});

test('APIError keeps source and code for new backends and stays empty for old ones', () => {
  const api = fakePage().context.OpenDesignAPI;
  const nested = new api.APIError(401, 'x', { error: { message: 'bad key', source: 'upstream', code: 'invalid_api_key' } });
  assert.equal(nested.source, 'upstream');
  assert.equal(nested.code, 'invalid_api_key');
  const topLevel = new api.APIError(403, 'x', { error: 'denied', source: 'upstream', code: 'scope' });
  assert.equal(topLevel.source, 'upstream');
  assert.equal(topLevel.code, 'scope');
  const legacy = new api.APIError(401, 'x', { error: 'invalid management key' });
  assert.equal(legacy.source, '');
  assert.equal(legacy.code, '');
  const garbage = new api.APIError(401, 'x', { error: { source: 42, code: { nested: true } } });
  assert.equal(garbage.source, '');
  assert.equal(garbage.code, '');
});

test('a mapping save that persisted but failed the host refresh reports the partial state', async () => {
  const { view, consoleApi } = twoAccountPage({
    fetchImpl: async (url) => {
      if (url.endsWith('/models')) {
        return respond(502, { error: 'model mapping saved but the host model refresh failed: host save rejected' });
      }
      return respond(404, { error: 'not found' });
    },
  });
  await consoleApi.handlers.saveModels();
  const text = view.document.getElementById('modelState').textContent;
  assert.match(text, /已保存/);
  assert.match(text, /宿主刷新模型目录失败/);
  assert.match(text, /刷新状态/);
  assert.equal(view.document.getElementById('mappingDirty').hidden, true, 'the persisted mapping is not a draft any more');
  assert.deepEqual(plain(consoleApi.state.savedMappings.map(row => row.id)), ['flash']);
});

test('a delete that disabled the credential but failed the unlink reports the partial state', async () => {
  const calls = [];
  const { view, consoleApi } = twoAccountPage({
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, method: init.method || 'GET' });
      if ((init.method || 'GET') === 'DELETE') {
        return respond(500, { error: 'credential removal failed: permission denied (the credential is now disabled and its models are withdrawn; retry the delete)' });
      }
      if (url.endsWith('/status')) {
        return respond(200, statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false }] }));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'cred-1');
  await actionOf(row, 'delete-account').emit('click');
  const text = view.document.getElementById('accountState').textContent;
  assert.match(text, /删除未完成/);
  assert.match(text, /已停用且模型已撤下/);
  assert.match(text, /可重试删除/);
  assert.ok(calls.some(call => call.method === 'GET' && call.url.endsWith('/status')), 'the page refreshes to show the partial state');
});

test('「获取模型」 on a row performs the fetch, and a disabled account cannot fetch', async () => {
  const calls = [];
  const { view, consoleApi } = twoAccountPage({
    fetchImpl: async (url) => {
      calls.push(url);
      if (url.includes('/upstream-models')) return respond(200, { data: [{ id: 'open-design/flash' }] });
      return respond(404, { error: 'not found' });
    },
  });
  const row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'cred-1');
  const fetchButton = actionOf(row, 'fetch-account');
  assert.match(fetchButton.className, /\bbutton\b/, 'row buttons use the shared button style');
  assert.match(fetchButton.className, /\bcompact\b/);
  assert.equal(actionOf(row, 'delete-account').className.includes('button'), true);
  await fetchButton.emit('click');
  assert.ok(calls.some(url => url.includes('/upstream-models?credential_id=cred-1')));
  assert.equal(consoleApi.state.upstreamFor, 'cred-1');

  consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: true }] }));
  const disabledRow = accountRows(view)[0];
  assert.equal(actionOf(disabledRow, 'fetch-account').disabled, true, 'a disabled account cannot pull models');
  const before = calls.length;
  await actionOf(disabledRow, 'fetch-account').emit('click');
  assert.equal(calls.length, before, 'no request is sent for a disabled account');
});

// --- optional quota (billing) UI ---

const quotaContract = overrides => ({
  credential_id: 'cred-1',
  billing_configured: true,
  workspace_id: 'ws-1',
  state: 'available',
  fetched_at: '2026-11-03T00:00:00Z',
  next_refresh_at: '2026-11-03T00:01:00Z',
  wallet: {
    state: 'available',
    fetched_at: '2026-11-03T00:00:00Z',
    data: { balanceUsd: '12.345', updatedAt: null },
    error: null,
  },
  funding: {
    state: 'available',
    fetched_at: '2026-11-03T00:00:00Z',
    data: {
      balanceUsd: '12.345',
      funding: 'coding_plan',
      modelCovered: null,
      fundingCheck: 'available',
      codingPlan: {
        tier: 'plus',
        windows: [{
          policyId: 'p',
          durationSeconds: 18000,
          resetMode: 'anchored_recurring',
          usedCredits: '200',
          limitCredits: '1000',
          remainingCredits: '800',
          windowStart: '2026-11-03T00:00:00Z',
          resetsAt: '2026-11-03T05:00:00Z',
          forecast: { state: 'insufficient_samples' },
        }],
      },
    },
    error: null,
  },
  ...overrides,
});

function quotaPage(fetchImpl, credentials) {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init, method: init.method || 'GET', body: init.body });
      return fetchImpl(url, init);
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.render.applyStatus(statusBody({
    credentials: credentials || [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
    models: [{ id: 'flash', upstream_id: 'open-design/flash' }],
  }));
  return { view, consoleApi, calls };
}

const quotaRow = view => view.document.getElementById('accountList').children[0];
const quotaText = view => quotaRow(view).children[2].textContent;
const flattenNodes = node => [node, ...Array.from(node.children || []).flatMap(flattenNodes)];
const quotaButtons = view => flattenNodes(quotaRow(view).children[2])
  .filter(node => node.tagName === 'BUTTON');

test('loading status never requests quota; only the explicit refresh button does', async () => {
  const { view, consoleApi, calls } = quotaPage(async url => {
    if (url.endsWith('/status')) return respond(200, statusBody({
      credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
    }));
    if (url.includes('/quota')) return respond(200, quotaContract());
    return respond(404, { error: 'not found' });
  });
  await consoleApi.render.loadStatus({ source: 'manual' });
  assert.equal(calls.filter(call => call.url.includes('/quota')).length, 0, 'a status load must not hit the quota route');
  assert.match(quotaText(view), /尚未查询/);
  assert.match(quotaText(view), /刷新额度/);

  // Opening the credentials tab refreshes the accounts, one at a time.
  consoleApi.handlers.activateTab('credentials');
  await new Promise(resolve => setTimeout(resolve, 0));
  const quotaCalls = calls.filter(call => call.url.includes('/quota'));
  assert.equal(quotaCalls.length, 1, 'one account means exactly one quota call');
  assert.match(quotaCalls[0].url, /\/quota\?credential_id=cred-1&refresh=1$/);
  assert.equal(quotaCalls[0].method, 'GET');

  // The explicit button still works after the automatic pass.
  const refresh = quotaButtons(view).find(node => node.getAttribute('data-action') === 'refresh-quota');
  assert.ok(refresh);
  await refresh.emit('click');
  assert.equal(calls.filter(call => call.url.includes('/quota')).length, 2);
});

test('opening the credentials tab walks accounts serially and skips the unconfigured ones', async () => {
  let inFlight = 0;
  let peak = 0;
  const order = [];
  const credentials = [
    { id: 'cred-a', label: 'A', disabled: false, billing_configured: true, workspace_id: 'ws-a' },
    { id: 'cred-b', label: 'B', disabled: false, billing_configured: false, workspace_id: '' },
    { id: 'cred-c', label: 'C', disabled: false, billing_configured: true, workspace_id: 'ws-c' },
    { id: 'cred-d', label: 'D', disabled: true, billing_configured: true, workspace_id: 'ws-d' },
  ];
  const { consoleApi, calls } = quotaPage(async url => {
    if (url.includes('/quota')) {
      inFlight += 1;
      peak = Math.max(peak, inFlight);
      const id = new URL(url, 'https://cpa.example').searchParams.get('credential_id');
      order.push(id);
      await new Promise(resolve => setTimeout(resolve, 5));
      inFlight -= 1;
      return respond(200, quotaContract({ credential_id: id }));
    }
    return respond(404, { error: 'not found' });
  }, credentials);
  consoleApi.handlers.activateTab('credentials');
  await new Promise(resolve => setTimeout(resolve, 60));
  assert.deepEqual(order, ['cred-a', 'cred-c'], 'configured, enabled accounts are queried in list order');
  assert.equal(peak, 1, 'accounts are queried one at a time, never concurrently');
  assert.equal(calls.filter(call => call.url.includes('/quota')).length, 2);
});

test('switching away from the credentials tab stops the serial walk', async () => {
  let release = () => {};
  const gate = new Promise(resolve => { release = resolve; });
  const order = [];
  const credentials = [
    { id: 'cred-a', label: 'A', disabled: false, billing_configured: true, workspace_id: 'ws-a' },
    { id: 'cred-b', label: 'B', disabled: false, billing_configured: true, workspace_id: 'ws-b' },
  ];
  const { consoleApi } = quotaPage(async url => {
    if (url.includes('/quota')) {
      const id = new URL(url, 'https://cpa.example').searchParams.get('credential_id');
      order.push(id);
      await gate;
      return respond(200, quotaContract({ credential_id: id }));
    }
    return respond(404, { error: 'not found' });
  }, credentials);
  consoleApi.handlers.activateTab('credentials');
  await new Promise(resolve => setTimeout(resolve, 0));
  consoleApi.handlers.activateTab('overview');
  release();
  await new Promise(resolve => setTimeout(resolve, 20));
  assert.deepEqual(order, ['cred-a'], 'the walk stops after leaving the tab');
});

test('an available quota snapshot renders balance, credits windows, meters and reset times', async () => {
  const { view, consoleApi } = quotaPage(async url => {
    if (url.includes('/quota')) return respond(200, quotaContract({
      funding: {
        state: 'available',
        fetched_at: '2026-11-03T00:00:00Z',
        data: {
          balanceUsd: '12.345',
          funding: 'coding_plan',
          modelCovered: null,
          fundingCheck: 'available',
          codingPlan: {
            tier: 'plus',
            windows: [{
              policyId: 'p',
              durationSeconds: 18000,
              resetMode: 'anchored_recurring',
              usedCredits: '9007199254740993',
              limitCredits: '9007199254740994',
              remainingCredits: '1',
              windowStart: '2026-11-03T00:00:00Z',
              resetsAt: new Date(Date.now() + 5 * 3600000).toISOString(),
              forecast: { state: 'estimated', seconds_to_exhaust: 3600, exhausts_at: new Date(Date.now() + 3600000).toISOString(), basis: 'recent_consumption' },
            }],
          },
        },
        error: null,
      },
    }));
    return respond(404, { error: 'not found' });
  });
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  const text = quotaText(view);
  assert.ok(text.includes('$12.345'), 'wallet USD balance is shown verbatim');
  assert.match(text, /查询时间/);
  assert.match(text, /套餐：plus/);
  assert.match(text, /9,007,199,254,740,993/, 'large credits are formatted without Number rounding');
  assert.match(text, /剩余 credits/);
  assert.match(text, /99\.99%/, 'the native meter uses exact BigInt basis points');
  assert.match(text, /重置于/);
  assert.match(text, /按该窗口最近整体 credits 消耗估算/);
  assert.match(text, /仅为估算/);
  assert.ok(!/还能用\s*\d+\s*天/.test(text), 'no "N days left" promise');
});

test('not configured, never queried, first failure and stale snapshots are distinct and never fake 0', async () => {
  // not configured
  const plain = quotaPage(async () => respond(404, { error: 'x' }), [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: false, workspace_id: '' }]);
  assert.match(quotaText(plain.view), /额度查询未配置/);
  assert.ok(!quotaText(plain.view).includes('$0'));
  const plainRefresh = quotaButtons(plain.view).find(node => node.getAttribute('data-action') === 'refresh-quota');
  assert.equal(plainRefresh.disabled, true, 'without a control key the refresh button is disabled');

  // first failure: error + null data
  const failed = quotaPage(async () => respond(200, quotaContract({
    state: 'error',
    fetched_at: null,
    wallet: { state: 'error', fetched_at: null, data: null, error: { kind: 'unauthorized', message: '额度授权无效' } },
    funding: { state: 'not_requested', fetched_at: null, data: null, error: null },
  })));
  await failed.consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  const failedText = quotaText(failed.view);
  assert.match(failedText, /首次查询失败/);
  assert.match(failedText, /unauthorized/);
  assert.ok(!failedText.includes('$0'), 'a failure must not be rendered as a zero balance');
  assert.ok(!failedText.includes('管理密钥无效'), 'a quota authorization failure never blames the management key');
  assert.ok(failed.consoleApi.state.verified, 'management auth survives a quota failure');

  // stale: keeps last data but drops the old prediction
  const stale = quotaPage(async () => respond(200, quotaContract({
    state: 'stale',
    wallet: { state: 'stale', fetched_at: '2026-11-02T00:00:00Z', data: { balanceUsd: '3.5', updatedAt: null }, error: { kind: 'timeout', message: '上游超时' } },
    funding: {
      state: 'stale',
      fetched_at: '2026-11-02T00:00:00Z',
      data: {
        balanceUsd: '3.5',
        funding: 'coding_plan',
        modelCovered: null,
        codingPlan: {
          tier: 'pro',
          windows: [{
            policyId: 'p', durationSeconds: 18000, resetMode: 'anchored_recurring',
            usedCredits: '10', limitCredits: '100', remainingCredits: '90',
            windowStart: '2026-11-02T00:00:00Z', resetsAt: '2026-11-02T05:00:00Z',
            forecast: { state: 'estimated', seconds_to_exhaust: 60, exhausts_at: '2026-11-02T01:00:00Z' },
          }],
        },
      },
      error: { kind: 'timeout', message: '上游超时' },
    },
  })));
  await stale.consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  const staleText = quotaText(stale.view);
  assert.match(staleText, /\$3\.5/, 'last successful balance stays visible');
  assert.match(staleText, /数据可能过时/);
  assert.match(staleText, /上次成功查询/);
  assert.ok(!staleText.includes('按该窗口最近整体 credits 消耗估算'), 'stale snapshots must not keep showing the old prediction');
});

test('partial success shows the available half and the failing half separately', async () => {
  const { view, consoleApi } = quotaPage(async () => respond(200, quotaContract({
    state: 'partial',
    wallet: { state: 'available', fetched_at: '2026-11-03T00:00:00Z', data: { balanceUsd: '7', updatedAt: null }, error: null },
    funding: { state: 'error', fetched_at: null, data: null, error: { kind: 'forbidden', message: '无权限' } },
  })));
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  const text = quotaText(view);
  assert.match(text, /\$7/);
  assert.match(text, /部分可用/);
  assert.match(text, /首次查询失败（forbidden）/);
});

test('quota editors are optional, keep secrets out of the page and never auto-query', async () => {
  const { view, consoleApi, calls } = quotaPage(async url => {
    if (url.endsWith('/credentials') && (url.includes('credentials'))) return respond(200, { id: 'cred-1' });
    if (url.endsWith('/status')) return respond(200, statusBody({
      credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
    }));
    return respond(404, { error: 'not found' });
  });
  const row = quotaRow(view);
  await Array.from(row.children[1].children).find(node => node.getAttribute('data-action') === 'edit-account').emit('click');
  // The control key is never read back into the page.
  assert.equal(view.document.getElementById('accountControlKey').value, '');
  assert.equal(view.document.getElementById('accountWorkspaceID').value, 'ws-1');
  assert.match(view.document.getElementById('accountControlKey').getAttribute('placeholder'), /留空则保留/);
  // The form itself advertises the official control key, not the inference key.
  assert.match(html, /可选：额度查询（官方 control key）/);
  assert.match(html, /官方 controlKey/);
  assert.match(html, /同一 OpenDesign 账号的额度授权/);
  assert.match(html, /清除已保存的额度授权/);

  view.document.getElementById('accountControlKey').value = 'control-key-secret';
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  const update = calls.find(call => call.method === 'POST' && call.url.endsWith('/credentials'));
  const body = JSON.parse(update.body);
  assert.equal(body.control_key, 'control-key-secret');
  assert.equal(body.workspace_id, 'ws-1');
  assert.ok(!('clear_billing' in body), 'leaving the field empty never clears billing');
  assert.ok(!('api_key' in body), 'the inference key is untouched');
  assert.equal(view.document.getElementById('accountControlKey').value, '');
  assert.equal(view.document.getElementById('accountWorkspaceID').value, '');
  assert.ok(!calls.some(call => call.url.includes('/quota')), 'saving credentials must not query upstream quota');
  assert.ok(!JSON.stringify(consoleApi.state).includes('control-key-secret'));
});

test('clear_billing is only sent when the operator explicitly checks it', async () => {
  const { view, consoleApi, calls } = quotaPage(async url => {
    if (url.endsWith('/credentials')) return respond(200, { id: 'cred-1' });
    if (url.endsWith('/status')) return respond(200, statusBody({
      credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: false, workspace_id: '' }],
    }));
    return respond(404, { error: 'not found' });
  });
  const row = quotaRow(view);
  await Array.from(row.children[1].children).find(node => node.getAttribute('data-action') === 'edit-account').emit('click');
  view.document.getElementById('accountControlKey').value = '';
  view.document.getElementById('accountWorkspaceID').value = '';
  view.document.getElementById('accountClearBilling').checked = true;
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  const update = calls.find(call => call.method === 'POST' && call.url.endsWith('/credentials'));
  const body = JSON.parse(update.body);
  assert.equal(body.clear_billing, true);
  assert.equal(body.label, '主账户');
  assert.ok(!('control_key' in body));
  assert.ok(!('api_key' in body), 'clearing billing keeps the inference key');
});

test('refresh is generation-guarded: a late response cannot overwrite newer credentials or logout', async () => {
  const pending = [];
  const { view, consoleApi } = quotaPage(async url => {
    if (url.includes('/quota')) {
      const defer = deferred();
      pending.push(defer);
      return defer.promise;
    }
    return respond(404, { error: 'not found' });
  });
  const first = consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  await new Promise(setImmediate);
  assert.equal(consoleApi.state.quotaBusy['cred-1'], true);
  // The account is reconfigured elsewhere (e.g. control key replaced) while the
  // request is in flight.
  consoleApi.render.applyStatus(statusBody({
    credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-2' }],
  }));
  pending[0].resolve(respond(200, quotaContract()));
  await first;
  assert.equal(consoleApi.state.quotas['cred-1'], undefined, 'the stale snapshot is discarded');
  assert.ok(!quotaText(view).includes('$12.345'));
  assert.equal(consoleApi.state.quotaBusy['cred-1'], undefined);

  // A normal refresh stores the snapshot.
  const second = consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-2' });
  await new Promise(setImmediate);
  pending[1].resolve(respond(200, quotaContract({ workspace_id: 'ws-2' })));
  await second;
  assert.equal(consoleApi.state.quotas['cred-1'].workspace_id, 'ws-2');

  // Logout drops every snapshot and invalidates in-flight responses.
  const third = consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-2' });
  await new Promise(setImmediate);
  consoleApi.handlers.clearAuth();
  pending[2].resolve(respond(200, quotaContract({ workspace_id: 'ws-2' })));
  await third;
  assert.deepEqual(plain(Object.keys(consoleApi.state.quotas)), []);
});

test('a throttled refresh keeps the previous snapshot and explains the 60s window', async () => {
  const { view, consoleApi } = quotaPage(async () => respond(200, quotaContract({ state: 'needs_refresh' })));
  consoleApi.state.quotas['cred-1'] = view.context.OpenDesignCore.normalizeQuota(quotaContract({
    wallet: { state: 'available', fetched_at: '2026-11-03T00:00:00Z', data: { balanceUsd: '4', updatedAt: null }, error: null },
  }));
  consoleApi.render.renderAccounts();
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  assert.equal(consoleApi.state.quotas['cred-1'].wallet.data.balanceUsd, '4', 'a throttled answer is not a failure or a fresh success');
  assert.match(quotaText(view), /\$4/);
  assert.match(view.document.getElementById('accountState').textContent, /节流/);
  assert.ok(!view.document.getElementById('accountState').className.includes('error'));
});

test('quota management HTTP 401/403 requires login; upstream rejection arrives in an HTTP 200 component', async () => {
  for (const status of [401, 403]) {
    const { view, consoleApi } = quotaPage(async () => respond(status, { error: 'management request rejected' }));
    await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
    assert.equal(consoleApi.state.verified, null);
    assert.equal(view.document.getElementById('accountsPanel').hidden, true);
    assert.match(view.document.getElementById('authState').textContent, new RegExp(String(status)));
    assert.deepEqual(Object.keys(consoleApi.state.quotas), []);
  }
});

test('workspace missing shows the funding card as not requested instead of a fake zero', async () => {
  const { view, consoleApi } = quotaPage(async () => respond(200, quotaContract({
    workspace_id: '',
    state: 'available',
    funding: { state: 'not_requested', fetched_at: null, data: null, error: null },
  })), [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: '' }]);
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: '' });
  const text = quotaText(view);
  assert.match(text, /未查询（缺 Workspace ID）/);
  assert.match(text, /无法查询套餐窗口/);
  assert.ok(!/0\s*credits/.test(text));
});

test('forecast states map to the documented Chinese descriptions', () => {
  const core = fakePage().context.OpenDesignCore;
  assert.match(core.forecastView({ state: 'insufficient_samples' }).text, /样本不足/);
  assert.match(core.forecastView({ state: 'rolling_window' }).text, /滚动窗口/);
  const estimated = core.forecastView({ state: 'estimated', seconds_to_exhaust: 7200, exhausts_at: '2026-11-03T02:00:00Z' });
  assert.match(estimated.text, /按该窗口最近整体 credits 消耗估算/);
  assert.match(estimated.text, /2 小时/);
  assert.ok(/2026-11-03/.test(estimated.text), 'estimated shows the projected time');
  assert.match(estimated.text, /仅为估算/);
  assert.match(core.forecastView({ state: 'resets_before_exhaustion' }).text, /预计先到窗口重置时间/);
  assert.match(core.forecastView({ state: 'resets_before_exhaustion', seconds_to_exhaust: 86400 }).text, /但会先到窗口重置时间/);
  assert.match(core.forecastView({ state: 'beyond_horizon' }).text, /远超一年/);
  assert.equal(core.forecastView({ state: 'unknown-state' }).text, '');
  assert.ok(!/还能用/.test(estimated.text));
});

test('quota helpers keep nulls unknown and never coerce them to zero', () => {
  const core = fakePage().context.OpenDesignCore;
  const q = core.normalizeQuota({
    state: 'partial',
    billing_configured: true,
    wallet: { state: 'available', data: { balanceUsd: null, updatedAt: null } },
    funding: { state: 'available', data: { codingPlan: { tier: 'plus', windows: [{ usedCredits: '0', limitCredits: '0', remainingCredits: null }] } } },
  });
  assert.equal(q.wallet.data.balanceUsd, '');
  assert.equal(q.funding.data.codingPlan.windows[0].remainingCredits, '');
  assert.equal(q.funding.data.codingPlan.windows[0].usedCredits, '0', 'an explicit 0 is preserved');
  assert.equal(core.creditsPercent('0', '0'), null, 'a zero limit has no percentage');
  assert.equal(core.creditsPercent('abc', '10'), null);
  assert.equal(core.creditsPercent('5', '10').text, '50.00%');
  assert.equal(core.formatUSD(''), '');
  assert.equal(core.formatCredits(''), '');
  assert.equal(core.formatTime('not-a-time'), '');
  assert.equal(core.formatUSD('0'), '$0');
  assert.equal(core.formatUSD('not-a-balance'), '');
  assert.equal(core.formatUSD(9007199254740993), '', 'rounded numeric amounts remain unknown');
  assert.equal(core.formatCredits(9007199254740993), '', 'numeric credits cannot silently lose precision');
  assert.equal(core.formatDuration(5400), '1 小时 30 分钟', 'window lengths must not round to a different hour');
  assert.equal(core.formatDuration(90000), '1 天 1 小时');
  assert.match(core.forecastView({ state: 'exhausted' }).text, /本次采样显示窗口额度已用尽/);
});

test('an elapsed forecast is withdrawn locally without requesting the upstream', async () => {
  const past = new Date(Date.now() - 60000).toISOString();
  const future = new Date(Date.now() + 3600000).toISOString();
  const { consoleApi, view, calls } = quotaPage(async () => respond(200, quotaContract({ funding: {
    state: 'available', fetched_at: past, error: null, data: { funding: 'coding_plan', codingPlan: {
      tier: 'plus', windows: [{ policyId: 'p', durationSeconds: 5400, resetMode: 'anchored_recurring',
        usedCredits: '200', limitCredits: '1000', remainingCredits: '800', resetsAt: future,
        forecast: { state: 'estimated', seconds_to_exhaust: 3600, exhausts_at: past },
      }],
    } },
  } })));
  await consoleApi.handlers.refreshQuota(consoleApi.state.status.credentials[0]);
  assert.match(quotaText(view), /上次估算时间或窗口已过/);
  assert.ok(!quotaText(view).includes('按该窗口最近整体 credits 消耗估算'));
  assert.ok(!quotaText(view).includes('anchored_recurring'));
  assert.match(quotaText(view), /窗口 1 小时 30 分钟/);
  assert.match(quotaText(view), /资金方式：套餐额度/);
  const count = calls.length;
  consoleApi.render.renderAccounts();
  assert.equal(calls.length, count, 'rendering locally never polls the quota endpoint');
});

test('creating an account can save the optional control key, and editing keeps the clear row scoped', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init, method: init.method || 'GET', body: init.body });
      if (url.endsWith('/credentials')) return respond(201, { id: 'cred-9', label: '新账户', disabled: false });
      if (url.endsWith('/status')) return respond(200, statusBody({ credentials: [] }));
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.render.applyStatus(statusBody({ credentials: [], models: [] }));
  assert.equal(view.document.getElementById('accountClearBillingRow').hidden, true, 'clear only appears in edit mode');

  view.document.getElementById('accountLabel').value = '新账户';
  view.document.getElementById('accountKey').value = 'inference-key-123456';
  view.document.getElementById('accountControlKey').value = 'control-key-123456';
  view.document.getElementById('accountWorkspaceID').value = 'ws-42';
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  const create = calls.find(call => call.method === 'POST' && call.url.endsWith('/credentials'));
  const body = JSON.parse(create.body);
  assert.deepEqual(body, {
    label: '新账户',
    api_key: 'inference-key-123456',
    control_key: 'control-key-123456',
    workspace_id: 'ws-42',
  });
  assert.equal(view.document.getElementById('accountControlKey').value, '');
  assert.equal(view.document.getElementById('accountWorkspaceID').value, '');
  assert.equal(view.document.getElementById('billingFields').open, false);

  // Wallet-only access is valid: the workspace is optional.
  view.document.getElementById('accountLabel').value = '仅钱包';
  view.document.getElementById('accountKey').value = 'inference-key-123456';
  view.document.getElementById('accountControlKey').value = 'control-key-123456';
  const before = calls.length;
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  const walletOnly = calls.slice(before).find(call => call.method === 'POST' && call.url.endsWith('/credentials'));
  assert.ok(walletOnly, 'control key alone supports wallet queries');
  assert.equal(JSON.parse(walletOnly.body).control_key, 'control-key-123456');
  assert.ok(!('workspace_id' in JSON.parse(walletOnly.body)));
  assert.equal(view.document.getElementById('accountControlKey').value, '');
});


test('editing one account does not strand a concurrent quota refresh for another account', async () => {
  const pending = new Map();
  const credentials = [
    { id: 'cred-1', label: '一', billing_configured: true, workspace_id: 'ws-1' },
    { id: 'cred-2', label: '二', billing_configured: true, workspace_id: 'ws-2' },
  ];
  const { consoleApi } = quotaPage(async url => {
    const id = new URL(url, 'https://cpa.example').searchParams.get('credential_id');
    const request = deferred();
    pending.set(id, request);
    return request.promise;
  }, credentials);
  const one = consoleApi.handlers.refreshQuota(credentials[0]);
  const two = consoleApi.handlers.refreshQuota(credentials[1]);
  await new Promise(resolve => setImmediate(resolve));
  consoleApi.render.applyStatus(statusBody({ credentials: [
    { ...credentials[0], workspace_id: 'ws-new' }, credentials[1],
  ] }));
  assert.equal(consoleApi.state.quotaBusy['cred-1'], undefined);
  assert.equal(consoleApi.state.quotaBusy['cred-2'], true);
  pending.get('cred-1').resolve(respond(200, quotaContract()));
  pending.get('cred-2').resolve(respond(200, quotaContract({ credential_id: 'cred-2', workspace_id: 'ws-2' })));
  await Promise.all([one, two]);
  assert.equal(consoleApi.state.quotas['cred-1'], undefined, 'obsolete account configuration is discarded');
  assert.equal(consoleApi.state.quotas['cred-2'].workspace_id, 'ws-2', 'unrelated response still applies');
  assert.equal(consoleApi.state.quotaBusy['cred-2'], undefined, 'unrelated button is released');
});

test('transport errors mark the old snapshot stale and suppress its forecast', async () => {
  let request = 0;
  const { consoleApi, view } = quotaPage(async () => {
    if (request++ === 0) return respond(200, quotaContract({ funding: {
      state: 'available', fetched_at: '2026-11-03T00:00:00Z', error: null,
      data: { funding: 'coding_plan', codingPlan: { tier: 'plus', windows: [{
        policyId: 'p', durationSeconds: 18000, resetMode: 'anchored_recurring',
        usedCredits: '200', limitCredits: '1000', remainingCredits: '800',
        windowStart: new Date(Date.now() - 3600000).toISOString(), resetsAt: new Date(Date.now() + 4 * 3600000).toISOString(),
        forecast: { state: 'estimated', seconds_to_exhaust: 3600, exhausts_at: new Date(Date.now() + 3600000).toISOString() },
      }] } },
    } }));
    throw new Error('fixture connection lost');
  });
  const account = consoleApi.state.status.credentials[0];
  await consoleApi.handlers.refreshQuota(account);
  assert.match(quotaText(view), /按该窗口最近整体 credits 消耗估算/);
  await consoleApi.handlers.refreshQuota(account);
  assert.match(quotaText(view), /上次成功查询/);
  assert.ok(!quotaText(view).includes('按该窗口最近整体 credits 消耗估算'));
  assert.equal(consoleApi.state.quotas['cred-1'].wallet.fetched_at, '2026-11-03T00:00:00Z');
});

test('a response for a different credential cannot populate the current quota card', async () => {
  const { consoleApi } = quotaPage(async () => respond(200, quotaContract({ credential_id: 'cred-other' })));
  await consoleApi.handlers.refreshQuota(consoleApi.state.status.credentials[0]);
  assert.equal(consoleApi.state.quotas['cred-1'], undefined);
  assert.equal(consoleApi.state.quotaBusy['cred-1'], undefined);
});

test('an empty mapping can be explicitly saved to withdraw all plugin models', async () => {
  const requests = [];
  const view = fakePage({ fetchImpl: async (url, init) => {
    requests.push({ url, init });
    return respond(200, { models: [] });
  } });
  const api = view.context.OpenDesignConsole;
  api.render.applyStatus(statusBody());
  api.state.mappings = [];
  await api.handlers.saveModels();
  const request = requests.find(item => item.url.endsWith('/models'));
  assert.ok(request);
  assert.deepEqual(JSON.parse(request.init.body), { models: [] });
  assert.equal(api.state.savedMappings.length, 0);
});

test('an opaque credential revision invalidates only the externally replaced account', async () => {
  const credentials = [
    { id: 'cred-1', label: '一', billing_configured: true, workspace_id: 'ws-1', billing_revision: 'revision-1' },
    { id: 'cred-2', label: '二', billing_configured: true, workspace_id: 'ws-2', billing_revision: 'revision-2' },
  ];
  const { consoleApi } = quotaPage(async url => {
    const id = new URL(url, 'https://cpa.example').searchParams.get('credential_id');
    return respond(200, quotaContract({ credential_id: id }));
  }, credentials);
  await consoleApi.handlers.refreshQuota(credentials[0]);
  await consoleApi.handlers.refreshQuota(credentials[1]);
  consoleApi.render.applyStatus(statusBody({ credentials: [
    { ...credentials[0], billing_revision: 'revision-replaced' }, credentials[1],
  ] }));
  assert.equal(consoleApi.state.quotas['cred-1'], undefined, 'same public flags with a new key revision discard the old snapshot');
  assert.ok(consoleApi.state.quotas['cred-2'], 'the other account keeps its current snapshot');
});

test('account keys are cleared even when a sent save request fails', async () => {
  const { consoleApi, view } = twoAccountPage({ fetchImpl: async () => respond(500, { error: 'fixture save failed' }) });
  view.document.getElementById('accountKey').value = 'inference-secret-fixture';
  view.document.getElementById('accountControlKey').value = 'control-secret-fixture';
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  assert.equal(view.document.getElementById('accountKey').value, '');
  assert.equal(view.document.getElementById('accountControlKey').value, '');
  assert.match(view.document.getElementById('accountState').textContent, /服务器结果尚未确认/);
  assert.match(view.document.getElementById('accountState').textContent, /先刷新状态确认后再试/);
  assert.equal(consoleApi.state.busy, false);
});

test('acknowledged account changes remain visible when the following status read fails', async () => {
  for (const action of ['create', 'edit', 'delete', 'toggle']) {
    const { consoleApi, view, calls } = twoAccountPage({ fetchImpl: async (url, init) => {
      if (url.endsWith('/status')) throw new Error('fixture status connection lost');
      if (init.method === 'DELETE') return respond(200, { deleted: true });
      const body = JSON.parse(init.body || '{}');
      return respond(action === 'create' ? 201 : 200, {
        id: body.id || 'cred-created', label: body.label || '主账户', disabled: body.disabled === true,
      });
    } });
    const item = consoleApi.state.status.credentials[0];
    if (action === 'create' || action === 'edit') {
      if (action === 'edit') consoleApi.handlers.editAccount(item);
      view.document.getElementById('accountLabel').value = '已保存标签';
      view.document.getElementById('accountKey').value = 'fixture-replacement-secret';
      await consoleApi.handlers.addAccount({ preventDefault() {} });
      assert.match(view.document.getElementById('accountState').textContent, /账户已保存，但读取最新状态失败/);
      assert.ok(consoleApi.state.status.credentials.some(c => c.label === '已保存标签'));
      assert.equal(view.document.getElementById('accountKey').value, '');
      assert.equal(calls.filter(call => call.method === 'POST').length, 1);
    } else if (action === 'delete') {
      await consoleApi.handlers.deleteAccount(item);
      assert.match(view.document.getElementById('accountState').textContent, /账户已删除，但读取最新状态失败/);
      assert.ok(!consoleApi.state.status.credentials.some(c => c.id === item.id));
    } else {
      await consoleApi.handlers.toggleAccount(item);
      assert.match(view.document.getElementById('accountState').textContent, /账户已停用，但读取最新状态失败/);
      assert.equal(consoleApi.state.status.credentials.find(c => c.id === item.id).disabled, true);
    }
    assert.equal(consoleApi.state.busy, false);
  }
});

test('five tabs switch with hidden and aria state only, including keyboard navigation', () => {
  const view = fakePage();
  const consoleApi = view.context.OpenDesignConsole;
  const before = view.calls.length;
  const tabs = [
    ['overview', 'tabOverview', 'overviewPanel'],
    ['credentials', 'tabCredentials', 'credentialsPanel'],
    ['models', 'tabModels', 'modelsTab'],
    ['logs', 'tabLogs', 'logsPanel'],
    ['config', 'tabConfig', 'configPanel'],
  ];
  for (const [name, button, panel] of tabs) {
    consoleApi.handlers.activateTab(name);
    assert.equal(consoleApi.state.activeTab, name);
    for (const [candidate, candidateButton, candidatePanel] of tabs) {
      const selected = candidate === name;
      assert.equal(view.document.getElementById(candidateButton).getAttribute('aria-selected'), selected ? 'true' : 'false');
      assert.equal(view.document.getElementById(candidateButton).getAttribute('tabindex'), selected ? '0' : '-1');
      assert.equal(view.document.getElementById(candidatePanel).hidden, !selected);
    }
    assert.equal(view.document.getElementById(panel).hidden, false);
    assert.equal(view.document.getElementById(button).getAttribute('aria-selected'), 'true');
  }
  let prevented = false;
  consoleApi.handlers.handleTabKey({ key: 'ArrowRight', preventDefault() { prevented = true; } });
  assert.equal(consoleApi.state.activeTab, 'overview', 'End-to-start keyboard navigation wraps across the tablist');
  assert.equal(prevented, true);
  assert.equal(view.calls.length, before, 'tab changes and rendering never fetch optional data');
});

test('request logs and stats are explicit, paged, and clearing also clears the visible stats', async () => {
  let clearCalls = 0;
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.includes('/request-stats')) return respond(200, {
        total: 1, success: 1, failed: 0, streams: 0, success_rate: 1,
        tokens: { input: 2, output: 3, total: 5, requests_with_usage: 1, requests_without_usage: 0 },
        models: [{ model: 'open-design-flash', total: 1, success: 1, failed: 0 }],
      });
      if (url.includes('/request-logs') && init.method === 'DELETE') {
        clearCalls += 1;
        return respond(200, { cleared: true });
      }
      if (url.includes('/request-logs')) return respond(200, {
        logs: [{ id: 'r1', model: 'open-design-flash', status: 'success', stream: false, duration_ms: 12 }],
        total: 2, limit: 1, offset: 0, has_more: true, next_offset: 1,
        retention: { enabled: true, retention_days: 7, max_entries: 500, entries: 2 },
      });
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  await consoleApi.handlers.loadRequestLogs({ reset: true });
  await consoleApi.handlers.loadRequestStats();
  assert.equal(consoleApi.state.requestLogs.length, 1);
  assert.equal(consoleApi.state.logPage.has_more, true);
  assert.equal(consoleApi.state.requestStats.total, 1);
  assert.ok(view.calls.some(call => call.url.includes('limit=50')));
  await consoleApi.handlers.clearRequestLogs();
  assert.equal(clearCalls, 1);
  assert.equal(consoleApi.state.requestLogs.length, 0);
  assert.equal(consoleApi.state.requestStats, null);
  assert.equal(consoleApi.state.logPage.total, 0);
});

test('a failed request-log page clears stale pagination offsets and disables both directions', async () => {
  const view = fakePage({
    fetchImpl: async url => url.includes('/request-logs')
      ? respond(503, { error: 'temporary failure' })
      : respond(404, { error: 'not found' }),
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.logQuery = { offset: 50, limit: 50, status: '', model: '', credentialID: '' };
  consoleApi.state.requestLogs = [{ id: 'old-page' }];
  consoleApi.state.logPage = { loaded: true, total: 99, offset: 50, limit: 50, has_more: true, next_offset: 100 };
  await consoleApi.handlers.loadRequestLogs();
  assert.ok(view.calls.some(call => call.url.includes('offset=50')));
  assert.equal(consoleApi.state.logQuery.offset, 0);
  assert.equal(consoleApi.state.logPage.offset, 0);
  assert.equal(consoleApi.state.logPage.total, 0);
  assert.equal(consoleApi.state.logPage.has_more, false);
  assert.equal(view.document.getElementById('logsPrev').disabled, true);
  assert.equal(view.document.getElementById('logsNext').disabled, true);
  assert.match(view.document.getElementById('logsPageInfo').textContent, /显示 0-0 \/ 共 0 条/);
});

test('config and runtime are loaded only by explicit actions and preserve nullable host facts', async () => {
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/config') && init.method === 'POST') return respond(200, {
        logs: { enabled: false, retention_days: 9, max_entries: 12, entries: 0 },
        upstream: { origin: 'https://amr-link.open-design.ai/v1', fixed: true },
        proxy: { owner: 'CPA', read_only: true, mode: 'host_managed' },
        routing: { owner: 'CPA', read_only: true, mode: 'host_managed' },
      });
      if (url.endsWith('/config')) return respond(200, {
        logs: { enabled: true, retention_days: 7, max_entries: 500, entries: 0 },
        upstream: { origin: 'https://amr-link.open-design.ai/v1', fixed: true },
        proxy: { owner: 'CPA', read_only: true, mode: 'host_managed' },
        routing: { owner: 'CPA', read_only: true, mode: 'host_managed' },
      });
      if (url.endsWith('/runtime')) return respond(200, {
        observed_at: '2026-11-03T00:00:00Z', loaded: true, stopped: false,
        active_requests: 0, pending_operations: 0, model_count: 1,
        host: { available: true },
        credentials: [
          { id: 'cred-null', label: '未知宿主', disabled: false, model_count: 0, host: null },
          { id: 'cred-state', label: '状态宿主', disabled: false, model_count: 1, host: {
            disabled: null, unavailable: null, status: 'cooldown', status_message: '等待宿主重试', next_retry_after: '2026-11-03T00:05:00Z',
          } },
        ],
      });
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  const before = view.calls.length;
  consoleApi.handlers.activateTab('config');
  consoleApi.handlers.activateTab('credentials');
  assert.equal(view.calls.length, before, 'tab navigation does not load config or runtime');
  await consoleApi.handlers.loadConfig();
  assert.equal(consoleApi.state.config.logs.retention_days, 7);
  view.document.getElementById('configLogsEnabled').checked = false;
  view.document.getElementById('configRetentionDays').value = '9';
  view.document.getElementById('configMaxEntries').value = '12';
  await consoleApi.handlers.saveConfig({ preventDefault() {} });
  const save = view.calls.find(call => call.url.endsWith('/config') && call.init.method === 'POST');
  assert.deepEqual(JSON.parse(save.init.body), { logs: { enabled: false, retention_days: 9, max_entries: 12 } });
  await consoleApi.handlers.loadRuntime();
  const runtimeText = view.document.getElementById('runtimeCredentials').textContent;
  assert.match(runtimeText, /宿主状态未知/);
  assert.match(runtimeText, /状态 cooldown/);
  assert.match(runtimeText, /等待宿主重试/);
  assert.match(runtimeText, /下次重试/);
});

test('model test requires confirmation and sends exactly one real request only after confirmation', async () => {
  let confirmations = 0;
  const view = fakePage({
    confirmImpl: () => {
      confirmations += 1;
      return confirmations > 1;
    },
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/model-test')) return respond(200, {
        credential_id: 'cred-1', model_id: 'flash', upstream_model: 'open-design/flash',
        status: 'success', http_status: 200, duration_ms: 8,
        usage: { prompt_tokens: 2, completion_tokens: 3, total_tokens: 5 },
        real_inference: true, streaming: false, request_kind: 'chat.completions',
      });
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false }] }));
  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'flash';
  view.document.getElementById('modelTestPrompt').value = 'Reply with OK.';
  await consoleApi.handlers.runModelTest();
  assert.equal(confirmations, 1);
  assert.equal(view.calls.length, 0, 'declining confirmation sends no request');
  await consoleApi.handlers.runModelTest();
  assert.equal(confirmations, 2);
  assert.equal(view.calls.length, 1);
  assert.equal(JSON.parse(view.calls[0].init.body).model_id, 'flash');
  assert.equal(consoleApi.state.modelTest.usage.total_tokens, 5);
});

test('model test result is discarded when its selection changes while the request is running', async () => {
  const pending = deferred();
  const view = fakePage({
    fetchImpl: async url => url.includes('/model-test') ? pending.promise : respond(404, { error: 'not found' }),
    confirmImpl: () => true,
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.render.applyStatus(statusBody({ credentials: [
    { id: 'cred-1', label: '主账户', disabled: false },
    { id: 'cred-2', label: '备用账户', disabled: false },
  ] }));
  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'flash';
  view.document.getElementById('modelTestPrompt').value = 'Reply with OK.';
  const flight = consoleApi.handlers.runModelTest();
  await new Promise(setImmediate);
  assert.equal(view.document.getElementById('modelTestCredential').disabled, true);
  assert.equal(view.document.getElementById('modelTestModel').disabled, true);
  assert.equal(view.document.getElementById('modelTestPrompt').disabled, true);
  assert.equal(view.document.getElementById('runModelTest').disabled, true);

  // A browser cannot change a disabled select, but a stale event or automation
  // can still mutate the value. The response must be fenced by the selection.
  view.document.getElementById('modelTestCredential').value = 'cred-2';
  pending.resolve(respond(200, {
    credential_id: 'cred-1', model_id: 'flash', status: 'success', http_status: 200,
    usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
  }));
  await flight;
  assert.equal(consoleApi.state.modelTest, null, 'a result for the old selection is not displayed');
  assert.equal(view.document.getElementById('modelTestCredential').disabled, false);
  assert.equal(view.document.getElementById('modelTestModel').disabled, false);
  assert.equal(view.document.getElementById('modelTestPrompt').disabled, false);
  assert.equal(view.document.getElementById('runModelTest').disabled, false);
});

test('model test discards a response whose credential or model identity does not match the request', async () => {
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async url => url.endsWith('/model-test')
      ? respond(200, {
        credential_id: 'cred-other', model_id: 'different-model', status: 'success', http_status: 200,
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      })
      : respond(404, { error: 'not found' }),
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false }] }));
  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'flash';
  await consoleApi.handlers.runModelTest();
  assert.equal(consoleApi.state.modelTest, null);
  assert.match(view.document.getElementById('modelTestState').textContent, /响应与当前账户或模型不一致/);
});

test('status refresh invalidates a running model test and replaces the sending message', async () => {
  const pending = deferred();
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async url => url.endsWith('/model-test') ? pending.promise : respond(404, { error: 'not found' }),
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false }] }));
  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'flash';
  const flight = consoleApi.handlers.runModelTest();
  await new Promise(setImmediate);
  assert.match(view.document.getElementById('modelTestState').textContent, /正在发送模型测试/);
  consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label: '主账户（刷新后）', disabled: false }] }));
  assert.match(view.document.getElementById('modelTestState').textContent, /正在发送的模型测试结果已失效/);
  pending.resolve(respond(200, { credential_id: 'cred-1', model_id: 'flash', status: 'success', usage: {} }));
  await flight;
  assert.equal(consoleApi.state.modelTest, null);
});

test('config or runtime busy state preserves log pagination boundaries', async () => {
  const pending = deferred();
  const view = fakePage({
    fetchImpl: async url => url.endsWith('/config') ? pending.promise : respond(404, { error: 'not found' }),
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.logPage = { loaded: true, total: 0, offset: 0, limit: 50, has_more: false, next_offset: null };
  consoleApi.render.renderRequestLogs();
  assert.equal(view.document.getElementById('logsPrev').disabled, true);
  assert.equal(view.document.getElementById('logsNext').disabled, true);
  const flight = consoleApi.handlers.loadConfig();
  await new Promise(setImmediate);
  assert.equal(view.document.getElementById('logsPrev').disabled, true);
  assert.equal(view.document.getElementById('logsNext').disabled, true);
  pending.resolve(respond(200, { logs: { enabled: true, retention_days: 7, max_entries: 500 } }));
  await flight;
  consoleApi.state.logPage = { loaded: true, total: 51, offset: 50, limit: 50, has_more: true, next_offset: null };
  consoleApi.render.renderRequestLogs();
  const runtimeFlight = consoleApi.handlers.loadRuntime();
  await new Promise(setImmediate);
  assert.equal(view.document.getElementById('logsPrev').disabled, false);
  assert.equal(view.document.getElementById('logsNext').disabled, false);
  runtimeFlight.catch(() => {});
});

test('logout clears all management data, invalidates optional responses, and restores controls for a new session', async () => {
  const cases = [
    {
      name: 'logs',
      match: '/request-logs',
      invoke: api => api.handlers.loadRequestLogs({ reset: true }),
      response: { logs: [], total: 0, limit: 50, offset: 0, has_more: false },
    },
    {
      name: 'config',
      match: '/config',
      invoke: api => api.handlers.loadConfig(),
      response: { logs: { enabled: true, retention_days: 7, max_entries: 500 } },
    },
    {
      name: 'runtime',
      match: '/runtime',
      invoke: api => api.handlers.loadRuntime(),
      response: { loaded: true, stopped: false, active_requests: 0, pending_operations: 0, model_count: 0, credentials: [] },
    },
    {
      name: 'model-test',
      match: '/model-test',
      invoke: api => api.handlers.runModelTest(),
      response: { credential_id: 'cred-1', model_id: 'flash', status: 'success', usage: {} },
    },
  ];
  for (const item of cases) {
    const pending = deferred();
    const view = fakePage({
      confirmImpl: () => true,
      fetchImpl: async url => url.includes(item.match) ? pending.promise : respond(404, { error: 'not found' }),
    });
    const consoleApi = view.context.OpenDesignConsole;
    consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
    consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false }], models: [{ id: 'flash', upstream_id: 'open-design/flash' }] }));
    consoleApi.state.mappings = [{ id: 'flash', upstream_id: 'open-design/flash' }];
    consoleApi.state.savedMappings = [{ id: 'flash', upstream_id: 'open-design/flash' }];
    consoleApi.state.upstreamFor = 'cred-1';
    consoleApi.state.upstream = [{ id: 'flash' }];
    consoleApi.state.mappings = [{ id: 'flash', upstream_id: 'open-design/flash' }];
    consoleApi.state.requestLogs = [{ id: 'old-log' }];
    consoleApi.state.logPage = { loaded: true, total: 1, offset: 0, limit: 50, has_more: false };
    consoleApi.state.requestStats = { total: 1 };
    consoleApi.state.config = {
      logs: { enabled: true, retention_days: 7, max_entries: 500 },
      upstream: { origin: 'https://amr-link.open-design.ai/v1', fixed: true },
      proxy: { owner: 'CPA', read_only: true },
      routing: { owner: 'CPA', read_only: true },
    };
    consoleApi.state.runtime = { loaded: true, credentials: [] };
    consoleApi.state.modelTest = { status: 'success', model_id: 'flash', usage: {} };
    consoleApi.state.modelCoverage = { 'cred-1\\u0000flash': { state: 'available' } };
    consoleApi.render.renderConfig();
    if (item.name === 'model-test') {
      view.document.getElementById('modelTestCredential').value = 'cred-1';
      view.document.getElementById('modelTestModel').value = 'flash';
      view.document.getElementById('modelTestPrompt').value = 'Reply with OK.';
    }
    const flight = item.invoke(consoleApi);
    await new Promise(setImmediate);
    const busyName = item.name === 'model-test' ? 'modelTest' : item.name;
    assert.equal(consoleApi.state.optionalBusy[busyName], true, `${item.name}: request is marked busy`);
    consoleApi.handlers.clearAuth();
    assert.equal(consoleApi.state.verified, null, `${item.name}: management auth is cleared`);
    assert.deepEqual(plain(consoleApi.state.status.credentials), []);
    assert.deepEqual(plain(consoleApi.state.requestLogs), []);
    assert.equal(consoleApi.state.requestStats, null);
    assert.equal(consoleApi.state.config, null);
    assert.equal(consoleApi.state.runtime, null);
    assert.equal(consoleApi.state.modelTest, null);
    assert.deepEqual(Object.keys(consoleApi.state.modelCoverage), []);
    assert.deepEqual(Object.keys(consoleApi.state.optionalBusy), []);
    assert.equal(view.document.getElementById('refreshLogs').disabled, false);
    assert.equal(view.document.getElementById('loadConfig').disabled, false);
    assert.equal(view.document.getElementById('loadRuntime').disabled, false);
    assert.equal(view.document.getElementById('runModelTest').disabled, false);
    assert.equal(view.document.getElementById('configRetentionDays').value, '', `${item.name}: stale config is cleared`);
    assert.equal(view.document.getElementById('configMaxEntries').value, '', `${item.name}: stale config is cleared`);

    pending.resolve(respond(200, item.response));
    await flight;
    assert.deepEqual(plain(consoleApi.state.requestLogs), [], `${item.name}: late response cannot restore logs`);
    assert.equal(consoleApi.state.config, null, `${item.name}: late response cannot restore config`);
    assert.equal(consoleApi.state.runtime, null, `${item.name}: late response cannot restore runtime`);
    assert.equal(consoleApi.state.modelTest, null, `${item.name}: late response cannot restore model test`);
  }
});

test('optional management 401 and 403 return to the auth state for every management route', async () => {
  const cases = [
    ['logs', url => url.includes('/request-logs'), api => api.handlers.loadRequestLogs({ reset: true })],
    ['stats', url => url.includes('/request-stats'), api => api.handlers.loadRequestStats()],
    ['clear', url => url.includes('/request-logs'), api => api.handlers.clearRequestLogs()],
    ['config', url => url.endsWith('/config'), api => api.handlers.loadConfig()],
    ['runtime', url => url.endsWith('/runtime'), api => api.handlers.loadRuntime()],
    ['model-test', url => url.endsWith('/model-test'), api => api.handlers.runModelTest()],
  ];
  for (const status of [401, 403]) {
    for (const [name, matches, invoke] of cases) {
      const view = fakePage({
        confirmImpl: () => true,
        fetchImpl: async url => matches(url) ? respond(status, { error: 'management request rejected' }) : respond(404, { error: 'not found' }),
      });
      const consoleApi = view.context.OpenDesignConsole;
      consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
      consoleApi.render.applyStatus(statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false }] }));
      if (name === 'model-test') {
        view.document.getElementById('modelTestCredential').value = 'cred-1';
        view.document.getElementById('modelTestModel').value = 'flash';
      }
      await invoke(consoleApi);
      assert.equal(consoleApi.state.verified, null, `${name} HTTP ${status}: auth is cleared`);
      assert.equal(view.document.getElementById('accountsPanel').hidden, true, `${name} HTTP ${status}: panels are hidden`);
      assert.match(view.document.getElementById('authState').textContent, new RegExp(String(status)));
      assert.deepEqual(plain(consoleApi.state.status.credentials), []);
    }
  }
});

test('optional busy groups do not unlock each other or leave pagination disabled', async () => {
  const pending = { logs: deferred(), config: deferred(), stats: deferred() };
  const view = fakePage({
    fetchImpl: async url => {
      if (url.includes('/request-logs')) return pending.logs.promise;
      if (url.includes('/request-stats')) return pending.stats.promise;
      if (url.endsWith('/config')) return pending.config.promise;
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  const configFlight = consoleApi.handlers.loadConfig();
  await new Promise(setImmediate);
  assert.equal(view.document.getElementById('loadConfig').disabled, true);
  assert.equal(view.document.getElementById('saveConfig').disabled, true);
  assert.equal(view.document.getElementById('refreshLogs').disabled, false);

  const logsFlight = consoleApi.handlers.loadRequestLogs({ reset: true });
  await new Promise(setImmediate);
  assert.equal(view.document.getElementById('refreshLogs').disabled, true);
  assert.equal(view.document.getElementById('loadConfig').disabled, true);

  pending.config.resolve(respond(200, { logs: { enabled: true, retention_days: 7, max_entries: 500 } }));
  await configFlight;
  assert.equal(view.document.getElementById('saveConfig').disabled, false, 'config controls recover after their own request');
  assert.equal(view.document.getElementById('refreshLogs').disabled, true, 'log request still owns log controls');

  pending.logs.resolve(respond(200, { logs: [], total: 0, limit: 50, offset: 0, has_more: true, next_offset: 50 }));
  await logsFlight;
  assert.equal(view.document.getElementById('refreshLogs').disabled, false);
  assert.equal(view.document.getElementById('logsNext').disabled, false);

  const statsFlight = consoleApi.handlers.loadRequestStats();
  await new Promise(setImmediate);
  assert.equal(view.document.getElementById('logsNext').disabled, true);
  pending.stats.resolve(respond(200, { total: 0, success: 0, failed: 0, streams: 0, tokens: {} }));
  await statsFlight;
  assert.equal(view.document.getElementById('logsNext').disabled, false, 'stats completion re-renders the current page controls');
});

test('rendering an unavailable config clears stale form values and disables saving', () => {
  const view = fakePage();
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.config = {
    logs: { enabled: true, retention_days: 9, max_entries: 12 },
    upstream: { origin: 'https://amr-link.open-design.ai/v1', fixed: true },
    proxy: { owner: 'CPA', read_only: true },
    routing: { owner: 'CPA', read_only: true },
  };
  consoleApi.render.renderConfig();
  assert.equal(view.document.getElementById('configRetentionDays').value, '9');
  assert.equal(view.document.getElementById('configMaxEntries').value, '12');
  assert.equal(view.document.getElementById('saveConfig').disabled, false);
  consoleApi.state.config = null;
  consoleApi.render.renderConfig();
  assert.equal(view.document.getElementById('configLogsEnabled').checked, false);
  assert.equal(view.document.getElementById('configRetentionDays').value, '');
  assert.equal(view.document.getElementById('configMaxEntries').value, '');
  assert.equal(view.document.getElementById('saveConfig').disabled, true);
});

test('request stats render per-model rows and the retention scope from input/output tokens', async () => {
  const view = fakePage({
    fetchImpl: async url => url.includes('/request-stats') ? respond(200, {
      total: 3, success: 2, failed: 1, streams: 1, success_rate: 2 / 3,
      upstream_attempted: 2, local_rejected: 1,
      tokens: { input: 11, output: 7, total: 18, requests_with_usage: 2, requests_without_usage: 1 },
      models: [
        { model: 'od-flash', total: 2, success: 2, failed: 0, upstream_attempted: 2, local_rejected: 0, input_tokens: 11, output_tokens: 7, total_tokens: 18 },
        { model: 'od-pro', total: 1, success: 0, failed: 1, upstream_attempted: 0, local_rejected: 1, input_tokens: null, output_tokens: null, total_tokens: null },
      ],
      retention: { enabled: true, retention_days: 7, max_entries: 500, entries: 3, oldest: '2026-11-01T00:00:00Z', newest: '2026-11-03T00:00:00Z' },
    }) : respond(404, { error: 'not found' }),
  });
  const consoleApi = view.context.OpenDesignConsole;
  await consoleApi.handlers.loadRequestStats();
  assert.equal(consoleApi.state.requestStats.tokens.input, 11, 'input tokens are read from tokens.input');
  assert.equal(consoleApi.state.requestStats.tokens.output, 7, 'output tokens are read from tokens.output');
  const facts = view.document.getElementById('requestStats').textContent;
  assert.match(facts, /输入 Token 11/);
  assert.match(facts, /输出 Token 7/);
  assert.match(facts, /本地拒绝 1/);
  assert.match(facts, /保留 7 天，最多 500 条/);
  assert.match(facts, /当前 3 条/);
  const models = view.document.getElementById('requestStatsModels');
  assert.equal(models.children.length, 2);
  assert.match(models.children[0].textContent, /od-flash/);
  assert.match(models.children[0].textContent, /Token 18/);
  assert.match(models.children[1].textContent, /本地拒绝 1/);
  assert.match(models.children[1].textContent, /有失败/);
  assert.ok(!models.children[1].textContent.includes('Token'), 'a null token total is omitted, never zero');
});

test('runtime lists only safe last-test identity fields and unknown enums without raw text', async () => {
  const view = fakePage({
    fetchImpl: async () => respond(200, {
      observed_at: '2026-11-03T00:00:00Z', loaded: true, stopped: false,
      active_requests: 0, pending_operations: 0, model_count: 1,
      host: { available: true },
      credentials: [{
        id: 'cred-1', label: '主账户', disabled: false, model_count: 1,
        host: { disabled: null, unavailable: null, status: 'cooldown', status_message: '', next_retry_after: '0001-01-01T00:00:00Z' },
        last_test: {
          model_id: 'flash', upstream_model: 'open-design/flash', status: 'failed',
          http_status: 429, duration_ms: 12, tested_at: '2026-11-03T00:00:00Z',
          error_kind: 'upstream_error', api_key: 'leaked-fixture-key', error_message: 'raw upstream body',
        },
      }],
    }),
  });
  const consoleApi = view.context.OpenDesignConsole;
  await consoleApi.handlers.loadRuntime();
  const text = view.document.getElementById('runtimeCredentials').textContent;
  assert.match(text, /最近测试 失败/);
  assert.match(text, /模型 flash/);
  assert.match(text, /上游 open-design\/flash/);
  assert.match(text, /原因 upstream_error/);
  assert.ok(!text.includes('leaked-fixture-key'));
  assert.ok(!text.includes('raw upstream body'));
  assert.ok(!text.includes('0001-01-01'), 'a zero next_retry_after is not rendered');
  const normalized = consoleApi.state.runtime.credentials[0].last_test;
  assert.deepEqual(Object.keys(normalized).sort(), [
    'duration_ms', 'error_kind', 'http_status', 'model_id', 'status', 'tested_at', 'upstream_model',
  ]);
});

test('a mapping save invalidates a visible model-test result and drops its in-flight answer', async () => {
  const pending = [];
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/model-test')) {
        const request = deferred();
        pending.push(request);
        return request.promise;
      }
      if (url.endsWith('/models') && init.method === 'POST') return respond(200, { models: JSON.parse(init.body).models });
      if (url.endsWith('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const consoleApi = view.context.OpenDesignConsole;
  consoleApi.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  consoleApi.state.status = view.context.OpenDesignCore.normalizeStatus(statusBody());
  consoleApi.handlers.activateTab('models');
  consoleApi.state.mappings = [{ id: 'flash', upstream_id: 'open-design/flash' }];
  consoleApi.state.savedMappings = [{ id: 'flash', upstream_id: 'open-design/flash' }];
  consoleApi.state.upstreamFor = 'cred-1';
  consoleApi.state.upstream = [{ id: 'open-design/flash' }];
  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'flash';
  view.document.getElementById('modelTestPrompt').value = 'Reply with OK.';

  const flight = consoleApi.handlers.runModelTest();
  await new Promise(setImmediate);
  assert.equal(consoleApi.state.optionalBusy.modelTest, true);
  const generation = consoleApi.state.modelTestGeneration;
  await consoleApi.handlers.saveModels();
  assert.ok(consoleApi.state.modelTestGeneration > generation, 'saving mappings advances the model-test generation');
  assert.equal(consoleApi.state.modelTest, null);
  pending[0].resolve(respond(200, {
    credential_id: 'cred-1', model_id: 'flash', status: 'success',
    usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
  }));
  await flight;
  assert.equal(consoleApi.state.modelTest, null, 'a stale answer is not displayed after the mapping changed');
  assert.notEqual(consoleApi.state.optionalBusy.modelTest, true, 'the busy flag is released after the stale answer');
});

test('model-test identity keeps a long alias intact and rejects one over 512 UTF-8 bytes', () => {
  const core = fakePage().context.OpenDesignCore;
  const alias = 'x'.repeat(400);
  const normalized = core.normalizeModelTest({ credential_id: 'cred-1', model_id: alias, status: 'success' });
  assert.equal(normalized.model_id, alias, 'identity is never truncated to a display width');
  const max = '模'.repeat(170) + 'ab'; // 512 bytes
  assert.equal(core.utf8ByteLength(max), 512);
  assert.equal(core.normalizeModelTest({ model_id: max }).model_id, max);
  assert.equal(core.normalizeModelTest({ model_id: `${max}x` }).model_id, '', 'over-length identities are refused');
  assert.equal(core.normalizeModelTest({ model_id: 'a\nb' }).model_id, '', 'control characters are refused');
  assert.equal(core.normalizeModelTest({ model_id: 42 }).model_id, '42', 'numbers are compared as text, then bounded');
  // Every page value is trimmed at input time, so identity matches exactly what
  // was sent upstream; only the byte bound and control-character check follow.
  assert.equal(core.safeIdentity(' x '), 'x');
  assert.equal(core.safeIdentity(''), '');
  assert.equal(core.safeIdentity(alias, 512), alias);
  assert.equal(core.safeIdentity(alias, 399), '', 'the byte budget is enforced, not the rune count');
});

test('a quota window whose reset time has passed or precedes exhaustion stops showing a forecast', () => {
  const core = fakePage().context.OpenDesignCore;
  assert.equal(core.quotaForecastExpired({}), false);
  assert.equal(core.quotaForecastExpired({ resetsAt: '2026-11-03T05:00:00Z', forecast: { exhausts_at: '2026-11-03T02:00:00Z' } }, Date.parse('2026-11-03T01:00:00Z')), false);
  assert.equal(core.quotaForecastExpired({ resetsAt: '2026-11-03T05:00:00Z', forecast: { exhausts_at: '2026-11-03T02:00:00Z' } }, Date.parse('2026-11-03T03:00:00Z')), true);
  assert.equal(core.quotaForecastExpired({ resetsAt: '2026-11-03T01:00:00Z', forecast: { exhausts_at: '2026-11-03T02:00:00Z' } }, Date.parse('2026-11-03T00:00:00Z')), true);
  assert.equal(core.quotaForecastExpired({ resetsAt: '2026-11-03T01:00:00Z', forecast: { exhausts_at: '2026-11-03T00:00:00Z' } }, Date.parse('2026-11-03T00:30:00Z')), true);
});

test('logout and re-login restore the log search and credential filters', async () => {
  const view = fakePage({
    fetchImpl: async () => respond(404, { error: 'not found' }),
  });
  const consoleApi = view.context.OpenDesignConsole;
  view.document.getElementById('logsFilterSearch').value = 'stale-model';
  view.document.getElementById('logsFilterCredential').value = 'cred-1';
  view.document.getElementById('logsFilterStatus').value = 'failed';
  consoleApi.handlers.clearAuth();
  assert.equal(view.document.getElementById('logsFilterSearch').value, '');
  assert.equal(view.document.getElementById('logsFilterCredential').value, '');
  assert.equal(view.document.getElementById('logsFilterStatus').value, '');
});

test('theme preference cycles system -> light -> dark, persists, and is applied to the root', () => {
  const view = fakePage();
  const consoleApi = view.context.OpenDesignConsole;
  assert.equal(consoleApi.state.themePreference, 'system');
  assert.equal(consoleApi.handlers.cycleThemePreference(), 'light');
  assert.equal(view.localStorage.getItem('open-design-theme'), 'light');
  assert.equal(view.document.documentElement.getAttribute('data-theme'), 'light');
  assert.equal(consoleApi.handlers.cycleThemePreference(), 'dark');
  assert.equal(view.localStorage.getItem('open-design-theme'), 'dark');
  assert.equal(view.document.documentElement.getAttribute('data-theme-resolved'), 'dark');
  assert.equal(consoleApi.handlers.cycleThemePreference(), 'system');
  assert.equal(view.localStorage.getItem('open-design-theme'), null, 'following the system clears the pin');
  const label = view.document.getElementById('themeToggle').textContent;
  assert.match(label, /^主题：/);
  const core = view.context.OpenDesignCore;
  assert.equal(core.nextThemePreference('bogus'), 'light', 'an unknown preference falls back to system first');
  assert.equal(core.resolvedTheme('system', true), 'dark');
  assert.equal(core.resolvedTheme('system', false), 'light');
  assert.equal(core.resolvedTheme('light', true), 'light');
  assert.match(core.themePreferenceLabel('system', 'dark'), /跟随系统（深色）/);
});

test('the console exposes an accessible theme toggle and a system-aware stylesheet default', () => {
  const page = pageHTML();
  assert.match(page, /id="themeToggle"[^>]*type="button"[^>]*aria-label=/);
  assert.match(page, /prefers-color-scheme:\s*dark/);
  assert.match(page, /:root\[data-theme="system"\]/);
  assert.ok(!/data-theme="dark"/.test(page.split('</style>')[0].replace(/:root\[data-theme="dark"\]/g, '')), 'dark is opt-in, not the default');
  for (const tab of ['概览', '凭据', '模型', '请求日志', '配置']) assert.ok(page.includes(tab), `tab ${tab} exists`);
  assert.ok(page.includes('role="tablist"') && page.includes('role="tab"') && page.includes('role="tabpanel"'));
});


test('system color changes update the theme label and resolved value without fetching data', async () => {
  const view = fakePage();
  const media = { matches: false };
  let onChange;
  view.context.matchMedia = () => ({
    get matches() { return media.matches; },
    addEventListener(type, callback) { assert.equal(type, 'change'); onChange = callback; },
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  assert.equal(view.document.documentElement.getAttribute('data-theme-resolved'), 'light');
  assert.equal(typeof onChange, 'function');
  media.matches = true;
  onChange();
  assert.equal(view.document.documentElement.getAttribute('data-theme-resolved'), 'dark');
  assert.match(view.document.getElementById('themeToggle').getAttribute('aria-label'), /跟随系统（深色）/);
  api.handlers.applyThemePreference('light');
  media.matches = false;
  onChange();
  media.matches = true;
  onChange();
  assert.equal(view.document.documentElement.getAttribute('data-theme-resolved'), 'light', 'system events preserve an explicit preference');
  assert.equal(view.calls.length, 0, 'color changes do not trigger management or provider requests');
});

// --- retry scheduling / request-scoped error rules ---------------------------

const errorRulesBody = (overrides = {}) => ({
  rules: [
    { status: 400, match: ['[upstream:tier_limit_exceeded]'], action: 'continue-and-cooldown' },
    { status: 429, match: ['[upstream:insufficient_quota]'], action: 'continue-and-cooldown' },
  ],
  defaults: [
    { status: 400, match: ['[upstream:tier_limit_exceeded]'], action: 'continue-and-cooldown' },
  ],
  sync: { total: 2, succeeded: 2, failed: [] },
  inherits_cpa: false,
  ...overrides,
});

function errorRulesResponse(view) {
  const calls = view.calls.map(call => ({ url: call.url, init: call.init }));
  return calls.filter(call => call.url.includes('/error-rules'));
}

// depth-first search for a node carrying a class, so the tests do not depend on
// the exact nesting of a rule card.
function byClass(node, className) {
  const stack = [...(node.children || [])];
  while (stack.length) {
    const current = stack.shift();
    if (current.className && String(current.className).split(/\s+/).includes(className)) return current;
    if (current.children) stack.push(...current.children);
  }
  return null;
}

function ruleCard(view, index) {
  return view.document.getElementById('errorRulesList').children[index];
}

test('error rule explanations map known markers and never produce markup', () => {
  const Core = fakePage().context.OpenDesignCore;
  const known = Core.explainErrorRule({ status: 400, match: ['[upstream:tier_limit_exceeded]'], action: 'continue-and-cooldown' }, 0);
  assert.match(known.name, /账号并发上限/);
  assert.match(known.detail, /tier_limit_exceeded/);
  const balance = Core.explainErrorRule({ status: 402, match: ['[upstream:insufficient_balance]'], action: 'continue-and-cooldown' }, 0);
  assert.match(balance.name, /额度耗尽或余额不足/);
  assert.match(balance.detail, /insufficient_balance/);
  const fallback = Core.explainErrorRule({ status: 503, match: ['some custom text'], action: 'stop' }, 2);
  assert.match(fallback.name, /规则 3 · 自定义条件/);
  assert.match(fallback.detail, /503/);
  const forged = Core.explainErrorRule({ status: 400, match: ['<img onerror=alert(1)>'], action: 'stop' }, 0);
  assert.ok(!/<img/.test(forged.name) && !/<img/.test(forged.detail), 'explanations must stay plain text');
  const empty = Core.explainErrorRule({ status: 500, action: 'stop' }, 0);
  assert.match(empty.detail, /状态码 500/);
  assert.match(empty.name, /自定义条件/);
});

test('error rule payload keeps both condition kinds, drops blanks and omits empty arrays', () => {
  const Core = fakePage().context.OpenDesignCore;
  const payload = Core.buildErrorRulesPayload([
    {
      status: 429,
      action: 'continue-and-cooldown',
      conditions: [
        { type: 'contains', text: '  [upstream:rate_limit_exceeded]  ' },
        { type: 'regex', text: '^\\[upstream:' },
        { type: 'contains', text: '   ' },
      ],
    },
    { status: 500, action: 'stop', conditions: [{ type: 'contains', text: 'boom' }] },
  ]);
  assert.equal(payload.length, 2);
  // Values cross a vm boundary, so compare structurally.
  assert.deepEqual(JSON.parse(JSON.stringify(payload[0])), {
    status: 429,
    action: 'continue-and-cooldown',
    match: ['[upstream:rate_limit_exceeded]'],
    'match-regexr': ['^\\[upstream:'],
  });
  assert.deepEqual(JSON.parse(JSON.stringify(payload[1])), { status: 500, action: 'stop', match: ['boom'] });
  assert.ok(!('match-regexr' in payload[1]), 'an empty regex list is omitted, never null');
  assert.deepEqual(JSON.parse(JSON.stringify(Core.buildErrorRulesPayload([]))), []);
});

test('error rule validation rejects bad status, empty conditions and invalid regexes', () => {
  const Core = fakePage().context.OpenDesignCore;
  assert.match(Core.validateErrorRules([{ status: 99, action: 'stop', conditions: [{ type: 'contains', text: 'x' }] }]), /100 到 599/);
  assert.match(Core.validateErrorRules([{ status: 400, action: 'stop', conditions: [{ type: 'contains', text: '  ' }] }]), /至少需要一个匹配条件/);
  assert.match(Core.validateErrorRules([{ status: 400, action: 'stop', conditions: [{ type: 'regex', text: '(' }] }]), /正则表达式无效/);
  assert.match(Core.validateErrorRules([{ status: 400, action: 'rotate', conditions: [{ type: 'contains', text: 'x' }] }]), /选择一个动作/);
  assert.equal(Core.validateErrorRules([{ status: 400, action: 'stop', conditions: [{ type: 'contains', text: 'x' }] }]), '');
});

test('opening the error-rules dialog fetches once, keeps a late response out of a newer open', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/error-rules') ? respond(200, errorRulesBody()) : respond(200, statusBody())),
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  const before = errorRulesResponse(view).length;
  assert.equal(before, 0, 'loading the console must not read the error rules');
  await api.handlers.openErrorRules();
  assert.equal(errorRulesResponse(view).length, 1);
  const list = view.document.getElementById('errorRulesList');
  assert.equal(list.children.length, 2);
  assert.equal(list.children[0].getAttribute('data-rule'), '0');
  assert.match(byClass(list.children[0], 'error-rule-name').textContent, /账号并发上限/);
  await api.handlers.openErrorRules();
  assert.equal(errorRulesResponse(view).length, 2, 'each explicit open reads the policy again');
});

test('saving error rules sends the edited policy and renders the per-account sync result', async () => {
  const bodies = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      if (url.includes('/error-rules/sync')) return respond(200, errorRulesBody());
      if (url.includes('/error-rules')) {
        if (init && init.method === 'PUT') {
          bodies.push(JSON.parse(init.body));
          return respond(200, errorRulesBody({ sync: { total: 2, succeeded: 1, failed: [{ id: 'cred-2', label: '备用账号', error: '凭据规则同步失败，请重试同步' }] } }));
        }
        return respond(200, errorRulesBody());
      }
      return respond(200, statusBody());
    },
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  await api.handlers.openErrorRules();
  const list = view.document.getElementById('errorRulesList');
  const status = byClass(ruleCard(view, 0), 'error-rule-status');
  status.value = '401';
  await status.emit('input');
  await api.handlers.submitErrorRules();
  assert.equal(bodies.length, 1);
  assert.equal(bodies[0].rules[0].status, 401);
  assert.deepEqual(bodies[0].rules[0].match, ['[upstream:tier_limit_exceeded]']);
  const state = view.document.getElementById('errorRulesState');
  assert.match(state.textContent, /同步完成：1\/2 个账号/);
  assert.match(state.textContent, /备用账号/);
  assert.ok(!/<img|onerror/.test(state.textContent));
});

test('the sync result renders an injected error string as literal text', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/error-rules')
      ? respond(200, errorRulesBody({ sync: { total: 1, succeeded: 0, failed: [{ id: 'cred-1', label: '<img onerror=alert(1)>', error: '<script>bad</script>' }] } }))
      : respond(200, statusBody())),
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  await api.handlers.openErrorRules();
  const text = view.document.getElementById('errorRulesState').textContent;
  assert.match(text, /<img onerror=alert\(1\)>/);
  assert.match(text, /<script>bad<\/script>/);
});

test('re-syncing is blocked while the draft is dirty and posts the saved policy when clean', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      return url.includes('/error-rules') ? respond(200, errorRulesBody()) : respond(200, statusBody());
    },
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  await api.handlers.openErrorRules();
  const retry = view.document.getElementById('retryErrorRules');
  assert.equal(retry.disabled, false, 'a clean draft can be re-synced');
  const status = byClass(ruleCard(view, 0), 'error-rule-status');
  status.value = '500';
  await status.emit('input');
  assert.equal(retry.disabled, true, 'unsaved edits block re-syncing');
  assert.match(retry.getAttribute('title'), /请先保存/);
  await api.handlers.retryErrorRules();
  assert.equal(calls.filter(call => call.url.includes('/error-rules/sync')).length, 0, 'a dirty draft must not re-sync');
  await api.handlers.submitErrorRules();
  assert.equal(retry.disabled, false);
  await api.handlers.retryErrorRules();
  const sync = calls.filter(call => call.url.includes('/error-rules/sync'));
  assert.equal(sync.length, 1);
  assert.equal(sync[0].init.method, 'POST');
  assert.equal(sync[0].init.body, '{}');
});

test('reset replaces the draft with the server defaults', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      if (url.includes('/error-rules/reset')) {
        return respond(200, errorRulesBody({ rules: [{ status: 500, match: ['empty response content'], action: 'stop' }], defaults: [{ status: 500, match: ['empty response content'], action: 'stop' }] }));
      }
      return url.includes('/error-rules') ? respond(200, errorRulesBody()) : respond(200, statusBody());
    },
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  await api.handlers.openErrorRules();
  await api.handlers.resetErrorRules();
  const reset = calls.filter(call => call.url.includes('/error-rules/reset'));
  assert.equal(reset.length, 1);
  assert.equal(reset[0].init.method, 'POST');
  const list = view.document.getElementById('errorRulesList');
  assert.equal(list.children.length, 1);
  const status = byClass(ruleCard(view, 0), 'error-rule-status');
  assert.equal(String(status.value), '500');
});

test('the error-rules dialog is present in the console page and adds no tab', () => {
  const page = pageHTML();
  for (const id of ['errorRulesDialog', 'errorRulesList', 'saveErrorRules', 'retryErrorRules', 'resetErrorRules', 'closeErrorRules', 'openErrorRuleHelp', 'errorRuleHelpDialog', 'errorRulesState', 'openErrorRules', 'addErrorRule']) {
    assert.ok(page.includes(`id="${id}"`), `missing #${id}`);
  }
  const tabs = [...page.matchAll(/<button class="tab" id="([^"]+)"/g)].map(match => match[1]);
  assert.deepEqual(tabs, ['tabOverview', 'tabCredentials', 'tabModels', 'tabLogs', 'tabConfig']);
  assert.ok(page.includes('role="dialog"') || page.includes('<dialog'), 'the rules editor is a dialog');
  for (const action of ['停止并冷却', '继续并冷却', '包含文本', '正则表达式']) assert.ok(page.includes(action), `help/editor mentions ${action}`);
});

test('an empty rule list is explained as inheriting the CPA configuration', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/error-rules')
      ? respond(200, errorRulesBody({ rules: [], defaults: [], inherits_cpa: true, sync: { total: 1, succeeded: 1, failed: [] } }))
      : respond(200, statusBody())),
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  await api.handlers.openErrorRules();
  assert.match(view.document.getElementById('errorRulesList').textContent, /由 CPA 自身的配置决定/);
  assert.match(view.document.getElementById('errorRulesState').textContent, /由 CPA 配置决定/);
});

test('loading the console, switching tabs and changing theme never read the error rules', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/error-rules') ? respond(200, errorRulesBody()) : respond(200, statusBody())),
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  for (const tab of ['credentials', 'models', 'logs', 'config', 'overview']) api.handlers.activateTab(tab);
  api.handlers.applyThemePreference('dark');
  api.handlers.applyThemePreference('light');
  api.handlers.cycleThemePreference();
  assert.equal(errorRulesResponse(view).length, 0);
});

test('the error-rules dialog and its action help are wired to real handlers', () => {
  const page = pageHTML();
  for (const id of ['openErrorRules', 'closeErrorRules', 'addErrorRule', 'saveErrorRules',
                    'retryErrorRules', 'resetErrorRules', 'openErrorRuleHelp', 'closeErrorRuleHelp']) {
    assert.ok(page.includes(`id="${id}"`), `missing #${id}`);
  }
  // The console binds every one of them in its own module, so the button is not
  // a dead control that only works in a browser acceptance run.
  const view = fakePage({
    fetchImpl: async url => (url.includes('/error-rules') ? respond(200, errorRulesBody()) : respond(200, statusBody())),
  });
  const api = view.context.OpenDesignConsole;
  assert.equal(typeof api.handlers.openErrorRuleHelp, 'function');
  assert.equal(typeof api.handlers.closeErrorRuleHelp, 'function');
  assert.equal(typeof api.handlers.closeErrorRules, 'function');
  // The fake DOM has no <dialog> implementation, so stub the two methods the
  // console calls and assert they are reached through the exported handler.
  const help = view.document.getElementById('errorRuleHelpDialog');
  const calls = [];
  help.showModal = () => calls.push('showModal');
  help.close = () => calls.push('close');
  help.open = false;
  api.handlers.openErrorRuleHelp();
  assert.deepEqual(calls, ['showModal']);
  help.open = true;
  api.handlers.closeErrorRuleHelp();
  assert.deepEqual(calls, ['showModal', 'close']);
});

test('editing a rule marks the draft dirty and blocks 重新同步', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/error-rules') ? respond(200, errorRulesBody()) : respond(200, statusBody())),
  });
  const api = view.context.OpenDesignConsole;
  await api.bootstrap();
  await api.handlers.openErrorRules();
  assert.equal(view.document.getElementById('retryErrorRules').disabled, false);
  api.handlers.addErrorRule();
  assert.equal(view.document.getElementById('retryErrorRules').disabled, true,
    'an unsaved draft must not silently re-sync the saved policy');
  assert.equal(view.document.getElementById('saveErrorRules').disabled, false);
});

// ---- 思考强度映射 (thinking-strength mapping) --------------------------------

// The shared `plain` helper above normalizes vm-context objects for strict
// deep comparison; the reasoning cases below rely on it.

const reasoningStatus = (...models) => statusBody({ models });

test('the reasoning panel exists with real controls and does not add a tab', () => {
  const page = pageHTML();
  for (const id of [
    'reasoningPanel', 'reasoningModel', 'reasoningEnabled', 'reasoningRows',
    'addReasoningRule', 'resetReasoningTable', 'saveReasoning', 'reasoningState', 'reasoningDirty',
  ]) {
    assert.ok(page.includes(`id="${id}"`), `${id} must be present`);
  }
  assert.ok(page.includes('思考强度映射'));
  // The panel lives inside the models tab: the console keeps its five tabs.
  const tabs = [...page.matchAll(/data-tab="([^"]+)"/g)].map(match => match[1]);
  assert.deepEqual(tabs, ['overview', 'credentials', 'models', 'logs', 'config']);
  // A disabled mapping never rewrites a request, so the copy has to say so.
  assert.ok(/默认关闭/.test(page));
});

test('reasoning helpers default, normalize and validate against the closed sets', () => {
  const core = fakePage().context.OpenDesignCore;
  // A DeepSeek upstream is pre-filled but disabled; anything else starts empty.
  const prefill = plain(core.defaultReasoningMapping('deepseek-v4.1-flash'));
  assert.equal(prefill.enabled, false, 'the pre-fill must not enable itself');
  assert.deepEqual(prefill, plain(core.defaultReasoningMapping('DeepSeek-V4')));
  assert.deepEqual(plain(core.defaultReasoningMapping('vela/gpt-6')), { enabled: false, rules: [] });
  assert.deepEqual(plain(core.defaultReasoningMapping('')), { enabled: false, rules: [] });
  assert.deepEqual(Object.fromEntries(prefill.rules.map(rule => [rule.from, rule.to])), {
    unset: 'unset', none: 'none', minimal: 'low', low: 'low',
    medium: 'high', high: 'high', xhigh: 'high', max: 'max', ultra: 'max',
  });
  // ultra is a request-side strength only: it can never be a target.
  assert.ok(!core.reasoningTargetOptions().some(option => option.value === 'ultra'));
  assert.ok(core.reasoningSourceOptions().some(option => option.value === 'ultra'));

  // Server data outside the closed sets, or a repeated source, is dropped.
  assert.deepEqual(
    plain(core.normalizeReasoningMapping({
      enabled: true,
      rules: [
        { from: 'high', to: 'high' },
        { from: 'bogus', to: 'high' },
        { from: 'medium', to: 'ultra' },
        { from: 'low', to: 'low' },
        { from: 'low', to: 'max' },
      ],
    })),
    { enabled: true, rules: [{ from: 'low', to: 'low' }, { from: 'high', to: 'high' }] },
    'unknown strengths and duplicate sources never reach the table',
  );
  // `enabled` is strict: only a literal true counts.
  assert.equal(core.normalizeReasoningMapping({ enabled: 'true' }).enabled, false);
  assert.deepEqual(plain(core.normalizeReasoningMapping(null)), { enabled: false, rules: [] });
});

test('reasoning dirtiness ignores rule order and the payload rejects duplicates', () => {
  const core = fakePage().context.OpenDesignCore;
  const left = { enabled: true, rules: [{ from: 'medium', to: 'high' }, { from: 'low', to: 'low' }] };
  const right = { enabled: true, rules: [{ from: 'low', to: 'low' }, { from: 'medium', to: 'high' }] };
  assert.equal(core.reasoningKey(left), core.reasoningKey(right), 'row order is not a change');
  assert.notEqual(core.reasoningKey(left), core.reasoningKey({ ...left, enabled: false }));
  assert.notEqual(core.reasoningKey(left), core.reasoningKey({ enabled: true, rules: [{ from: 'medium', to: 'low' }] }));

  assert.deepEqual(plain(core.buildReasoningPayload({ id: 'od-flash', upstream_id: 'open-design/flash' }, left)), {
    model: 'od-flash',
    upstream_id: 'open-design/flash',
    reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }, { from: 'low', to: 'low' }] },
  }, 'the edited row order is what gets sent');
  assert.throws(() => core.buildReasoningPayload({ id: 'a', upstream_id: 'b' }, {
    enabled: true, rules: [{ from: 'medium', to: 'high' }, { from: 'medium', to: 'low' }],
  }), /重复/, 'a duplicate source is an operator error, not a silent overwrite');
  assert.throws(() => core.buildReasoningPayload({ id: 'a', upstream_id: 'b' }, {
    enabled: true, rules: [{ from: 'extreme', to: 'high' }],
  }), /未知的思考强度/, 'a value outside the closed sets is refused, never dropped');
  // Adding a rule always picks an unused source, so the table cannot self-collide.
  assert.equal(core.nextReasoningSource([{ from: 'unset' }, { from: 'none' }]), 'minimal');
  assert.equal(core.nextReasoningSource(core.reasoningSourceOptions().map(option => ({ from: option.value }))), '');
});

test('the reasoning table pre-fills a DeepSeek model and stays empty for others', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, reasoningStatus(
      { id: 'od-deepseek', upstream_id: 'deepseek-v4.1-flash' },
      { id: 'od-other', upstream_id: 'vela/gpt-6-luna' },
    )) : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const select = view.document.getElementById('reasoningModel');
  assert.equal(select.children.length, 2, 'every saved model gets a row in the picker');
  assert.equal(api.state.reasoningModelKey, select.children[0].value, 'the first saved model is selected');
  assert.equal(view.document.getElementById('reasoningEnabled').checked, false, 'a fresh model is disabled');
  assert.equal(view.document.getElementById('reasoningRows').children.length, 9, 'the vendor table is pre-filled');

  api.handlers.selectReasoningModel(select.children[1].value);
  assert.equal(view.document.getElementById('reasoningRows').children.length, 1,
    'an unknown upstream shows the empty-state row instead of a fabricated table');
  assert.equal(api.render.reasoningDirty(), false, 'the pre-fill equals the stored baseline until it is edited');
});

test('saving a reasoning policy posts the rule table and clears the draft', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        calls.push(JSON.parse(init.body));
        return respond(200, {
          models: [{
            id: 'od-flash',
            upstream_id: 'open-design/flash',
            reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] },
          }],
        });
      }
      if (url.includes('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.setReasoningEnabled(true);
  assert.equal(api.render.reasoningDirty(), true, 'enabling the policy is an unsaved change');
  assert.equal(view.document.getElementById('reasoningDirty').hidden, false, 'the unsaved badge is shown');

  await api.handlers.saveReasoning();
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], {
    model: 'od-flash',
    upstream_id: 'open-design/flash',
    reasoning_mapping: { enabled: true, rules: [] },
  }, 'the payload is the full policy for one model');
  assert.equal(api.render.reasoningDirty(), false, 'the draft is dropped once the server accepted it');
  assert.match(view.document.getElementById('reasoningState').textContent, /已保存/);
  assert.equal(api.state.reasoningSaving, false);
  assert.equal(view.document.getElementById('reasoningDirty').hidden, true);
  // The server's answer is the new baseline, so the table now shows the policy.
  assert.deepEqual(plain(api.render.currentReasoningMapping(api.state.serverModels[0])),
    { enabled: true, rules: [{ from: 'medium', to: 'high' }] });
});

test('a duplicate strength is refused in the table and never sent', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        calls.push(JSON.parse(init.body));
        return respond(200, { models: [] });
      }
      if (url.includes('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const model = () => api.state.serverModels[0];
  await api.handlers.addReasoningRule();
  await api.handlers.addReasoningRule();
  let rules = plain(api.render.currentReasoningMapping(model())).rules;
  assert.equal(rules.length, 2, 'two rows were added with distinct sources');
  assert.notEqual(rules[0].from, rules[1].from);

  // Point the second row at a strength the first row already owns.
  await api.handlers.changeReasoningRule(1, 'from', rules[0].from);
  rules = plain(api.render.currentReasoningMapping(model())).rules;
  assert.notEqual(rules[1].from, rules[0].from, 'the duplicate is refused before it can be saved');
  assert.match(view.document.getElementById('reasoningState').textContent, /已有规则/,
    'the conflict is explained instead of silently applied');

  await api.handlers.saveReasoning();
  assert.equal(calls.length, 1);
  const sent = calls[0].reasoning_mapping.rules;
  assert.equal(new Set(sent.map(rule => rule.from)).size, sent.length, 'the saved policy has no duplicate source');
});

test('a reasoning save reports 404 and 409 instead of pretending to succeed', async () => {
  for (const [status, expected] of [[404, /模型已不存在/], [409, /模型或规则已变更/]]) {
    const view = fakePage({
      fetchImpl: async (url, init = {}) => {
        if (url.endsWith('/models/reasoning') && init.method === 'POST') return respond(status, { error: 'rejected' });
        if (url.includes('/status')) return respond(200, statusBody());
        return respond(404, { error: 'not found' });
      },
    });
    const api = view.context.OpenDesignConsole;
    api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
    await api.render.loadStatus();
    await api.handlers.addReasoningRule();
    await api.handlers.saveReasoning();
    assert.match(view.document.getElementById('reasoningState').textContent, expected, `${status} is explained`);
    assert.equal(view.document.getElementById('reasoningDirty').hidden, false, 'the draft survives a rejected save');
  }
});

test('the reasoning table follows a server refresh and keeps one draft per model', async () => {
  let models = [
    { id: 'od-a', upstream_id: 'deepseek-v4.1-flash', reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } },
    { id: 'od-b', upstream_id: 'vela/gpt-6-luna' },
  ];
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, reasoningStatus(...models)) : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const select = view.document.getElementById('reasoningModel');
  const first = select.children[0].value;
  assert.deepEqual(plain(api.render.currentReasoningMapping(api.state.serverModels[0])),
    { enabled: true, rules: [{ from: 'medium', to: 'high' }] }, 'the stored policy is the starting point');
  assert.equal(api.render.reasoningDirty(), false);

  await api.handlers.addReasoningRule();
  assert.equal(api.render.reasoningDirty(), true);
  // Switching away keeps the draft for the first model instead of discarding it.
  api.handlers.selectReasoningModel(select.children[1].value);
  assert.equal(api.render.reasoningDirty(), false, 'the second model has its own baseline');
  assert.equal(plain(api.render.currentReasoningMapping(api.state.serverModels[0])).rules.length, 2);
  api.handlers.selectReasoningModel(first);
  assert.equal(api.render.reasoningDirty(), true, 'returning to a model restores its draft');

  // A status refresh must not silently accept an unsaved draft as saved.
  api.render.applyStatus(reasoningStatus(...models));
  assert.equal(api.render.reasoningDirty(), true, 'an unsaved draft survives a refresh as a draft');
  // A model that disappears keeps its draft (or an upstream change would
  // silently destroy unsaved rules) and the copyable message names it.
  models = [{ id: 'od-b', upstream_id: 'vela/gpt-6-luna' }];
  api.render.applyStatus(reasoningStatus(...models));
  assert.equal(api.state.reasoningModelKey.includes('od-b'), true, 'the picker falls back to a live model');
  assert.equal(api.state.reasoningDrafts.size, 1, 'a removed model keeps its draft instead of losing it');
  assert.match(view.document.getElementById('reasoningOrphans').textContent, /草稿已保留/);
  assert.match(view.document.getElementById('reasoningOrphans').textContent, /od-a/);
});

test('orphan reasoning drafts remain separately visible through saves, editing and refreshes', async () => {
  const x = { id: 'od-x', upstream_id: 'vela/gpt-6-luna', reasoning_revision: 'rev-x' };
  const r = { id: 'od-r', upstream_id: 'vela/gpt-6-saturn', reasoning_revision: 'rev-r' };
  const posts = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        posts.push(JSON.parse(init.body));
        return respond(200, { models: [{ ...r, reasoning_revision: 'rev-r-2', reasoning_mapping: posts.at(-1).reasoning_mapping }] });
      }
      if (url.includes('/status')) return respond(200, reasoningStatus(x, r));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const orphanKey = 'od-x\u0000vela/gpt-6-luna';
  const savedKey = 'od-r\u0000vela/gpt-6-saturn';
  api.handlers.selectReasoningModel(orphanKey);
  await api.handlers.addReasoningRule();
  await api.handlers.changeReasoningRule(0, 'from', 'medium');
  await api.handlers.changeReasoningRule(0, 'to', 'max');
  api.handlers.selectReasoningModel(savedKey);
  await api.handlers.addReasoningRule();
  api.render.applyStatus(reasoningStatus(r)); // another tab removed X

  await api.handlers.saveReasoning();
  assert.equal(posts.length, 1);
  assert.equal(posts[0].model, 'od-r');
  assert.match(view.document.getElementById('reasoningState').textContent, /已保存 od-r/);
  const warning = view.document.getElementById('reasoningOrphans');
  const checkWarning = () => {
    assert.equal(warning.hidden, false);
    assert.match(warning.textContent, /草稿已保留/);
    assert.match(warning.textContent, /od-x → vela\/gpt-6-luna/);
    assert.match(warning.textContent, /medium→max/);
    assert.ok(!warning.textContent.includes(managementKey));
  };
  checkWarning();
  assert.equal(api.state.reasoningDrafts.has(orphanKey), true);
  assert.equal(api.state.reasoningDrafts.has(savedKey), false);
  assert.deepEqual(plain(api.state.reasoningDrafts.get(orphanKey).rules), [{ from: 'medium', to: 'max' }]);
  await api.handlers.addReasoningRule(); // routine edits must not erase the warning
  checkWarning();
  api.render.applyStatus(reasoningStatus(r)); // repeated refresh, X was already absent
  checkWarning();
  api.render.applyStatus(reasoningStatus(x, r)); // re-adding X makes its draft selectable
  assert.equal(warning.hidden, true);
  assert.equal(api.state.reasoningDrafts.has(orphanKey), true);
  api.handlers.selectReasoningModel(orphanKey);
  assert.deepEqual(plain(api.render.currentReasoningMapping(api.state.serverModels[0]).rules), [{ from: 'medium', to: 'max' }]);
  api.render.applyStatus(reasoningStatus(r));
  checkWarning();
  api.handlers.clearAuth();
  assert.equal(warning.hidden, true, 'logout clears notices belonging to the old session');
});

test('the model test sends a chosen strength and displays the applied conversion', async () => {
  const bodies = [];
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/model-test') && init.method === 'POST') {
        bodies.push(JSON.parse(init.body));
        return respond(200, {
          credential_id: 'cred-1', model_id: 'od-flash', status: 'success', upstream_attempted: true, http_status: 200,
          reasoning_effort: 'high', reasoning_mapping: { from: 'medium', to: 'high' },
          usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
        });
      }
      if (url.includes('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.handlers.populateReasoningInputs();
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const strength = view.document.getElementById('modelTestReasoning');
  assert.equal(strength.children[0].value, '', '未指定 stays the default so an existing test is unchanged');
  assert.equal(strength.children[0].textContent, '未指定');
  assert.ok(![...strength.children].some(option => option.value === 'unset'),
    'the configuration sentinel is not offered as a test input');

  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'od-flash';
  await api.handlers.runModelTest();
  assert.equal(Object.prototype.hasOwnProperty.call(bodies[0], 'reasoning_effort'), false,
    'an unspecified strength is omitted from the request body entirely');

  strength.value = 'medium';
  await api.handlers.runModelTest();
  assert.equal(bodies[1].reasoning_effort, 'medium', 'the chosen strength is sent as the input strength');
  const facts = view.document.getElementById('modelTestFacts').textContent;
  assert.match(facts, /思考强度/, 'the conversion is reported');
  assert.match(facts, /medium → high/);
});

test('an injected reasoning value in a model-test response is rendered as literal text', async () => {
  const injected = '‹img src=x onerror=alert(1)›';
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/model-test') && init.method === 'POST') {
        return respond(200, {
          credential_id: 'cred-1', model_id: 'od-flash', status: 'success',
          reasoning_effort: injected, reasoning_mapping: { from: injected, to: injected },
        });
      }
      if (url.includes('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'od-flash';
  await api.handlers.runModelTest();
  const facts = view.document.getElementById('modelTestFacts').textContent;
  assert.ok(!facts.includes(injected), 'a value outside the closed sets is dropped, not displayed');
  assert.ok(!facts.includes('思考强度'), 'no row is shown for an unrecognized strength');
});

test('a request-log row shows a mapped strength and stays quiet for a passthrough', async () => {
  const logs = {
    logs: [
      {
        id: 'log-1', started_at: '2026-11-05T01:00:00Z', status: 'success', model: 'od-flash',
        reasoning_effort: 'high', reasoning_mapping: { from: 'medium', to: 'high' },
      },
      { id: 'log-2', started_at: '2026-11-05T01:01:00Z', status: 'success', model: 'od-flash', reasoning_effort: 'none' },
      { id: 'log-3', started_at: '2026-11-05T01:02:00Z', status: 'success', model: 'od-flash' },
      {
        id: 'log-4', started_at: '2026-11-05T01:03:00Z', status: 'success', model: 'od-flash',
        reasoning_effort: '‹b›injected‹/b›', reasoning_mapping: { from: '‹b›', to: '‹b›' },
      },
    ],
    total: 4,
  };
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, statusBody()) : respond(200, logs)),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.loadRequestLogs({ reset: true });
  const rows = view.document.getElementById('requestLogList').children;
  assert.match(rows[0].textContent, /思考强度 medium → high/);
  assert.match(rows[1].textContent, /思考强度 none/, 'a passthrough with an explicit strength is still reported');
  assert.ok(!/思考强度/.test(rows[2].textContent), 'a request with no strength shows no row');
  assert.ok(!rows[3].textContent.includes('injected'), 'an injected value outside the closed sets is dropped');
});

test('request-log rows keep a dense summary and move identities into an expandable detail', async () => {
  const logs = {
    logs: [
      {
        id: 'log-1', started_at: '2026-11-05T01:00:00Z', finished_at: '2026-11-05T01:00:01.234Z',
        status: 'error', http_status: 402, model: 'od-flash', upstream_model: 'deepseek-v4.1-flash',
        credential_id: 'cred-1', duration_ms: 1234, input_tokens: 1000, output_tokens: 234,
        total_tokens: 1234, request_id: 'req-abc', error_kind: 'insufficient_balance',
        stream: true, upstream_attempted: true,
      },
      {
        id: 'log-2', started_at: '2026-11-05T01:01:00Z', status: 'error', model: 'od-flash',
        credential_id: 'cred-1', error_kind: 'model_not_enabled', upstream_attempted: false,
      },
    ],
    total: 2,
  };
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, statusBody()) : respond(200, logs)),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.loadRequestLogs({ reset: true });
  const list = view.document.getElementById('requestLogList');
  const rows = list.children;
  assert.equal(rows[0].tagName, 'DETAILS', 'each row is expandable');
  assert.match(rows[0].className, /failed/, 'a failed request row is toned red');
  const summary = rows[0].children[0];
  assert.match(summary.textContent, /额度耗尽/, 'a known error kind becomes the result label');
  assert.match(summary.textContent, /HTTP 402/);
  assert.match(summary.textContent, /deepseek-v4\.1-flash/);
  assert.match(summary.textContent, /主账户/);
  assert.match(summary.textContent, /1\.23 s/);
  assert.match(summary.textContent, /1,234/);
  assert.ok(!summary.textContent.includes('req-abc'), 'the request id lives in the detail, not the dense summary');
  const detail = rows[0].children[1];
  assert.match(detail.textContent, /req-abc/);
  assert.match(detail.textContent, /输入 Token 1,000/);
  assert.match(detail.textContent, /输出 Token 234/);
  assert.match(rows[1].children[0].textContent, /本地拒绝/);

  rows[0].open = true;
  await rows[0].emit('toggle');
  api.render.renderRequestLogs();
  assert.equal(list.children[0].open, true, 'the expansion survives a countdown-style repaint');
});

test('request-log filters send free-text search and fold local rejection into attempted=no', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/request-logs')
      ? respond(200, { logs: [], total: 0, limit: 25, offset: 0, has_more: false })
      : respond(200, statusBody())),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  view.document.getElementById('logsFilterSearch').value = '  DeepSeek  ';
  view.document.getElementById('logsFilterStatus').value = 'local';
  view.document.getElementById('logsFilterLimit').value = '25';
  await api.handlers.loadRequestLogs({ reset: true });
  const request = view.calls.find(call => call.url.includes('/request-logs')).url;
  assert.match(request, /q=DeepSeek/);
  assert.match(request, /attempted=no/);
  assert.match(request, /limit=25/);
  assert.ok(!/[?&]status=/.test(request), 'a local-rejection filter does not also constrain status');
  assert.equal(api.state.logQuery.search, 'DeepSeek');
  assert.equal(api.state.logQuery.attempted, 'no');
});

test('opening the logs tab reads one page and the summary once per session', async () => {
  let logCalls = 0;
  let statsCalls = 0;
  const view = fakePage({
    fetchImpl: async url => {
      if (url.includes('/request-stats')) {
        statsCalls += 1;
        return respond(200, {
          total: 1, success: 1, failed: 0, streams: 0, success_rate: 1,
          tokens: { input: 2, output: 3, total: 5, requests_with_usage: 1, requests_without_usage: 0 },
          models: [{ model: 'od-flash', total: 1, success: 1, failed: 0 }],
          retention: { enabled: true, retention_days: 7, max_entries: 500, entries: 1 },
        });
      }
      if (url.includes('/request-logs')) {
        logCalls += 1;
        return respond(200, { logs: [], total: 0, limit: 50, offset: 0, has_more: false });
      }
      return respond(200, statusBody());
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const settle = () => new Promise(resolve => setTimeout(resolve, 0));
  api.handlers.activateTab('logs');
  await settle();
  assert.equal(logCalls, 1, 'opening the logs tab loads the first page');
  assert.equal(statsCalls, 1, 'opening the logs tab loads the summary cards');
  api.handlers.activateTab('overview');
  api.handlers.activateTab('logs');
  await settle();
  assert.equal(logCalls, 1, 'switching back does not re-read a loaded page');
  assert.equal(statsCalls, 1, 'the summary is not re-read either');
  await api.handlers.refreshRequestLogs();
  assert.equal(logCalls, 2, 'the refresh button always reads again');
  assert.equal(statsCalls, 2);
  assert.match(view.document.getElementById('requestStatsModelsSummary').textContent, /按模型分布（1）/);
});

test('log formatting keeps seconds, humanises latency, and groups token counts', () => {
  const core = fakePage().context.OpenDesignCore;
  const now = new Date(2026, 10, 6, 12, 0, 0).getTime();
  assert.equal(core.formatLogTime(new Date(2026, 10, 6, 9, 5, 7).toISOString(), now), '09:05:07');
  assert.equal(core.formatLogTime(new Date(2026, 10, 5, 23, 59, 59).toISOString(), now), '11-05 23:59:59');
  assert.equal(core.formatLogTime(new Date(2025, 11, 31, 0, 0, 0).toISOString(), now), '2025-12-31 00:00:00');
  assert.equal(core.formatLogTime('not-a-time', now), '');
  assert.equal(core.formatLogDuration(999), '999 ms');
  assert.equal(core.formatLogDuration(1234), '1.23 s');
  assert.equal(core.formatLogDuration(12500), '12.5 s');
  assert.equal(core.formatLogDuration(65000), '1 分 5 秒');
  assert.equal(core.formatLogDuration(null), '');
  assert.equal(core.formatCount(1234567), '1,234,567');
  assert.equal(core.formatCount(null), '');
});

test('reset uses the model-derived default table and new rules pick a legal target', async () => {
  const core = fakePage().context.OpenDesignCore;
  // A source that is not itself a legal target (ultra) falls back to a real option.
  assert.equal(core.defaultReasoningTarget('ultra'), 'max');
  assert.equal(core.defaultReasoningTarget('high'), 'high');
  assert.equal(core.defaultReasoningTarget(''), 'max');

  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, reasoningStatus(
      { id: 'od-deepseek', upstream_id: 'deepseek-v4.1-flash' },
      { id: 'od-other', upstream_id: 'vela/gpt-6-luna' },
    )) : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const select = view.document.getElementById('reasoningModel');

  // A non-DeepSeek model must not receive the DeepSeek table on reset.
  api.handlers.selectReasoningModel(select.children[1].value);
  await api.handlers.addReasoningRule();
  assert.equal(api.render.reasoningDirty(), true, 'the added row is an unsaved change');
  await api.handlers.resetReasoningTable();
  assert.equal(api.render.reasoningDirty(), false, 'reset on a non-DeepSeek model returns to its empty default');
  assert.equal(view.document.getElementById('reasoningRows').children.length, 1,
    'the empty-state row is shown, not a fabricated vendor table');

  // DeepSeek still gets its vendor table back on reset.
  api.handlers.selectReasoningModel(select.children[0].value);
  await api.handlers.setReasoningEnabled(true);
  await api.handlers.resetReasoningTable();
  assert.equal(api.render.reasoningDirty(), true, 'the enable toggle remains an unsaved change');
  assert.equal(plain(api.render.currentReasoningMapping(api.state.serverModels[0])).rules.length, 9);
});

test('a newly added rule keeps a legal target even when the source is ultra', async () => {
  const models = [{
    id: 'od-flash', upstream_id: 'open-design/flash',
    reasoning_mapping: {
      enabled: true,
      rules: ['unset', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].map(from => ({ from, to: from })),
    },
  }];
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, reasoningStatus(...models)) : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();
  const added = plain(api.render.currentReasoningMapping(api.state.serverModels[0])).rules.find(rule => rule.from === 'ultra');
  assert.ok(added, 'the only unused source is ultra');
  assert.equal(added.to, 'max', 'ultra maps to a target the picker actually offers');
  assert.equal(api.render.reasoningDirty(), true, 'the unsaved ultra row survives the dirty check');
  assert.equal(view.document.getElementById('reasoningRows').children.length, 9);
});

test('a reasoning save disables every row control and restores them afterwards', async () => {
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        await gate;
        return respond(200, { models: [{ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } }] });
      }
      if (url.includes('/status')) return respond(200, reasoningStatus({ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const controls = () => [...view.document.getElementById('reasoningRows').children]
    .flatMap(row => [...row.children].flatMap(cell => [...cell.children]));
  const pending = api.handlers.saveReasoning();
  assert.equal(api.state.reasoningSaving, true);
  assert.ok(controls().filter(node => node.tagName === 'SELECT').every(node => node.disabled === true),
    'row selects are disabled while the save is in flight');
  assert.ok(controls().filter(node => node.tagName === 'BUTTON').every(node => node.disabled === true),
    'row delete buttons are disabled while the save is in flight');
  release();
  await pending;
  assert.equal(api.state.reasoningSaving, false);
  assert.ok(controls().filter(node => node.tagName === 'SELECT').every(node => node.disabled === false),
    'row selects recover after the save');
  assert.ok(controls().filter(node => node.tagName === 'BUTTON').every(node => node.disabled === false),
    'row delete buttons recover after the save');
});

test('the model test reports a low→unset conversion even with no sent strength', async () => {
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/model-test') && init.method === 'POST') {
        return respond(200, {
          credential_id: 'cred-1', model_id: 'od-flash', status: 'success', upstream_attempted: true, http_status: 200,
          reasoning_mapping: { from: 'low', to: 'unset' },
        });
      }
      if (url.includes('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  view.document.getElementById('modelTestCredential').value = 'cred-1';
  view.document.getElementById('modelTestModel').value = 'od-flash';
  await api.handlers.runModelTest();
  const facts = view.document.getElementById('modelTestFacts').textContent;
  assert.match(facts, /思考强度/, 'the conversion is shown even when the sent strength is empty');
  assert.match(facts, /low → 未设置/);
});

test('a reasoning save carries expected_revision only when the server advertises one', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        calls.push(JSON.parse(init.body));
        return respond(200, { models: [] });
      }
      if (url.includes('/status')) return respond(200, reasoningStatus({
        id: 'od-flash', upstream_id: 'open-design/flash', reasoning_revision: 'rev-7',
        reasoning_mapping: { enabled: true, rules: [] },
      }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-7', 'the opaque revision is preserved');
  await api.handlers.addReasoningRule();
  await api.handlers.saveReasoning();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].expected_revision, 'rev-7', 'the selected model revision is sent for optimistic concurrency');
  assert.equal(calls[0].model, 'od-flash');
});

test('a reasoning save omits expected_revision for an older backend', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        calls.push(JSON.parse(init.body));
        return respond(200, { models: [] });
      }
      if (url.includes('/status')) return respond(200, reasoningStatus({
        id: 'od-flash', upstream_id: 'open-design/flash',
        reasoning_mapping: { enabled: true, rules: [] },
      }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  assert.equal(api.state.serverModels[0].reasoning_revision, '', 'an absent revision stays empty');
  await api.handlers.addReasoningRule();
  await api.handlers.saveReasoning();
  assert.equal(calls.length, 1);
  assert.equal(Object.prototype.hasOwnProperty.call(calls[0], 'expected_revision'), false,
    'the body stays byte-identical to the old shape when no revision exists');
});

test('a status refresh discards an in-flight reasoning save response', async () => {
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        await gate;
        return respond(200, { models: [{ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } }] });
      }
      if (url.includes('/status')) return respond(200, reasoningStatus({ id: 'od-flash', upstream_id: 'open-design/flash' }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();
  const pending = api.handlers.saveReasoning();
  assert.equal(api.state.reasoningSaving, true);
  // A refresh bumps the generation while the save is still in flight, but the
  // POST owns the lock: a status answer must not unlock the rows early.
  api.render.applyStatus(reasoningStatus({ id: 'od-flash', upstream_id: 'open-design/flash' }));
  assert.equal(api.state.reasoningSaving, true, 'the refresh does not release the saving lock');
  assert.match(view.document.getElementById('reasoningState').textContent, /未保存草稿已保留/);
  release();
  await pending;
  assert.equal(api.state.reasoningSaving, false, 'the in-flight save releases its own lock');
  assert.equal(api.render.reasoningDirty(), true, 'the unsaved draft survives; the stale answer did not adopt the server policy');
  const draftRules = plain(api.render.currentReasoningMapping(api.state.serverModels[0])).rules;
  assert.equal(draftRules.length, 1, 'the stale response never replaced the local draft');
  assert.notEqual(JSON.stringify(draftRules), JSON.stringify([{ from: 'medium', to: 'high' }]),
    'the superseded server policy was not applied');
  assert.equal(api.state.reasoningSaving, false);
  assert.ok(!/已保存/.test(view.document.getElementById('reasoningState').textContent),
    'a response for the superseded generation is not announced as a fresh save');
});

test('a status refresh keeps the draft on its first-edit revision and refuses a silent overwrite', async () => {
  const calls = [];
  const model = (revision, mapping) => ({
    id: 'od-flash', upstream_id: 'open-design/flash',
    reasoning_revision: revision, reasoning_mapping: mapping,
  });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        const body = JSON.parse(init.body);
        calls.push(body);
        if (body.expected_revision === 'rev-0') return respond(409, { error: 'reasoning_revision_conflict' });
        return respond(200, { models: [model('rev-2', { enabled: true, rules: [{ from: 'medium', to: 'high' }] })] });
      }
      if (url.includes('/status')) {
        return respond(200, reasoningStatus(model('rev-0', { enabled: true, rules: [{ from: 'medium', to: 'high' }] })));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();
  const firstDraft = api.state.reasoningDrafts.get(api.state.reasoningModelKey);
  assert.equal(firstDraft.baseRevision, 'rev-0', 'the draft captures the revision it was edited against');

  // Another tab saves while this page's draft is open: the status refresh moves
  // the server model to rev-1 but must not move the draft's own base revision.
  api.render.applyStatus(reasoningStatus(model('rev-1', { enabled: true, rules: [{ from: 'low', to: 'low' }] })));
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-1');
  assert.equal(api.state.reasoningDrafts.get(api.state.reasoningModelKey).baseRevision, 'rev-0');
  assert.equal(api.render.reasoningDirty(), true, 'the unsaved draft is still visible');

  // A plain save sends the draft's own revision (r0), so the backend can reject
  // the competing tab's change instead of overwriting it with r1.
  await api.handlers.saveReasoning();
  assert.equal(calls.length, 1, 'the draft is posted with its original base revision');
  assert.equal(calls[0].expected_revision, 'rev-0');
  assert.match(view.document.getElementById('reasoningState').textContent, /草稿已保留/);
  assert.equal(api.render.reasoningDirty(), true, 'the 409 keeps the draft');
  assert.equal(api.state.reasoningDrafts.get(api.state.reasoningModelKey).baseRevision, 'rev-0');

  // The second explicit click is the operator's overwrite decision and uses the
  // revision just observed, so the backend can still CAS-check it.
  await api.handlers.saveReasoning();
  assert.equal(calls.length, 2);
  assert.equal(calls[1].expected_revision, 'rev-1');
  assert.equal(api.render.reasoningDirty(), false, 'the confirmed save clears the draft');
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-2');
});

test('saving the model list keeps each reasoning revision for the next CAS save', async () => {
  const reasoningCalls = [];
  const model = (revision, mapping) => ({
    id: 'od-flash', upstream_id: 'open-design/flash',
    reasoning_revision: revision, reasoning_mapping: mapping,
  });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        return respond(200, { models: [model('rev-8', { enabled: true, rules: [] })] });
      }
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        reasoningCalls.push(JSON.parse(init.body));
        return respond(200, { models: [model('rev-9', { enabled: true, rules: [{ from: 'medium', to: 'high' }] })] });
      }
      if (url.includes('/status')) return respond(200, reasoningStatus(model('rev-7', { enabled: true, rules: [] })));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  api.state.mappings = [{ id: 'od-flash', upstream_id: 'open-design/flash' }];
  await api.handlers.saveModels();
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-8',
    'the model-list response revision is not discarded');
  assert.equal(api.state.serverModels[0].reasoning_mapping.enabled, true);

  await api.handlers.addReasoningRule();
  await api.handlers.saveReasoning();
  assert.equal(reasoningCalls.length, 1);
  assert.equal(reasoningCalls[0].expected_revision, 'rev-8',
    'a mapping save after a model-list save still carries a CAS token');
});

test('an unrelated busy transition never unlocks reasoning rows mid-save', async () => {
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        await gate;
        return respond(200, { models: [{ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_revision: 'rev-2', reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } }] });
      }
      if (url.includes('/upstream-models')) return respond(200, { data: [{ id: 'open-design/flash' }] });
      if (url.includes('/status')) return respond(200, reasoningStatus({ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_revision: 'rev-1', reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();
  const rowButtons = () => [...view.document.getElementById('reasoningRows').children]
    .flatMap(row => [...row.children].flatMap(cell => [...cell.children]))
    .filter(node => node.tagName === 'BUTTON');
  const pending = api.handlers.saveReasoning();
  assert.ok(rowButtons().every(node => node.disabled === true), 'rows are locked by the reasoning save');

  // An unrelated operation starts and releases the shared busy flag. That must
  // not re-enable the delete buttons while the reasoning response is pending.
  api.state.selectedCredential = 'cred-1';
  view.document.getElementById('modelCredential').value = 'cred-1';
  await api.handlers.fetchModels();
  assert.equal(api.state.reasoningSaving, true);
  assert.ok(rowButtons().every(node => node.disabled === true),
    'a setBusy(false) from another operation leaves the saving rows locked');

  release();
  await pending;
  assert.ok(rowButtons().every(node => node.disabled === false), 'the rows recover once the save finishes');
});

test('a model-list save waits instead of racing a pending reasoning POST', async () => {
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const models = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        models.push(JSON.parse(init.body));
        return respond(200, { models: [{ id: 'new-alias', upstream_id: 'open-design/new' }] });
      }
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        await gate;
        return respond(200, { models: [{ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_revision: 'rev-1', reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } }] });
      }
      if (url.includes('/status')) return respond(200, reasoningStatus({ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_revision: 'rev-0' }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();
  api.state.mappings = [{ id: 'new-alias', upstream_id: 'open-design/new' }];
  const pending = api.handlers.saveReasoning();

  await api.handlers.saveModels();
  assert.equal(models.length, 0, 'the model-list POST is not sent while reasoning is saving');
  assert.match(view.document.getElementById('modelState').textContent, /思考强度映射正在保存/);

  release();
  await pending;
  // The delayed reasoning answer is the only applied list, so the newer model
  // list could not be overwritten by an out-of-order response.
  assert.equal(api.state.serverModels.length, 1);
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-1');
  assert.match(view.document.getElementById('modelState').textContent, /思考强度映射正在保存/);
});

test('an old status answer cannot roll a newer reasoning save back', async () => {
  const pendingStatus = deferred();
  let statusCalls = 0;
  const model = (revision, mapping) => ({
    id: 'od-flash', upstream_id: 'open-design/flash',
    reasoning_revision: revision, reasoning_mapping: mapping,
  });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.includes('/status')) {
        statusCalls += 1;
        if (statusCalls === 1) {
          return respond(200, reasoningStatus(model('rev-0', { enabled: true, rules: [{ from: 'medium', to: 'high' }] })));
        }
        return pendingStatus.promise;
      }
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        return respond(200, { models: [model('rev-1', { enabled: true, rules: [{ from: 'medium', to: 'high' }, { from: 'low', to: 'low' }] })] });
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();

  // A status read starts before the save, so its answer is older than the save.
  const staleStatus = api.render.loadStatus();
  await new Promise(setImmediate);
  await api.handlers.saveReasoning();
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-1');
  assert.match(view.document.getElementById('reasoningState').textContent, /已保存/);

  pendingStatus.resolve(respond(200, reasoningStatus(model('rev-0', { enabled: true, rules: [{ from: 'medium', to: 'high' }] }))));
  await staleStatus;
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-1',
    'the old status snapshot did not roll the saved revision back');
  assert.deepEqual(plain(api.render.currentReasoningMapping(api.state.serverModels[0])).rules,
    [{ from: 'low', to: 'low' }, { from: 'medium', to: 'high' }],
    'the saved policy stays visible instead of reverting to the stale snapshot');
  assert.match(view.document.getElementById('reasoningState').textContent, /已保存/,
    'the success message is not contradicted by a stale status answer');
});

test('the model-list token is pinned at draft start and status refreshes do not reset it', async () => {
  const posts = [];
  let currentRevision = 'list-rev-1';
  const models = () => [{ id: 'od-flash', upstream_id: 'open-design/flash' }];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        const body = JSON.parse(init.body);
        posts.push(body);
        if (body.expected_models_revision === 'list-rev-1') return respond(409, { error: 'models_revision_conflict' });
        return respond(200, { models: models(), models_revision: 'list-rev-3' });
      }
      if (url.includes('/status')) return respond(200, statusBody({ models: models(), models_revision: currentRevision }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  assert.equal(api.state.modelsRevision, 'list-rev-1', 'the status token is kept');

  // The first edit pins the token the draft was started against.
  api.handlers.addMapping();
  assert.equal(api.state.modelsDraftActive, true);
  assert.equal(api.state.modelsDraftRevision, 'list-rev-1');
  api.state.mappings[1].id = 'od-pro';
  api.state.mappings[1].upstream_id = 'open-design/pro';

  // A status refresh moves the server forward but must not move the draft base.
  currentRevision = 'list-rev-2';
  api.render.applyStatus(statusBody({ models: models(), models_revision: 'list-rev-2' }));
  assert.equal(api.state.modelsRevision, 'list-rev-2');
  assert.equal(api.state.modelsDraftRevision, 'list-rev-1', 'the draft keeps its first-edit token');
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['od-flash', 'od-pro']);

  // The first save posts the pinned token; the server rejects the competing tab.
  await api.handlers.saveModels();
  assert.equal(posts.length, 1);
  assert.equal(posts[0].expected_models_revision, 'list-rev-1');
  assert.match(view.document.getElementById('modelState').textContent, /提交内容已保留/);
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['od-flash', 'od-pro'], 'the rejected content stays editable');

  // The second explicit click is the overwrite decision and uses the refreshed token.
  await api.handlers.saveModels();
  assert.equal(posts.length, 2);
  assert.equal(posts[1].expected_models_revision, 'list-rev-2');
  assert.equal(api.state.modelsRevision, 'list-rev-3', 'the response token becomes the new baseline');
  assert.equal(api.state.modelsDraftActive, false);
});

test('a draft begun before the backend advertised models_revision needs an explicit overwrite', async () => {
  const posts = [];
  let revision = '';
  const models = () => [{ id: 'od-flash', upstream_id: 'open-design/flash' }];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        posts.push(JSON.parse(init.body));
        return respond(200, { models: models(), models_revision: 'list-rev-9' });
      }
      if (url.includes('/status')) return respond(200, statusBody({ models: models(), models_revision: revision }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  assert.equal(api.state.modelsRevision, '', 'the old backend sends no token');
  api.handlers.addMapping();
  api.state.mappings[1].id = 'od-pro';
  api.state.mappings[1].upstream_id = 'open-design/pro';

  // The backend starts providing a token after the draft was begun.
  revision = 'list-rev-9';
  api.render.applyStatus(statusBody({ models: models(), models_revision: 'list-rev-9' }));
  assert.equal(api.state.modelsDraftRevision, '', 'the draft stays tokenless until it is explicitly re-based');

  await api.handlers.saveModels();
  assert.equal(posts.length, 0, 'a tokenless draft is not silently posted once the backend provides a token');
  assert.match(view.document.getElementById('modelState').textContent, /再次点击/);
  assert.equal(api.state.modelsConflict, true);

  await api.handlers.saveModels();
  assert.equal(posts.length, 1);
  assert.equal(posts[0].expected_models_revision, 'list-rev-9', 'the confirmed save uses the current server token');
});

test('a rejected model-list save keeps its content and never skips confirmation by reverting rows', async () => {
  const posts = [];
  let confirmations = 0;
  let allowOverwrite = false;
  const models = () => [{ id: 'od-flash', upstream_id: 'open-design/flash' }];
  let statusCalls = 0;
  const view = fakePage({
    confirmImpl: () => { confirmations += 1; return allowOverwrite; },
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        const body = JSON.parse(init.body);
        posts.push(body);
        if (body.expected_models_revision === 'list-rev-1') return respond(409, { error: 'models_revision_conflict' });
        return respond(200, { models: models(), models_revision: 'list-rev-3' });
      }
      if (url.includes('/status')) {
        statusCalls += 1;
        return respond(200, statusBody({ models: models(), models_revision: statusCalls === 1 ? 'list-rev-1' : 'list-rev-2' }));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();

  // A clean save (no draft) still carries the token and the server has moved on.
  await api.handlers.saveModels();
  assert.equal(posts.length, 1);
  assert.equal(posts[0].expected_models_revision, 'list-rev-1');
  assert.match(view.document.getElementById('modelState').textContent, /提交内容已保留/);
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['od-flash']);

  // A refresh shows the competing revision. The submitted content now equals the
  // old baseline again, but the conflict flag must still demand confirmation.
  api.render.applyStatus(statusBody({ models: models(), models_revision: 'list-rev-2' }));
  assert.equal(api.mappingsDirty(), false, 'the rows match the old baseline by content');
  await api.handlers.saveModels();
  assert.equal(confirmations, 1, 'the confirmation is asked even though the content hash matches');
  assert.equal(posts.length, 1, 'a declined confirmation sends nothing');
  assert.equal(view.document.getElementById('modelState').textContent.includes('已取消保存'), true);

  allowOverwrite = true;
  await api.handlers.saveModels();
  assert.equal(confirmations, 2);
  assert.equal(posts.length, 2);
  assert.equal(posts[1].expected_models_revision, 'list-rev-2');
});

test('model rows lock during a save and a racing edit survives the response', async () => {
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        await gate;
        return respond(200, { models: [{ id: 'saved', upstream_id: 'open-design/saved' }], models_revision: 'list-rev-2' });
      }
      if (url.includes('/upstream-models')) return respond(200, { data: [{ id: 'open-design/flash' }] });
      if (url.includes('/status')) return respond(200, statusBody({ models: [{ id: 'od-flash', upstream_id: 'open-design/flash' }], models_revision: 'list-rev-1' }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  api.state.selectedCredential = 'cred-1';
  view.document.getElementById('modelCredential').value = 'cred-1';
  await api.handlers.fetchModels();
  const upstreamCheckbox = Array.from(view.document.getElementById('upstreamList').children)
    .flatMap(row => Array.from(row.children || []))
    .find(node => node.tagName === 'INPUT');
  assert.equal(upstreamCheckbox.disabled, false);

  const pending = api.handlers.saveModels();
  const aliasInput = view.document.getElementById('mappingList').children[0].children[0].children[1];
  assert.equal(aliasInput.disabled, true, 'mapping inputs are locked while the save is in flight');
  assert.equal(upstreamCheckbox.disabled, true, 'upstream checkboxes are locked while the save is in flight');

  // The lock blocks user edits, but a racing programmatic edit must not be lost.
  api.state.mappings[0].id = 'edited-during-save';
  release();
  await pending;
  assert.equal(api.state.mappings[0].id, 'edited-during-save', 'the racing edit is kept as a draft');
  assert.deepEqual(plain(api.state.savedMappings.map(row => row.id)), ['saved']);
  assert.match(view.document.getElementById('modelState').textContent, /草稿/);
  const freshAliasInput = view.document.getElementById('mappingList').children[0].children[0].children[1];
  const freshUpstreamCheckbox = Array.from(view.document.getElementById('upstreamList').children)
    .flatMap(row => Array.from(row.children || []))
    .find(node => node.tagName === 'INPUT');
  assert.equal(freshAliasInput.disabled, false, 'inputs unlock once the save is over');
  assert.equal(freshUpstreamCheckbox.disabled, false, 'checkboxes unlock once the save is over');
});

test('a status refresh landing during a model-list save cannot replace the rows', async () => {
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  let statusCalls = 0;
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        await gate;
        return respond(200, { models: [{ id: 'saved', upstream_id: 'open-design/saved' }], models_revision: 'list-rev-2' });
      }
      if (url.includes('/status')) {
        statusCalls += 1;
        return respond(200, statusBody({
          models: statusCalls === 1
            ? [{ id: 'od-flash', upstream_id: 'open-design/flash' }]
            : [{ id: 'server-new', upstream_id: 'open-design/server-new' }],
          models_revision: statusCalls === 1 ? 'list-rev-1' : 'list-rev-9',
        }));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  api.handlers.addMapping();
  api.state.mappings[1].id = 'od-pro';
  api.state.mappings[1].upstream_id = 'open-design/pro';

  const pending = api.handlers.saveModels();
  assert.equal(api.state.modelsSaving, true);
  // A refresh answer lands while the POST is still pending: its model side must
  // not replace the rows the response is about to confirm.
  api.render.applyStatus(statusBody({ models: [{ id: 'server-new', upstream_id: 'open-design/server-new' }], models_revision: 'list-rev-9' }));
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['od-flash', 'od-pro']);
  assert.equal(api.state.modelsRevision, 'list-rev-1', 'the pending save owns the revision until it answers');

  release();
  await pending;
  assert.equal(api.state.modelsSaving, false);
  assert.deepEqual(plain(api.state.savedMappings.map(row => row.id)), ['saved'], 'the confirmed response is the new baseline');
  assert.equal(api.state.modelsRevision, 'list-rev-2');
  assert.equal(api.state.modelsDraftActive, false);
});

test('a 401 during a model save leaves the auth form usable for re-verification', async () => {
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  let statusCalls = 0;
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        await gate;
        return respond(401, { error: 'management key expired' });
      }
      if (!url.includes('/status')) return respond(404, { error: 'not found' });
      statusCalls += 1;
      // Re-verification with a good key succeeds.
      return respond(200, statusBody({ models_revision: `rev-${statusCalls}` }));
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const pending = api.handlers.saveModels();
  assert.equal(api.state.busy, true);

  // Release the save first: its 401 calls showAuth, which resets the old
  // session and must release the busy lock it was holding.
  release();
  await pending;
  assert.equal(api.state.verified, null, 'the 401 ends the old session');
  assert.equal(api.state.busy, false, 'showAuth releases the model-save busy lock');
  assert.equal(view.document.getElementById('authSubmit').disabled, false, 'the auth button is re-enabled');

  view.document.getElementById('managementKey').value = 'replacement-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.equal(api.state.verified && api.state.verified.source, 'manual', 're-verification succeeds');
  assert.match(view.document.getElementById('authState').textContent, /验证成功/);
});

test('re-authentication works while a model POST from the old session is still pending', async () => {
  let releaseSave;
  const saveGate = new Promise(resolve => { releaseSave = resolve; });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        await saveGate;
        return respond(200, { models: [{ id: 'od-flash', upstream_id: 'open-design/flash' }], models_revision: 'saved-rev' });
      }
      if (url.includes('/status')) return respond(200, statusBody({ models_revision: 'fresh-rev' }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const pending = api.handlers.saveModels();
  assert.equal(api.state.busy, true);

  // A different request returns 401 and calls showAuth while the model POST is
  // still in flight. The auth form must be usable immediately.
  api.render.showAuth('401：管理密钥无效或已过期，请重新输入当前 CPA 入口的管理密钥。');
  assert.equal(api.state.busy, false);
  assert.equal(view.document.getElementById('authSubmit').disabled, false);
  view.document.getElementById('managementKey').value = 'replacement-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.equal(api.state.verified && api.state.verified.source, 'manual', 'the new key is accepted');
  assert.equal(api.state.modelsRevision, 'fresh-rev');

  // The stale POST answer must not apply its old model list or release the new
  // session's state.
  releaseSave();
  await pending;
  assert.equal(api.state.modelsRevision, 'fresh-rev');
  assert.equal(api.state.busy, false);
  assert.equal(api.state.verified && api.state.verified.source, 'manual');
});

test('a synthetic account result keeps the models_revision won by a model save', async () => {
  const posts = [];
  let statusCalls = 0;
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/credentials') && init.method === 'POST') return respond(200, { id: 'cred-1', label: '主账户', disabled: false });
      if (url.endsWith('/models') && init.method === 'POST') {
        posts.push(JSON.parse(init.body));
        return respond(200, { models: [{ id: 'od-flash', upstream_id: 'open-design/flash' }], models_revision: 'list-rev-8' });
      }
      if (url.includes('/status')) {
        statusCalls += 1;
        // After the account save the follow-up status read fails, so the
        // synthetic acknowledgement must carry its own (fresh) token.
        if (statusCalls === 1) return respond(200, statusBody({ models_revision: 'list-rev-7' }));
        return respond(500, { error: 'status unavailable' });
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.saveModels();
  assert.equal(api.state.modelsRevision, 'list-rev-8');

  // An acknowledged account edit builds a synthetic status from state.status.
  // It must not roll the token back to the older status snapshot.
  view.document.getElementById('accountLabel').value = '主账户';
  view.document.getElementById('accountKey').value = key;
  await api.handlers.addAccount({ preventDefault() {} });
  assert.equal(api.state.modelsRevision, 'list-rev-8', 'the synthetic status keeps the fresh token');

  // The next save must still be CAS-protected rather than fall back to no lock.
  await api.handlers.saveModels();
  assert.equal(posts.length, 2);
  assert.equal(posts[1].expected_models_revision, 'list-rev-8');
});

test('editing model rows back to the saved values releases the old-backend draft', () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, statusBody({ models_revision: '' })) : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  api.render.applyStatus(statusBody({ models: [{ id: 'od-flash', upstream_id: 'open-design/flash' }], models_revision: '' }));
  const alias = view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'renamed';
  alias.emit('input');
  assert.equal(api.state.modelsDraftActive, true);
  assert.equal(api.mappingsDirty(), true);

  // Typing the old value back on an old backend releases the draft, so a later
  // refresh may adopt the new server list instead of preserving a stale draft.
  alias.value = 'od-flash';
  alias.emit('input');
  assert.equal(api.state.modelsDraftActive, false);
  assert.equal(api.mappingsDirty(), false);

  api.render.applyStatus(statusBody({ models: [{ id: 'server-new', upstream_id: 'open-design/server-new' }], models_revision: '' }));
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['server-new']);
});

test('re-validating the same management key keeps unsaved drafts', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status')
      ? respond(200, statusBody({
        credentials: [{ id: 'cred-1', label: '主账户', disabled: false }],
        models: [{ id: 'od-flash', upstream_id: 'open-design/flash' }],
        models_revision: 'list-rev-1',
      }))
      : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'session' };
  await api.render.loadStatus();
  api.handlers.addMapping();
  assert.equal(api.state.modelsDraftActive, true);
  const draftIds = plain(api.state.mappings.map(row => row.id));

  view.document.getElementById('managementKey').value = managementKey;
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.equal(api.state.modelsDraftActive, true, 'the same key is the same session');
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), draftIds, 'the draft survives re-validation');
  assert.equal(api.state.modelsDraftRevision, 'list-rev-1');

  // A different key is a new identity and clears it.
  view.document.getElementById('managementKey').value = 'another-management-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.equal(api.state.modelsDraftActive, false, 'a different key clears the draft');
});

test('switching the management key clears unsubmitted api_key and control_key fields', async () => {
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status')
      ? respond(200, statusBody({ credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }] }))
      : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const row = accountRows(view)[0];
  api.handlers.editAccount(api.state.status.credentials[0]);
  assert.ok(row);
  view.document.getElementById('accountKey').value = 'unsubmitted-api-key';
  view.document.getElementById('accountControlKey').value = 'unsubmitted-control-key';
  view.document.getElementById('accountWorkspaceID').value = 'ws-1';

  view.document.getElementById('managementKey').value = 'different-management-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.equal(view.document.getElementById('accountKey').value, '', 'the api_key input is cleared');
  assert.equal(view.document.getElementById('accountControlKey').value, '', 'the control_key input is cleared');
  assert.equal(view.document.getElementById('accountWorkspaceID').value, '');
  assert.ok(!JSON.stringify(api.state).includes('unsubmitted-api-key'));
  assert.ok(!JSON.stringify(api.state).includes('unsubmitted-control-key'));
});

test('switching the management key invalidates in-flight quota requests and their 401s', async () => {
  const pendingQuota = deferred();
  let statusCalls = 0;
  const view = fakePage({
    fetchImpl: async url => {
      if (url.includes('/quota')) return pendingQuota.promise;
      if (!url.includes('/status')) return respond(404, { error: 'not found' });
      statusCalls += 1;
      return respond(200, statusBody({
        credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
        models_revision: `rev-${statusCalls}`,
      }));
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const flight = api.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  await new Promise(setImmediate);
  assert.equal(api.state.quotaBusy['cred-1'], true);

  view.document.getElementById('managementKey').value = 'different-management-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.notEqual(api.state.verified, null, 'the new key is verified');
  pendingQuota.resolve(respond(401, { error: 'old key expired' }));
  await flight;
  assert.notEqual(api.state.verified, null, 'the old quota 401 does not log the new session out');
  assert.equal(api.state.quotaBusy['cred-1'], undefined, 'the old busy marker is gone with its session');
  assert.equal(view.document.getElementById('accountsPanel').hidden, false);
});

test('a reasoning success adopts a new models_revision for later model-list CAS', async () => {
  const modelPosts = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        return respond(200, {
          models: [{ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_revision: 'r-2', reasoning_mapping: { enabled: true, rules: [] } }],
          models_revision: 'list-rev-5',
        });
      }
      if (url.endsWith('/models') && init.method === 'POST') {
        modelPosts.push(JSON.parse(init.body));
        return respond(200, { models: [{ id: 'od-flash', upstream_id: 'open-design/flash' }], models_revision: 'list-rev-6' });
      }
      if (url.includes('/status')) return respond(200, statusBody({ models_revision: 'list-rev-4' }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();
  await api.handlers.saveReasoning();
  assert.equal(api.state.modelsRevision, 'list-rev-5', 'the reasoning response token is kept');

  api.handlers.addMapping();
  await api.handlers.saveModels();
  assert.equal(modelPosts.length, 1);
  assert.equal(modelPosts[0].expected_models_revision, 'list-rev-5');
});

test('a 409 retry waits for a freshly read token and then asks to confirm', async () => {
  const posts = [];
  let confirmations = 0;
  let allowOverwrite = false;
  let statusRevision = 'list-rev-1';
  const models = () => [{ id: 'od-flash', upstream_id: 'open-design/flash' }];
  const view = fakePage({
    confirmImpl: () => { confirmations += 1; return allowOverwrite; },
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        const body = JSON.parse(init.body);
        posts.push(body);
        if (body.expected_models_revision === 'list-rev-1') return respond(409, { error: 'models_revision_conflict' });
        return respond(200, { models: models(), models_revision: 'list-rev-9' });
      }
      if (url.includes('/status')) return respond(200, statusBody({ models: models(), models_revision: statusRevision }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.saveModels();
  assert.equal(posts.length, 1, 'the first save uses the status token');
  assert.match(view.document.getElementById('modelState').textContent, /提交内容已保留/);

  // Clicking save again without reading a new token must not spin on the same
  // stale revision or overwrite blindly.
  await api.handlers.saveModels();
  assert.equal(posts.length, 1, 'no retry before a new token is read');
  assert.equal(confirmations, 0);
  assert.match(view.document.getElementById('modelState').textContent, /请先刷新状态/);

  // Reading the new token arms the next explicit confirmation.
  statusRevision = 'list-rev-2';
  await api.render.loadStatus();
  await api.handlers.saveModels();
  assert.equal(confirmations, 1, 'the retry asks for confirmation after the new token');
  assert.equal(posts.length, 1, 'a declined confirmation sends nothing');

  allowOverwrite = true;
  await api.handlers.saveModels();
  assert.equal(posts.length, 2);
  assert.equal(posts[1].expected_models_revision, 'list-rev-2');
});

test('switching the management key clears drafts and blocks the old status answer', async () => {
  const pendingStatus = deferred();
  let statusCalls = 0;
  const statusFor = (id, revision) => statusBody({ models: [{ id, upstream_id: id }], models_revision: revision });
  const view = fakePage({
    fetchImpl: async url => {
      if (!url.includes('/status')) return respond(404, { error: 'not found' });
      statusCalls += 1;
      if (statusCalls === 1) return respond(200, statusFor('old-model', 'old-rev'));
      if (statusCalls === 2) return pendingStatus.promise;
      return respond(200, statusFor('new-model', 'new-rev'));
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  api.handlers.addMapping();
  assert.equal(api.state.modelsDraftActive, true);

  // An old-session status read is still pending when the key is switched.
  const stale = api.render.loadStatus();
  await new Promise(setImmediate);
  view.document.getElementById('managementKey').value = 'new-management-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.equal(api.state.modelsDraftActive, false, 'a key switch clears the old mapping draft');
  assert.equal(api.state.reasoningDrafts.size, 0, 'a key switch clears the old reasoning drafts');
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['new-model']);

  pendingStatus.resolve(respond(200, statusFor('old-model', 'old-rev')));
  await stale;
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['new-model'], 'the old status cannot repaint the new session');
  assert.equal(api.state.modelsRevision, 'new-rev');
  assert.equal(api.state.serverModels[0].id, 'new-model');
});

test('a late 401 from the old session cannot log out the newly verified key', async () => {
  const pendingStatus = deferred();
  let statusCalls = 0;
  const view = fakePage({
    fetchImpl: async url => {
      if (!url.includes('/status')) return respond(404, { error: 'not found' });
      statusCalls += 1;
      if (statusCalls === 1) return respond(200, statusBody({ models_revision: 'old-rev' }));
      if (statusCalls === 2) return pendingStatus.promise;
      return respond(200, statusBody({ models_revision: 'new-rev' }));
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const stale = api.render.loadStatus();
  await new Promise(setImmediate);

  view.document.getElementById('managementKey').value = 'brand-new-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.notEqual(api.state.verified, null);
  pendingStatus.resolve(respond(401, { error: 'old key expired' }));
  await stale;
  assert.notEqual(api.state.verified, null, 'the old 401 must not log out the new session');
  assert.equal(api.state.candidate, undefined,
    'a typed key is never stored as page auth, so no candidate can outlive its verification request');
  assert.equal(api.state.modelsRevision, 'new-rev');
});

test('a delayed refreshStatus from the old session cannot repopulate after logout', async () => {
  const pendingStatus = deferred();
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/credentials') && init.method === 'POST') return respond(200, { id: 'cred-9', label: '新账户', disabled: false });
      if (url.includes('/status')) return pendingStatus.promise;
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  view.document.getElementById('accountLabel').value = '新账户';
  view.document.getElementById('accountKey').value = key;
  const saving = api.handlers.addAccount({ preventDefault() {} });
  await new Promise(setImmediate);

  api.handlers.clearAuth();
  assert.equal(api.state.verified, null);
  pendingStatus.resolve(respond(200, statusBody({
    credentials: [{ id: 'cred-9', label: '新账户', disabled: false }],
    models: [{ id: 'stale-model', upstream_id: 'stale-model' }],
    models_revision: 'stale-rev',
  })));
  await saving;
  assert.deepEqual(plain(api.state.status.credentials), [], 'the old refresh cannot repopulate credentials after logout');
  assert.deepEqual(plain(api.state.serverModels), [], 'the old refresh cannot repopulate models after logout');
  assert.equal(api.state.modelsRevision, '', 'the old token is not adopted after logout');
  assert.equal(view.document.getElementById('accountsPanel').hidden, true);
});

test('an upstream change keeps the old reasoning draft and surfaces it as copyable text', async () => {
  let models = [{ id: 'od-a', upstream_id: 'vela/gpt-6-luna', reasoning_revision: 'rev-0' }];
  const view = fakePage({
    fetchImpl: async url => (url.includes('/status') ? respond(200, reasoningStatus(...models)) : respond(404, { error: 'not found' })),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.setReasoningEnabled(true);
  const draftKey = api.state.reasoningModelKey;
  assert.equal(api.state.reasoningDrafts.has(draftKey), true);
  assert.ok(draftKey.includes('vela/gpt-6-luna'));

  // The same alias now points at a different upstream: the draft's key disappears
  // from the live list but the rules must not vanish with it.
  models = [{ id: 'od-a', upstream_id: 'vela/gpt-6-saturn', reasoning_revision: 'rev-0' }];
  api.render.applyStatus(reasoningStatus(...models));
  assert.equal(api.state.reasoningDrafts.has(draftKey), true, 'the upstream-changed draft is kept');
  const text = view.document.getElementById('reasoningOrphans').textContent;
  assert.match(text, /草稿已保留/);
  assert.match(text, /od-a → vela\/gpt-6-luna/, 'the copyable text names the old mapping');
  assert.match(text, /→/, 'the rule table is included for copying');
});

// ---- 额度暂停 (plugin-owned quota pause) ------------------------------------

test('quota pause is shown separately from the operator disabled switch', () => {
  const core = fakePage().context.OpenDesignCore;
  const paused = core.normalizeStatus(statusBody({
    credentials: [{
      id: 'cred-1', label: '主账户', disabled: false,
      quota_paused: true, effective_disabled: true,
      quota_pause: {
        reason: 'quota_exhausted', source: 'upstream',
        resume_at: new Date(Date.now() + 3600_000).toISOString(),
        manual_disabled: false, model_id: 'open-design/flash',
      },
    }],
  })).credentials[0];
  // The operator's own switch stays off even while the account is paused.
  assert.equal(paused.disabled, false);
  assert.equal(paused.quota_paused, true);
  assert.equal(paused.effective_disabled, true);
  assert.match(core.quotaPauseText(paused.quota_pause), /额度暂停（约 .*后复查）/);
  // A pause with no usable deadline still renders as a pause.
  assert.equal(core.quotaPauseText({ resume_at: '' }), '额度暂停');
  assert.equal(core.quotaPauseText(null), '');
  assert.equal(core.normalizeQuotaPause(null), null);
});

test('a paused account offers 解除额度暂停 and never flips the disabled switch', async () => {
  const calls = [];
  const pause = {
    reason: 'quota_exhausted', source: 'upstream',
    resume_at: new Date(Date.now() + 3600_000).toISOString(),
    manual_disabled: false, model_id: 'open-design/flash',
  };
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init });
      if (url.endsWith('/quota-pause/resume')) return respond(200, { pauses: [] });
      if (url.endsWith('/status')) {
        return respond(200, statusBody({
          credentials: [{ id: 'cred-1', label: '主账户', disabled: false, quota_paused: true, effective_disabled: true, quota_pause: pause }],
        }));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const row = view.document.getElementById('accountList').children[0];
  assert.match(row.textContent, /额度耗尽 · 自动暂停/);
  assert.match(row.textContent, /约 .*后复查/);
  // The row exposes an explicit resume control, but nothing runs on render.
  const actionsRow = row.children[1];
  const actionTexts = [...actionsRow.children].map(node => node.textContent);
  assert.ok(actionTexts.includes('解除额度暂停'), `expected a resume control, got ${actionTexts.join(',')}`);
  assert.equal(calls.filter(call => call.url.endsWith('/quota-pause/resume')).length, 0,
    'the console must not resume without an operator click');

  await api.handlers.resumeQuotaPause(api.state.status.credentials[0]);
  const sent = calls.filter(call => call.url.endsWith('/quota-pause/resume'));
  assert.equal(sent.length, 1);
  assert.deepEqual(JSON.parse(sent[0].init.body), { credential_id: 'cred-1' });
  assert.equal(sent[0].init.method, 'POST');
  // A new block can arrive before refresh; the UI must reflect the latest state
  // rather than claiming that the account is no longer paused.
  assert.match(view.document.getElementById('accountState').textContent, /仍处于额度暂停/);
  assert.doesNotMatch(view.document.getElementById('accountState').textContent, /已解除插件额度暂停/);
  // The pause is cleared through the pause route only; `disabled` was never posted.
  assert.ok(!calls.some(call => call.url.endsWith('/credentials')), 'resuming a pause must not toggle the account');
});

test('paused accounts sort after active ones by their recorded resume time', () => {
  const at = hour => `2030-01-01T${String(hour).padStart(2, '0')}:00:00Z`;
  const active = id => ({ id, label: id, disabled: false });
  const paused = (id, resumeAt) => ({
    id, label: id, disabled: false, quota_paused: true,
    quota_pause: resumeAt ? { reason: 'quota_exhausted', resume_at: resumeAt } : {},
  });
  const view = fakePage();
  const api = view.context.OpenDesignConsole;
  api.render.applyStatus(statusBody({
    credentials: [
      active('active-a'),
      paused('paused-late', at(9)),
      active('active-b'),
      paused('paused-early', at(2)),
      paused('paused-unknown'),
      paused('paused-tie-a', at(5)),
      paused('paused-tie-b', at(5)),
    ],
  }));
  const displayed = accountRows(view).map(row => row.getAttribute('data-credential-id'));
  // Active accounts keep the operator's order; paused accounts follow, earliest
  // resume first, an unusable deadline last, and exact ties stay stable.
  assert.deepEqual(displayed, [
    'active-a', 'active-b', 'paused-early', 'paused-tie-a', 'paused-tie-b',
    'paused-late', 'paused-unknown',
  ]);
  // Display ordering is a view only: the status snapshot itself is never sorted.
  assert.deepEqual([...api.state.status.credentials].map(item => item.id), [
    'active-a', 'paused-late', 'active-b', 'paused-early', 'paused-unknown',
    'paused-tie-a', 'paused-tie-b',
  ]);
});

test('a historical upstream reset or retry hint is not presented as a recovery prediction', () => {
  const core = fakePage().context.OpenDesignCore;
  const now = Date.now();
  const hours = value => new Date(now + value * 3600_000).toISOString();
  const pause = core.normalizeQuotaPause({
    reason: 'quota_exhausted', resume_at: hours(1), estimate_at: hours(72),
  });
  assert.equal(pause.estimate_at, hours(72));
  // This is a hint from the old error, not proof that recovery must wait that long.
  assert.equal(core.quotaPauseCountdown(pause, now), '1 小时后复查 · 上游原提示 3 天后重置或重试');
  assert.equal(core.quotaPauseText(pause, now), '额度暂停（1 小时后复查，上游原提示 3 天后重置或重试）');
  // Without a hint the countdown keeps its previous wording.
  const plain = core.normalizeQuotaPause({ reason: 'quota_exhausted', resume_at: hours(2) });
  assert.equal(core.quotaPauseCountdown(plain, now), '约 2 小时后复查');
  assert.equal(core.quotaPauseCountdown({ resume_at: hours(2), estimate_at: 'not-a-time' }, now), '约 2 小时后复查');
  // A hint that is not later than the deadline adds nothing.
  assert.equal(core.quotaPauseCountdown({ resume_at: hours(2), estimate_at: hours(1) }, now), '约 2 小时后复查');
  // An unusable or absent hint stays absent in the normalized shape.
  assert.equal(core.normalizeQuotaPause({ resume_at: hours(1), estimate_at: 'nope' }).estimate_at, '');
  assert.equal(core.normalizeQuotaPause({ resume_at: hours(1) }).estimate_at, '');
});

test('credits convert to an approximate dollar value only with a configured rate', () => {
  const core = fakePage().context.OpenDesignCore;
  // 5000 credits = $1, and the number the operator records is the credits per $1.
  assert.equal(core.formatCreditsUSD('5000', '5000'), '≈ $1.00', 'the rate is credits per $1');
  assert.equal(core.formatCreditsUSD('47387', '5000'), '≈ $9.48');
  // The backend accepts positive decimal rates; a legal fractional rate keeps working.
  assert.equal(core.formatCreditsUSD('10001', '5000.5'), '≈ $2.00');
  assert.equal(core.formatCreditsUSD('1', '8'), '≈ $0.13', 'half a cent rounds up');
  assert.equal(core.formatCreditsUSD('1', '200'), '≈ $0.01', 'half of the first cent rounds up');
  assert.equal(core.formatCreditsUSD('0', '5000.5'), '≈ $0.00', 'known zero remains a valid amount');
  assert.equal(core.formatCreditsUSD('47387', ''), '', 'no rate means no dollar value');
  for (const rate of ['0', '0.0', '-5', '1e6', 'abc', '5000cr', '   ', '9'.repeat(33), 5000]) {
    assert.equal(core.formatCreditsUSD('47387', rate), '', `invalid rate ${JSON.stringify(rate)} must not divide`);
  }
  assert.equal(core.formatCreditsUSD('', '5000'), '');
  // BigInt math keeps large credit counts exact instead of rounding to a float.
  assert.equal(core.formatCreditsUSD('123456789012345678901234567890', '5000'), '≈ $24691357802469135780246913.58');
  // Fractional cents are summed exactly and rounded once, not per account.
  const parts = Array.from({ length: 3 }, () => ({ credits: '1', creditsPerUSD: '8' }));
  assert.deepEqual({ ...core.sumCreditsUSD(parts) }, { usd: '0.38', converted: 3, accounts: 3 });
  const subCent = Array.from({ length: 3 }, () => ({ credits: '1', creditsPerUSD: '300' }));
  assert.deepEqual({ ...core.sumCreditsUSD(subCent) }, { usd: '0.01', converted: 3, accounts: 3 });
  assert.deepEqual({ ...core.sumCreditsUSD([
    { credits: '5000', creditsPerUSD: '5000' },
    { credits: '5000' },
  ]) }, { usd: '1.00', converted: 1, accounts: 2 }, 'missing rate keeps credits but adds no dollars');
  assert.deepEqual({ ...core.sumCreditsUSD([{ credits: '5000' }]) },
    { usd: '', converted: 0, accounts: 1 }, 'no configured rate must not invent a zero-dollar total');
});

test('normalizeStatus keeps a legal fractional rate and drops invalid ones', () => {
  const core = fakePage().context.OpenDesignCore;
  const rateOf = credits_per_usd => core.normalizeStatus(statusBody({
    credentials: [{ id: 'cred-1', label: '主账户', disabled: false, credits_per_usd }],
  })).credentials[0].credits_per_usd;
  assert.equal(rateOf(' 5000.5 '), '5000.5');
  assert.equal(rateOf('5000'), '5000');
  assert.equal(rateOf(''), '');
  for (const rate of ['0', '0.0', '-5', '1e6', 'abc', '9'.repeat(33), 5000]) {
    assert.equal(rateOf(rate), '', `invalid rate ${JSON.stringify(rate)} stays unknown`);
  }
});

test('the account header and rows show the equivalent dollar value of remaining credits', () => {
  const credentials = [
    { id: 'active-a', label: 'A', disabled: false, quota_paused: false, billing_configured: true, workspace_id: 'ws-a', credits_per_usd: '5000' },
    { id: 'active-b', label: 'B', disabled: false, quota_paused: false, billing_configured: true, workspace_id: 'ws-b' },
  ];
  const view = fakePage();
  const api = view.context.OpenDesignConsole;
  const Core = view.context.OpenDesignCore;
  const quota = (id, windows) => Core.normalizeQuota(quotaContract({
    credential_id: id,
    funding: { state: 'available', data: { codingPlan: { windows } } },
  }));
  api.render.applyStatus(statusBody({ credentials }));
  api.state.quotas['active-a'] = quota('active-a', [{ durationSeconds: 18000, remainingCredits: '50000' }]);
  api.render.renderAccounts();
  const total = view.document.getElementById('accountQuotaTotal');
  const usage = [...total.children].find(child => child.className === 'usage');
  // Only the account that recorded a rate produces a dollar equivalent.
  assert.deepEqual([...usage.children[0].children].map(child => child.textContent), ['窗口 5 小时', '50,000 · ≈ $10.00']);
  assert.match(usage.children[1].textContent, /约合 \$10\.00/);
  // The row summary shows the same equivalent next to the plain credits.
  const row = accountRows(view).find(node => node.getAttribute('data-credential-id') === 'active-a');
  const summary = flattenNodes(row.children[2]).filter(node => node.className === 'usage');
  assert.match(summary.map(node => node.textContent).join(' | '), /剩余 50,000 \(≈ \$10\.00\)/);
  // A sibling without a rate keeps showing credits alone.
  api.state.quotas['active-b'] = quota('active-b', [{ durationSeconds: 18000, remainingCredits: '2000' }]);
  api.render.renderAccounts();
  const rows = accountRows(view);
  const plainRow = rows.find(node => node.getAttribute('data-credential-id') === 'active-b');
  const plainSummary = flattenNodes(plainRow.children[2]).filter(node => node.className === 'usage')
    .map(node => node.textContent).join(' | ');
  assert.match(plainSummary, /剩余 2,000/);
  assert.doesNotMatch(plainSummary, /≈ \$/);
});

test('the account header converts each account by its own rate and only sums configured ones', () => {
  const credentials = [
    { ...activeCredential('a'), credits_per_usd: '5000' },
    { ...activeCredential('b'), credits_per_usd: '10000' },
  ];
  const view = fakePage();
  const api = view.context.OpenDesignConsole;
  const Core = view.context.OpenDesignCore;
  const remaining = id => Core.normalizeQuota(quotaContract({
    credential_id: id,
    wallet: { state: 'not_configured', data: null },
    funding: { state: 'available', data: { codingPlan: { windows: [{ durationSeconds: 18000, remainingCredits: '5000' }] } } },
  }));
  api.render.applyStatus(statusBody({ credentials }));
  api.state.quotas.a = remaining('a');
  api.state.quotas.b = remaining('b');
  api.render.renderAccounts();
  const total = view.document.getElementById('accountQuotaTotal');
  const usage = () => [...total.children].find(child => child.className === 'usage');
  const money = () => [...usage().children[0].children].map(child => child.textContent);
  const sub = () => usage().children[1].textContent;
  assert.deepEqual(money(), ['窗口 5 小时', '10,000 · ≈ $1.50'], '5000/5000 + 5000/10000');
  const firstOrder = total.textContent;

  // Swapping the accounts must not change either the credits or the dollars.
  api.render.applyStatus(statusBody({ credentials: [credentials[1], credentials[0]] }));
  api.render.renderAccounts();
  assert.equal(total.textContent, firstOrder);
  assert.deepEqual(money(), ['窗口 5 小时', '10,000 · ≈ $1.50']);

  // An account without a rate still contributes credits, never dollars.
  api.render.applyStatus(statusBody({ credentials: [...credentials, activeCredential('c')] }));
  api.state.quotas.c = remaining('c');
  api.render.renderAccounts();
  assert.deepEqual(money(), ['窗口 5 小时', '15,000 · ≈ $1.50']);
  assert.match(sub(), /已换算 2\/3 个账户/);
  assert.match(sub(), /美元仅含已配置部分/);

  // Once every contributing account has a rate the normal total needs no caveat.
  api.render.applyStatus(statusBody({ credentials: [...credentials, { ...activeCredential('c'), credits_per_usd: '5000' }] }));
  api.render.renderAccounts();
  assert.deepEqual(money(), ['窗口 5 小时', '15,000 · ≈ $2.50']);
  assert.doesNotMatch(sub(), /已换算/);
  assert.doesNotMatch(sub(), /美元仅含已配置部分/);
});

test('the account header sums remaining quota from active accounts only', () => {
  const credentials = [
    { id: 'active-a', label: 'A', disabled: false, quota_paused: false },
    { id: 'active-b', label: 'B', disabled: false, quota_paused: false },
    { id: 'paused-c', label: 'C', disabled: false, quota_paused: true, quota_pause: { resume_at: '2030-01-01T00:00:00Z' } },
    { id: 'off-d', label: 'D', disabled: true },
  ];
  const view = fakePage();
  const api = view.context.OpenDesignConsole;
  const Core = view.context.OpenDesignCore;
  const quota = (id, walletUSD, windows) => Core.normalizeQuota(quotaContract({
    credential_id: id,
    wallet: { state: 'available', data: { balanceUsd: walletUSD } },
    funding: { state: 'available', data: { codingPlan: { windows } } },
  }));
  const window = (durationSeconds, remainingCredits) => ({ durationSeconds, remainingCredits });
  const total = view.document.getElementById('accountQuotaTotal');
  // Every account is either paused or manually disabled, so nothing is summed.
  api.render.applyStatus(statusBody({ credentials: [credentials[2], credentials[3]] }));
  assert.match(total.textContent, /暂无活动中账户/);
  api.render.applyStatus(statusBody({ credentials }));
  // Active accounts exist but no snapshot has arrived yet: report them as
  // pending instead of showing a zero total.
  assert.match(total.children[0].textContent, /正在按账户顺序查询额度…（活动 2 个账户）/);

  api.state.quotas['active-a'] = quota('active-a', '12.34', [
    window(18000, '800'), window(604800, '1234'),
  ]);
  // Paused and disabled snapshots exist but must never inflate the total.
  api.state.quotas['paused-c'] = quota('paused-c', '999.00', [
    window(18000, '5000'), window(604800, '5000'),
  ]);
  api.state.quotas['off-d'] = quota('off-d', '777.00', [
    window(18000, '7000'), window(604800, '7000'),
  ]);
  api.render.renderAccounts();
  const usageRows = node => [...node.children].filter(child => child.className === 'usage');
  assert.match(total.children[0].textContent, /当前可用总额度（活动中 2 个账户）/);
  assert.deepEqual(usageRows(total).map(node => ({
    head: [...node.children[0].children].map(child => child.textContent),
    sub: node.children[1].textContent,
  })), [
    { head: ['窗口 5 小时', '800'], sub: '1 个账户剩余 credits' },
    { head: ['窗口 7 天', '1,234'], sub: '1 个账户剩余 credits' },
    { head: ['钱包余额', '$12.34'], sub: '1 个账户' },
  ]);
  assert.equal(total.children[total.children.length - 1].textContent, '已查询 1 / 2 个账户 · 1 个待查询');

  api.state.quotas['active-b'] = quota('active-b', '0.66', [
    window(18000, '200'), window(604800, '66'),
  ]);
  api.render.renderAccounts();
  assert.match(total.children[0].textContent, /当前可用总额度（活动中 2 个账户）/);
  assert.deepEqual(usageRows(total).map(node => ({
    head: [...node.children[0].children].map(child => child.textContent),
    sub: node.children[1].textContent,
  })), [
    { head: ['窗口 5 小时', '1,000'], sub: '2 个账户剩余 credits' },
    { head: ['窗口 7 天', '1,300'], sub: '2 个账户剩余 credits' },
    { head: ['钱包余额', '$13.00'], sub: '2 个账户' },
  ]);
  assert.equal(total.children[total.children.length - 1].textContent, '已查询 2 / 2 个账户');
});

test('old-session quota actions cannot trigger a status refresh in a new session', async () => {
  for (const action of ['resumeQuotaPause', 'recheckQuotaPause']) {
    const pendingAction = deferred();
    const calls = [];
    const view = fakePage({
      confirmImpl: () => true,
      fetchImpl: async (url) => {
        calls.push(url);
        if (url.includes('/quota-pause/')) return pendingAction.promise;
        if (url.includes('/status')) return respond(200, statusBody({ models_revision: 'new-session-rev' }));
        return respond(404, { error: 'not found' });
      },
    });
    const api = view.context.OpenDesignConsole;
    api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
    await api.render.loadStatus();
    const pending = api.handlers[action]({ id: 'cred-1', label: '主账户' });
    await new Promise(setImmediate);
    api.render.showAuth('请重新认证');
    view.document.getElementById('managementKey').value = 'new-session-key';
    await api.handlers.submitAuth({ preventDefault() {} });
    const before = calls.filter(url => url.includes('/status')).length;
    pendingAction.resolve(respond(200, { pauses: [] }));
    await pending;
    assert.equal(calls.filter(url => url.includes('/status')).length, before, `${action} unexpectedly refreshed the new session`);
    assert.equal(api.state.modelsRevision, 'new-session-rev');
    assert.ok(!view.document.getElementById('accountState').textContent.includes('已解除'), 'old success must not overwrite new session UI');
  }
});

test('recheck posts the credential id and reports an unproven preflight', async () => {
  const calls = [];
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init });
      if (url.endsWith('/quota-pause/recheck')) return respond(409, { error: '本次预检未能证明额度已恢复' });
      if (url.endsWith('/status')) return respond(200, statusBody());
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.recheckQuotaPause({ id: 'cred-1', label: '主账户' });
  const sent = calls.filter(call => call.url.endsWith('/quota-pause/recheck'));
  assert.equal(sent.length, 1);
  assert.deepEqual(JSON.parse(sent[0].init.body), { credential_id: 'cred-1' });
  assert.match(view.document.getElementById('accountState').textContent, /重查未通过/);
});

test('额度自动暂停在旧配置或旧后端缺失该选项时默认开启', async () => {
  const core = fakePage().context.OpenDesignCore;
  const defaults = core.normalizeConfig({ logs: {}, upstream: { origin: 'https://x', fixed: true }, proxy: {}, routing: {} });
  assert.equal(defaults.pause.enabled, true);
  assert.equal(defaults.pause.max_seconds, 86400);
  // Out-of-range values fall back instead of being sent back to the plugin.
  const clamped = core.normalizeConfig({ pause: { enabled: false, max_seconds: 1 } });
  assert.equal(clamped.pause.enabled, false);
  assert.equal(clamped.pause.max_seconds, 86400);

  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init });
      if (url.endsWith('/config') && init.method === 'POST') {
        return respond(200, { logs: {}, pause: { enabled: false, max_seconds: 3600, owner: 'plugin' }, upstream: { fixed: true }, proxy: {}, routing: {} });
      }
      if (url.endsWith('/config')) {
        return respond(200, { logs: {}, pause: { enabled: true, max_seconds: 86400, owner: 'plugin' }, upstream: { fixed: true }, proxy: {}, routing: {} });
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.handlers.loadConfig();
  assert.equal(view.document.getElementById('configPauseEnabled').checked, true);
  assert.equal(view.document.getElementById('configPauseMaxSeconds').value, '86400');
  view.document.getElementById('configPauseMaxSeconds').value = '5';
  await api.handlers.savePauseConfig({ preventDefault() {} });
  assert.ok(!calls.some(call => call.init.method === 'POST'), 'an out-of-range value must not be submitted');
  assert.match(view.document.getElementById('configState').textContent, /60 到 604800/);
  view.document.getElementById('configPauseMaxSeconds').value = '3600';
  view.document.getElementById('configPauseEnabled').checked = false;
  await api.handlers.savePauseConfig({ preventDefault() {} });
  const sent = calls.filter(call => call.init.method === 'POST');
  assert.equal(sent.length, 1);
  assert.deepEqual(JSON.parse(sent[0].init.body), { pause: { enabled: false, max_seconds: 3600 } });
  assert.equal(view.document.getElementById('configPauseEnabled').checked, false);
});

// ---- 额度暂停：同步待重试、等待复查与本地倒计时 --------------------------------

const pauseStatus = (overrides = {}) => statusBody({
  credentials: [{
    id: 'cred-1', label: '主账户', disabled: false,
    billing_configured: true, workspace_id: 'ws-1',
    quota_paused: true, effective_disabled: true,
    quota_pause: {
      reason: 'quota_exhausted', source: 'upstream',
      resume_at: new Date(Date.now() + 3600_000).toISOString(),
      observed_at: new Date().toISOString(),
      manual_disabled: false, model_id: 'open-design/flash',
      ...overrides,
    },
  }],
});

test('仅完整模型集合与有效额度配置显示重查按钮，多个模型也可重查', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init });
      if (url.endsWith('/quota-pause/recheck')) return respond(200, { pauses: [] });
      if (url.endsWith('/status')) return respond(200, pauseStatus({ multiple_models: true, models_ambiguous: false }));
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const row = () => view.document.getElementById('accountList').children[0];
  const recheckButton = () => [...row().children[1].children].find(node => node.textContent === '重查额度');
  assert.ok(recheckButton(), '完整的多模型集合应允许主动复查');
  assert.match(row().textContent, /须逐一复查并确认属于同一 Workspace 成员/);
  await recheckButton().emit('click');
  const sent = calls.filter(call => call.url.endsWith('/quota-pause/recheck'));
  assert.equal(sent.length, 1);
  assert.deepEqual(JSON.parse(sent[0].init.body), { credential_id: 'cred-1' });
  // A block may be recreated before the status refresh completes. Do not say
  // that the account has been resumed while the latest list still says paused.
  assert.match(view.document.getElementById('accountState').textContent, /仍处于额度暂停/);
  assert.doesNotMatch(view.document.getElementById('accountState').textContent, /已解除插件额度暂停/);

  // A legacy multi-model result without the completeness flag is ambiguous and
  // must not invite a preflight that cannot prove recovery.
  api.render.applyStatus(pauseStatus({ multiple_models: true }));
  assert.equal(recheckButton(), undefined);
  assert.ok(!row().textContent.includes('可点「重查额度」'));
  api.render.applyStatus(pauseStatus({ multiple_models: true, models_ambiguous: true }));
  assert.equal(recheckButton(), undefined);
  assert.ok(!row().textContent.includes('可点「重查额度」'));
  api.render.applyStatus(pauseStatus({ multiple_models: true, models_ambiguous: null }));
  assert.equal(recheckButton(), undefined, '非布尔完整性标记不得被当成已确认');

  const unconfigured = pauseStatus();
  unconfigured.credentials[0].billing_configured = false;
  api.render.applyStatus(unconfigured);
  assert.equal(recheckButton(), undefined);
  api.render.applyStatus(pauseStatus({ model_id: '', multiple_models: false }));
  assert.equal(recheckButton(), undefined);
  assert.ok(!row().textContent.includes('可点「重查额度」'));
  api.render.applyStatus(pauseStatus());
  assert.ok(recheckButton(), '已配置的单模型暂停仍可重查');
});

test('同步失败在账户行显示“同步待重试”，并说明不会泄漏细节', async () => {
  const view = fakePage({
    fetchImpl: async (url) => respond(url.endsWith('/status')
      ? 200
      : 404, url.endsWith('/status') ? pauseStatus({ error: '额度暂停状态未能同步到凭据文件；插件会重试。' }) : { error: 'not found' }),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const row = view.document.getElementById('accountList').children[0];
  assert.match(row.textContent, /同步待重试/, '/status 的暂停详情必须能驱动同步失败徽标');
  assert.match(row.textContent, /按固定间隔重试/);
  // A pause without a sync error must not show the badge.
  api.render.applyStatus(pauseStatus());
  assert.ok(!view.document.getElementById('accountList').children[0].textContent.includes('同步待重试'));
});

test('暂停期间的说明明确人工启停边界、复查要求与无授权限制', async () => {
  const view = fakePage({
    fetchImpl: async (url) => respond(url.endsWith('/status') ? 200 : 404,
      url.endsWith('/status') ? pauseStatus() : { error: 'not found' }),
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const note = view.document.getElementById('accountList').children[0].textContent;
  assert.match(note, /请在本页启用\/停用/, '暂停期间必须要求通过插件控制台人工启停');
  assert.match(note, /CPA 原生入口和并发文件改写无法可靠归因/, '必须提示宿主入口无法可靠归因');
  assert.match(note, /到点只表示应复查，不代表额度已恢复/);
  assert.match(note, /开启自动暂停时，插件会逐账号串行复查/);
  assert.match(note, /只有新鲜官方结果确认可用才解除，失败继续暂停/);
  assert.match(note, /controlKey 和推理 key 属于同一账号；插件无法自动验证归属/);

  // No control key: the note must say recovery cannot be checked automatically.
  api.render.applyStatus(statusBody({
    credentials: [{
      id: 'cred-1', label: '主账户', disabled: false, billing_configured: false,
      quota_paused: true, effective_disabled: true,
      quota_pause: {
        reason: 'quota_exhausted', source: 'upstream',
        resume_at: new Date(Date.now() + 3600_000).toISOString(),
        manual_disabled: false, model_id: 'open-design/flash',
      },
    }],
  }));
  const bare = view.document.getElementById('accountList').children[0].textContent;
  assert.match(bare, /未配置官方 controlKey 或 Workspace ID，无法复查额度/);
  assert.match(bare, /仍须人工处理/);
  assert.ok(!bare.includes('可点「重查额度」'), 'an unconfigured account has no recheck path at all');

  // A control key alone is insufficient without the workspace identifier.
  const missingWorkspace = pauseStatus();
  missingWorkspace.credentials[0].workspace_id = '';
  api.render.applyStatus(missingWorkspace);
  const missing = view.document.getElementById('accountList').children[0].textContent;
  assert.match(missing, /未配置官方 controlKey 或 Workspace ID，无法复查额度/);
  assert.ok(!missing.includes('可点「重查额度」'), 'a missing workspace cannot be rechecked');

  api.render.applyStatus(pauseStatus({ recheck_status: 'unauthorized' }));
  assert.match(view.document.getElementById('accountList').children[0].textContent, /官方拒绝额度授权，请检查 controlKey/);
  api.render.applyStatus(pauseStatus({ multiple_models: true, models_ambiguous: false }));
  assert.match(view.document.getElementById('accountList').children[0].textContent, /须逐一复查并确认属于同一 Workspace 成员/);
  api.render.applyStatus(pauseStatus({ multiple_models: true }));
  assert.match(view.document.getElementById('accountList').children[0].textContent, /耗尽模型集合不完整，无法自动确认恢复/);
});

test('人工停用与额度暂停是两个独立徽标，解除暂停不会发送 disabled', async () => {
  const calls = [];
  const view = fakePage({
    confirmImpl: () => true,
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init });
      if (url.endsWith('/quota-pause/resume')) return respond(200, { pauses: [] });
      if (url.endsWith('/status')) {
        return respond(200, statusBody({
          credentials: [{
            id: 'cred-1', label: '主账户', disabled: true, billing_configured: true,
            quota_paused: true, effective_disabled: true,
            quota_pause: {
              reason: 'quota_exhausted', source: 'upstream',
              resume_at: new Date(Date.now() + 3600_000).toISOString(),
              manual_disabled: true, model_id: 'open-design/flash',
            },
          }],
        }));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  const row = view.document.getElementById('accountList').children[0];
  assert.match(row.textContent, /已停用/, '人工停用状态单独显示');
  assert.match(row.textContent, /额度耗尽 · 自动暂停/, '额度暂停状态单独显示');
  assert.match(row.textContent, /已停用/);
  const actionTexts = [...row.children[1].children].map(node => node.textContent);
  assert.ok(actionTexts.includes('启用'), 'the operator switch reflects the manual disable');

  await api.handlers.resumeQuotaPause(api.state.status.credentials[0]);
  assert.equal(calls.filter(call => call.url.endsWith('/quota-pause/resume')).length, 1);
  assert.ok(!calls.some(call => call.url.endsWith('/credentials')), '解除暂停不得改写人工停用');
  assert.match(view.document.getElementById('accountState').textContent, /不会查询或证明额度已恢复/);
});

test('暂停倒计时在页面内本地变化，且不发出任何网络请求', async () => {
  const calls = [];
  const resumeAt = new Date(Date.now() + 65_000).toISOString();
  const view = fakePage({
    fetchImpl: async (url) => {
      calls.push(url);
      if (url.endsWith('/status')) {
        return respond(200, statusBody({
          credentials: [{
            id: 'cred-1', label: '主账户', disabled: false, billing_configured: true,
            quota_paused: true, effective_disabled: true,
            quota_pause: {
              reason: 'quota_exhausted', source: 'upstream', resume_at: resumeAt,
              manual_disabled: false, model_id: 'open-design/flash',
            },
          }],
        }));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  // Only the badge row is inspected: the pause footer legitimately contains the
  // words "不代表额度已恢复", so the badge itself must stay free of any
  // recovered/expiring wording.
  const readBadge = () => view.document.getElementById('accountList').children[0].children[0].textContent;
  assert.match(readBadge(), /额度耗尽 · 自动暂停/);
  assert.match(readBadge(), /约 .*后复查/);
  const requestsAfterLoad = calls.length;

  // The scheduled tick is local: it re-renders through the same renderer with no
  // fetch, no quota route and no status reload.
  const before = readBadge();
  api.render.renderAccounts();
  await new Promise(resolve => setTimeout(resolve, 30));
  assert.equal(calls.length, requestsAfterLoad, '本地倒计时不得发出网络请求');
  assert.equal(readBadge(), before);

  // Past the deadline the badge reads as waiting for a recheck, never as
  // recovered, and applying the same status again still issues no request.
  api.state.status.credentials[0].quota_pause.resume_at = new Date(Date.now() - 1_000).toISOString();
  api.render.renderAccounts();
  const overdue = readBadge();
  assert.match(overdue, /等待复查/, '到期必须是等待复查');
  assert.ok(!/即将恢复|已恢复/.test(overdue), '到期不能显示为已恢复');
  await new Promise(resolve => setTimeout(resolve, 30));
  assert.equal(calls.length, requestsAfterLoad, '到期渲染同样不得触发网络');
});

test('后端标记 awaiting_recheck 时即使本地时钟未到期也显示等待复查', () => {
  const core = fakePage().context.OpenDesignCore;
  const pause = core.normalizeQuotaPause({
    resume_at: new Date(Date.now() + 3600_000).toISOString(),
    awaiting_recheck: true,
  });
  assert.equal(pause.awaiting_recheck, true);
  assert.equal(core.quotaPauseText(pause), '额度暂停（等待复查）');
  // The flag wins even when the recorded deadline is missing or unparsable.
  assert.equal(core.quotaPauseText({ resume_at: '', awaiting_recheck: true }), '额度暂停（等待复查）');
  assert.equal(core.quotaPauseText({ resume_at: '' }), '额度暂停');
  assert.equal(core.normalizeQuotaPause({ awaiting_recheck: 'yes' }).awaiting_recheck, false);
});

// --- model row original identity (whole-list CAS) --------------------------
//
// The backend resolves original_id/original_upstream_id against the snapshot
// named by expected_models_revision. Sending them is what lets an alias-only
// rename keep its thinking-strength policy; a row the console created itself
// must never claim to be an existing server row.

const pinnedModels = () => [
  {
    id: 'od-flash',
    upstream_id: 'open-design/flash',
    reasoning_revision: 'rev-flash',
    reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] },
  },
  {
    id: 'od-pro',
    upstream_id: 'open-design/pro',
    reasoning_revision: 'rev-pro',
    reasoning_mapping: { enabled: true, rules: [{ from: 'low', to: 'low' }] },
  },
];

// Mirrors 883929e's whole-table semantics for the parts the console depends on:
// an explicit original identity keeps the stored policy only while the upstream
// is unchanged, and a new row simply has no policy.
function applyImportLikeBackend(models, body) {
  return (body.models || []).map(row => {
    const original = (row.original_id && row.original_upstream_id)
      ? models.find(item => item.id === row.original_id && item.upstream_id === row.original_upstream_id)
      : models.find(item => item.id === row.id && item.upstream_id === row.upstream_id);
    const next = { id: row.id, upstream_id: row.upstream_id };
    if (original && original.upstream_id === row.upstream_id) {
      if (original.reasoning_mapping) next.reasoning_mapping = original.reasoning_mapping;
      // An unchanged row with no policy keeps its revision too; otherwise a
      // different tab's unrelated table edit spuriously breaks reasoning CAS.
      next.reasoning_revision = original.reasoning_revision || `rev-${row.id}`;
    } else {
      next.reasoning_revision = `rev-${row.id}`;
    }
    return next;
  });
}

function modelSavePage(options = {}) {
  const posts = [];
  const confirmations = [];
  let models = options.models || pinnedModels();
  let revision = options.revision === undefined ? 'list-rev-1' : options.revision;
  const view = fakePage({
    confirmImpl: message => {
      confirmations.push(message);
      return options.confirmImpl ? options.confirmImpl(message, confirmations.length) : true;
    },
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/models') && init.method === 'POST') {
        const body = JSON.parse(init.body);
        posts.push(body);
        const result = options.onSave ? options.onSave(body, posts.length) : null;
        if (result) {
          if (result.models) models = result.models;
          if (result.revision !== undefined) revision = result.revision;
          return respond(result.status || 200, result.body || { models, models_revision: revision });
        }
        models = applyImportLikeBackend(models, body);
        return respond(200, { models, models_revision: revision });
      }
      if (url.includes('/status')) {
        return respond(200, statusBody({ models, models_revision: revision }));
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  return {
    view, api, posts, confirmations,
    setModels: next => { models = next; },
    setRevision: next => { revision = next; },
  };
}

test('a persisted 502 keeps blank rows unsaved and requires explicit per-row server adoption after refresh', async () => {
  let acceptRow = false;
  const page = modelSavePage({
    confirmImpl: text => text.includes('仅此行') ? acceptRow : true,
    onSave: (body, count) => count === 1 ? {
      status: 502,
      models: applyImportLikeBackend(pinnedModels(), body),
      revision: 'list-rev-2',
      body: { error: { code: 'host_contract_error', source: 'local', message: 'model mapping saved but the host model refresh failed: host rejected the refresh' } },
    } : null,
  });
  await page.api.render.loadStatus();
  const alias = page.view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'renamed';
  await alias.emit('input');
  page.api.handlers.addMapping(); // buildModelsPayload intentionally omits this blank row
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1);
  assert.equal(page.posts[0].models.length, 2);
  assert.deepEqual(plain(page.api.state.savedMappings.map(row => row.id)), ['renamed', 'od-pro'],
    'the unsent blank row must not appear in the saved baseline');
  assert.equal(page.api.state.mappings.length, 3, 'the unsent blank remains an editable draft');
  assert.equal(page.api.state.mappings[0].original_id, 'od-flash', 'a 502 has no echoed snapshot and cannot re-pin the row');
  assert.equal(page.api.mappingsDirty(), true);
  assert.match(page.view.document.getElementById('modelState').textContent, /刷新状态/);
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1, 'retry requires an actual status read');

  // A competing page changes the just-saved policy and adds another row before
  // this page reads it. Adopting Y must be explicit, never inferred from 502.
  const replacementPolicy = { enabled: true, rules: [{ from: 'low', to: 'max' }] };
  page.setModels([
    { id: 'renamed', upstream_id: 'open-design/flash', reasoning_mapping: replacementPolicy },
    pinnedModels()[1],
    { id: 'other-tab', upstream_id: 'up/other' },
  ]);
  page.setRevision('list-rev-3');
  await page.api.render.loadStatus();
  assert.equal(page.api.state.mappings[0].original_id, 'od-flash');
  assert.equal(page.api.state.modelsDraftRevision, 'list-rev-1');
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1, 'the old pin is not stripped or automatically rebound');
  assert.match(page.view.document.getElementById('modelState').textContent, /原行.*不存在/);
  assert.match(page.view.document.getElementById('modelState').textContent, /此行|逐行/);
  const row = page.view.document.getElementById('mappingList').children[0];
  const adopt = Array.from(row.children).find(node => node.getAttribute('data-action') === 'adopt-server-mapping');
  assert.ok(adopt, 'a real row control offers an explicit, testable resolution');
  await adopt.emit('click');
  assert.equal(page.api.state.mappings[0].original_id, 'od-flash', 'declining retains the original pin');
  assert.match(page.confirmations.at(-1), /仅此行/);
  assert.match(page.confirmations.at(-1), /思考强度/);
  acceptRow = true;
  await adopt.emit('click');
  assert.equal(page.api.state.mappings[0].original_id, 'renamed');
  assert.equal(page.api.state.mappings[0].original_upstream_id, 'open-design/flash');
  assert.equal(page.api.state.mappings.length, 3, 'other unsent rows are not discarded');
  assert.equal(page.api.state.modelsDraftRevision, 'list-rev-1', 'row adoption does not silently rebase the whole draft');
  assert.match(page.view.document.getElementById('modelState').textContent, /仅此行/);
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 2);
  assert.equal(page.posts[1].expected_models_revision, 'list-rev-3');
  assert.equal(page.posts[1].models[0].original_id, 'renamed');
  assert.equal(page.posts[1].models.length, 2, 'the blank row is still not sent');
  assert.ok(page.confirmations.some(text => /覆盖服务器/.test(text)), 'the whole-table overwrite still needs confirmation');
  assert.deepEqual(plain(page.api.state.serverModels[0].reasoning_mapping), replacementPolicy);
});

test('a 502 on a legacy backend also blocks the unresolved old pin until an explicit row decision', async () => {
  const page = modelSavePage({
    revision: '',
    onSave: (body, count) => count === 1 ? {
      status: 502,
      models: applyImportLikeBackend(pinnedModels(), body),
      revision: '',
      body: { error: 'model mapping saved but the host model refresh failed: host rejected the refresh' },
    } : null,
  });
  await page.api.render.loadStatus();
  const alias = page.view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'renamed';
  await alias.emit('input');
  await page.api.handlers.saveModels();
  await page.api.render.loadStatus();
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1, 'legacy alias matching must not silently adopt the saved row');
  assert.equal(page.api.state.mappings[0].original_id, 'od-flash');
  const row = page.view.document.getElementById('mappingList').children[0];
  const adopt = Array.from(row.children).find(node => node.getAttribute('data-action') === 'adopt-server-mapping');
  assert.ok(adopt);
  await adopt.emit('click');
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 2);
  assert.ok(page.confirmations.some(text => /不支持.*版本|没有.*版本/.test(text)), 'legacy overwrite limitations are explicit');
});

test('an alias-only rename sends the original identity so the policy survives', async () => {
  const { view, api, posts, confirmations } = modelSavePage();
  await api.render.loadStatus();
  assert.equal(api.state.mappings[0].original_id, 'od-flash');
  assert.equal(api.state.mappings[0].original_upstream_id, 'open-design/flash');

  const alias = view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-flash-fast';
  alias.emit('input');

  await api.handlers.saveModels();
  assert.equal(confirmations.length, 0, 'a rename drops nothing, so nothing is asked');
  assert.deepEqual(posts[0], {
    models: [
      { id: 'od-flash-fast', upstream_id: 'open-design/flash', original_id: 'od-flash', original_upstream_id: 'open-design/flash' },
      { id: 'od-pro', upstream_id: 'open-design/pro', original_id: 'od-pro', original_upstream_id: 'open-design/pro' },
    ],
    expected_models_revision: 'list-rev-1',
  });
  // Only the success message may claim the policy was kept, and only because the
  // echoed list actually still carries it.
  assert.match(view.document.getElementById('modelState').textContent, /思考强度映射已按原模型保留/);
  assert.equal(api.state.mappings[0].original_id, 'od-flash-fast', 'the saved row is pinned to the new identity');
  assert.equal(api.state.serverModels[0].reasoning_revision, 'rev-flash');
});

test('the console never claims a policy was kept when the echo does not carry one', async () => {
  const { view, api } = modelSavePage({
    // The backend cleared the policy: the success note must not pretend it stayed.
    onSave: body => ({
      models: body.models.map(row => ({ id: row.id, upstream_id: row.upstream_id, reasoning_revision: 'rev-new' })),
    }),
  });
  await api.render.loadStatus();
  const alias = view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-flash-fast';
  alias.emit('input');
  await api.handlers.saveModels();
  const text = view.document.getElementById('modelState').textContent;
  assert.match(text, /已保存 2 个模型映射/);
  assert.ok(!text.includes('思考强度映射已按原模型保留'), 'no unsupported claim about the dropped policy');
});

test('changing the upstream asks first and never carries the old policy silently', async () => {
  let warnings = 0;
  const page = modelSavePage({
    confirmImpl: message => {
      if (!message.includes('不会沿用原来的思考强度映射')) return true;
      warnings += 1;
      // Decline the warning once, then accept it.
      return warnings > 1;
    },
  });
  await page.api.render.loadStatus();
  const upstream = page.view.document.getElementById('mappingList').children[0].children[2].children[1];
  upstream.value = 'open-design/flash-2';
  upstream.emit('input');

  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 0, 'a declined warning sends nothing');
  assert.equal(page.confirmations.length, 1);
  assert.match(page.confirmations[0], /od-flash → open-design\/flash-2/);
  assert.match(page.view.document.getElementById('modelState').textContent, /草稿仍然保留/);
  assert.equal(page.api.state.mappings[0].upstream_id, 'open-design/flash-2', 'the row is untouched');

  // Accepting sends the row without a mapping and without an opt-in, which is
  // exactly the backend's "clear the old policy" default.
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1);
  assert.equal(page.posts[0].models[0].reasoning_mapping, undefined);
  assert.equal(page.posts[0].models[0].carry_reasoning_mapping, undefined);
  assert.equal(page.posts[0].models[0].original_id, 'od-flash');
  assert.equal(page.posts[0].models[0].upstream_id, 'open-design/flash-2');
  assert.equal(warnings, 2);
  // The server cleared the policy: the console must not claim otherwise.
  assert.ok(!page.view.document.getElementById('modelState').textContent.includes('思考强度映射已按原模型保留'));
});

test('a row added in the console never invents an original identity', async () => {
  const { api, posts } = modelSavePage();
  await api.render.loadStatus();
  api.handlers.addMapping();
  api.state.mappings[2].id = 'custom';
  api.state.mappings[2].upstream_id = 'open-design/custom';
  await api.handlers.saveModels();
  const added = posts[0].models[2];
  assert.deepEqual(added, { id: 'custom', upstream_id: 'open-design/custom' });
  assert.equal('original_id' in added, false);
  assert.equal('original_upstream_id' in added, false);
  // The rows read from the server stay pinned.
  assert.equal(posts[0].models[0].original_id, 'od-flash');
});

test('an upstream checkbox row is a new row, not a pinned server row', async () => {
  const { view, api, posts } = modelSavePage();
  await api.render.loadStatus();
  api.state.upstream = [{ id: 'open-design/gamma', owned_by: 'vela' }];
  api.state.upstreamFor = 'cred-1';
  api.render.renderUpstream();
  const checkbox = view.document.getElementById('upstreamList').children[0].children[0];
  assert.equal(checkbox.tagName, 'INPUT');
  checkbox.checked = true;
  await checkbox.emit('change');
  assert.deepEqual(plain(api.state.mappings[2]), { id: 'open-design-open-design/gamma', upstream_id: 'open-design/gamma' });

  await api.handlers.saveModels();
  const added = posts[0].models[2];
  assert.deepEqual(added, { id: 'open-design-open-design/gamma', upstream_id: 'open-design/gamma' });
  assert.equal('original_id' in added, false);
  assert.equal('original_upstream_id' in added, false);
});

test('a rejected save keeps every original identity for the retry', async () => {
  const page = modelSavePage({
    onSave: (body, count) => {
      if (count === 1) return { status: 409, body: { error: 'models_revision_conflict' } };
      return null;
    },
  });
  await page.api.render.loadStatus();
  const alias = page.view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-flash-fast';
  alias.emit('input');

  await page.api.handlers.saveModels();
  assert.equal(page.posts[0].models[0].original_id, 'od-flash');
  assert.equal(page.api.state.modelsConflict, true);
  assert.equal(page.api.state.mappings[0].original_id, 'od-flash', 'the draft keeps its pin');

  // A fresh token is required before the overwrite confirmation.
  page.setRevision('list-rev-2');
  await page.api.render.loadStatus();
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 2);
  assert.equal(page.posts[1].expected_models_revision, 'list-rev-2');
  assert.equal(page.posts[1].models[0].original_id, 'od-flash');
  assert.equal(page.posts[1].models[0].original_upstream_id, 'open-design/flash');
});

test('a conflict retry blocks rather than dropping an unresolved original identity', async () => {
  const page = modelSavePage({
    onSave: (body, count) => {
      if (count === 1) return { status: 409, body: { error: 'models_revision_conflict' } };
      return null;
    },
  });
  await page.api.render.loadStatus();
  const alias = page.view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-flash-fast';
  await alias.emit('input');
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1);

  page.setModels([{ id: 'od-pro', upstream_id: 'open-design/pro' }]);
  page.setRevision('list-rev-2');
  await page.api.render.loadStatus();
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1, 'an unresolvable original is never stripped and posted');
  assert.equal(page.api.state.mappings[0].original_id, 'od-flash');
  assert.equal(page.api.state.mappings[0].original_upstream_id, 'open-design/flash');
  assert.match(page.view.document.getElementById('modelState').textContent, /原行.*不存在/);
});

test('deleting or reordering rows keeps every remaining row pinned to its own server row', async () => {
  const { view, api, posts } = modelSavePage();
  await api.render.loadStatus();

  // Reverse the two rows: each keeps the identity of the server row it came
  // from, so the backend still knows which stored policy belongs to which row.
  api.state.mappings = [api.state.mappings[1], api.state.mappings[0]];
  api.render.renderMappings();
  assert.deepEqual(plain(api.state.mappings.map(row => [row.id, row.original_id])), [
    ['od-pro', 'od-pro'],
    ['od-flash', 'od-flash'],
  ]);
  await api.handlers.saveModels();
  assert.deepEqual(posts[0].models.map(row => [row.id, row.original_id]), [
    ['od-pro', 'od-pro'],
    ['od-flash', 'od-flash'],
  ]);

  // Delete the first (now reordered) row through its own button.
  const removeFirst = view.document.getElementById('mappingList').children[0].children[3];
  assert.equal(removeFirst.textContent, '删除');
  await removeFirst.emit('click');
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), ['od-flash']);
  await api.handlers.saveModels();
  assert.deepEqual(posts[1].models, [
    { id: 'od-flash', upstream_id: 'open-design/flash', original_id: 'od-flash', original_upstream_id: 'open-design/flash' },
  ]);
});

test('an alias swap is refused locally instead of arming an unresolvable 409', async () => {
  const { view, api, posts } = modelSavePage();
  await api.render.loadStatus();
  // Swap the two aliases: each row's new alias is the other row's original.
  api.state.mappings[0].id = 'od-pro';
  api.state.mappings[1].id = 'od-flash';
  await api.handlers.saveModels();
  assert.equal(posts.length, 0, 'the unresolvable list is not sent');
  assert.match(view.document.getElementById('modelState').textContent, /原始模型身份/);
  assert.equal(api.state.mappings[0].id, 'od-pro', 'the draft is kept for the operator to fix');
});

test('an old backend without a token gets no original identity and warns before a rename', async () => {
  const page = modelSavePage({ revision: '' });
  await page.api.render.loadStatus();
  assert.equal(page.api.state.modelsRevision, '');
  const alias = page.view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-flash-fast';
  alias.emit('input');

  await page.api.handlers.saveModels();
  assert.equal(page.confirmations.length, 1);
  assert.match(page.confirmations[0], /当前后端不支持按原行关联/);
  assert.deepEqual(page.posts[0], {
    models: [
      { id: 'od-flash-fast', upstream_id: 'open-design/flash' },
      { id: 'od-pro', upstream_id: 'open-design/pro' },
    ],
  });
  assert.equal('expected_models_revision' in page.posts[0], false);
});

test('a session switch re-pins the editor rows to the new snapshot only', async () => {
  const oldModels = pinnedModels();
  const newModels = [{ id: 'od-next', upstream_id: 'open-design/next', reasoning_revision: 'rev-next' }];
  let statusCalls = 0;
  const view = fakePage({
    fetchImpl: async url => {
      if (!url.includes('/status')) return respond(404, { error: 'not found' });
      statusCalls += 1;
      if (statusCalls === 1) {
        return respond(200, statusBody({ models: oldModels, models_revision: 'old-rev' }));
      }
      return respond(200, statusBody({
        models: newModels,
        models_revision: 'new-rev',
        credentials: [{ id: 'cred-1', label: '主账户', disabled: false }],
      }));
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  assert.equal(api.state.mappings[0].original_id, 'od-flash');

  view.document.getElementById('managementKey').value = 'replacement-management-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.deepEqual(plain(api.state.mappings), [
    { id: 'od-next', upstream_id: 'open-design/next', original_id: 'od-next', original_upstream_id: 'open-design/next' },
  ], 'the new session inherits no identity from the old one');
  assert.equal(api.state.modelsRevision, 'new-rev');
});

test('the save body carries original identity only with a whole-list token', () => {
  const core = fakePage().context.OpenDesignCore;
  const rows = [{
    id: 'renamed',
    upstream_id: 'open-design/flash',
    original_id: 'od-flash',
    original_upstream_id: 'open-design/flash',
  }];
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.buildModelsSaveBody(rows, 'rev-1'))),
    {
      models: [{ id: 'renamed', upstream_id: 'open-design/flash', original_id: 'od-flash', original_upstream_id: 'open-design/flash' }],
      expected_models_revision: 'rev-1',
    },
  );
  // Without a token the backend cannot resolve an identity, so it is omitted.
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.buildModelsSaveBody(rows, ''))),
    { models: [{ id: 'renamed', upstream_id: 'open-design/flash' }] },
  );
  // A half-filled pair is never sent.
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.buildModelsSaveBody([{ id: 'a', upstream_id: 'b', original_id: 'a' }], 'rev-1'))),
    { models: [{ id: 'a', upstream_id: 'b' }], expected_models_revision: 'rev-1' },
  );
  // The plain payload shape is unchanged for older callers.
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.buildModelsPayload(rows))),
    [{ id: 'renamed', upstream_id: 'open-design/flash' }],
  );
});

// --- cross-tab model/reasoning consistency and auth-candidate isolation ------

// Two console pages share one in-memory backend. `applyImportLikeBackend` mirrors
// the whole-table write, while a reasoning save returns the complete current list
// (including rows another tab added) with a new whole-list revision.
function sharedModelBackend({ models, revision = 'list-rev-1', legacy = false }) {
  let rows = models.map(model => ({ ...model }));
  let currentRevision = revision;
  // The initial snapshot is already revision 1: the first mutation must not
  // accidentally reuse it, otherwise a stale CAS appears to succeed in tests.
  let counter = 1;
  let reasoningGate = null;
  const posts = [];
  const fetchImpl = async (url, init = {}) => {
    if (url.endsWith('/models/reasoning') && init.method === 'POST') {
      const body = JSON.parse(init.body);
      if (reasoningGate) await reasoningGate;
      const row = rows.find(item => item.id === body.model && item.upstream_id === body.upstream_id);
      if (!row) return respond(404, { error: 'model not found' });
      if (body.expected_revision && body.expected_revision !== (row.reasoning_revision || '')) {
        return respond(409, { error: 'reasoning_revision_conflict' });
      }
      row.reasoning_mapping = body.reasoning_mapping;
      row.reasoning_revision = `reasoning-${++counter}`;
      currentRevision = `list-rev-${++counter}`;
      return respond(200, {
        models: rows.map(item => ({ ...item })),
        models_revision: legacy ? '' : currentRevision,
      });
    }
    if (url.endsWith('/models') && init.method === 'POST') {
      const body = JSON.parse(init.body);
      posts.push(body);
      if ('expected_models_revision' in body && body.expected_models_revision !== currentRevision) {
        return respond(409, { error: 'models_revision_conflict' });
      }
      rows = applyImportLikeBackend(rows, body);
      currentRevision = `list-rev-${++counter}`;
      return respond(200, {
        models: rows.map(item => ({ ...item })),
        models_revision: legacy ? '' : currentRevision,
      });
    }
    if (url.includes('/status')) {
      return respond(200, statusBody({
        models: rows.map(item => ({ ...item })),
        models_revision: legacy ? '' : currentRevision,
      }));
    }
    return respond(404, { error: 'not found' });
  };
  return {
    fetchImpl,
    posts,
    models: () => rows,
    revision: () => currentRevision,
    delayReasoning() {
      let release;
      reasoningGate = new Promise(resolve => { release = resolve; });
      return () => {
        const done = reasoningGate;
        reasoningGate = null;
        release();
        return done;
      };
    },
  };
}

function twoModelTabs(backend) {
  const open = () => {
    const view = fakePage({ fetchImpl: backend.fetchImpl });
    const api = view.context.OpenDesignConsole;
    api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
    return { view, api };
  };
  return { a: open(), b: open() };
}

test('unchecking and rechecking an upstream restores its own pinned rows and other drafts on the original token', async () => {
  const policy = { enabled: true, rules: [{ from: 'medium', to: 'high' }] };
  const backend = sharedModelBackend({ models: [
    { id: 'custom-a', upstream_id: 'up/a', reasoning_mapping: policy },
    { id: 'custom-b', upstream_id: 'up/b' },
    { id: 'another-a', upstream_id: 'up/a' },
  ] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  const originalRevision = a.api.state.modelsRevision;
  const alias = a.view.document.getElementById('mappingList').children[1].children[0].children[1];
  alias.value = 'b-draft';
  await alias.emit('input');
  a.api.state.upstream = [{ id: 'up/a' }];
  a.api.state.upstreamFor = 'cred-1';
  a.api.render.renderUpstream();
  const checkbox = a.view.document.getElementById('upstreamList').children[0].children[0];
  checkbox.checked = false;
  await checkbox.emit('change');
  checkbox.checked = true;
  await checkbox.emit('change');
  assert.deepEqual(plain(a.api.state.mappings.map(row => [row.id, row.original_id])), [
    ['custom-a', 'custom-a'], ['b-draft', 'custom-b'], ['another-a', 'another-a'],
  ], 'restore every removed row in place, not a default alias or a borrowed identity');
  assert.equal(a.api.state.modelsDraftRevision, originalRevision);

  b.api.handlers.addMapping();
  b.api.state.mappings[3].id = 'added-by-b';
  b.api.state.mappings[3].upstream_id = 'up/c';
  await b.api.handlers.saveModels();
  await a.api.render.loadStatus();
  assert.equal(a.api.state.modelsDraftRevision, originalRevision);
  assert.equal(a.api.state.mappings[1].id, 'b-draft');
  await a.api.handlers.saveModels();
  assert.equal(backend.posts.at(-1).expected_models_revision, originalRevision);
  assert.equal(a.api.state.modelsConflict, true, 'the real CAS, not a fabricated same-alias conflict, rejects the stale draft');
  assert.deepEqual(backend.models()[0].reasoning_mapping, policy);
});

test('a clean upstream undo on a legacy backend adopts another page’s later row instead of overwriting it', async () => {
  const backend = sharedModelBackend({ legacy: true, models: [
    { id: 'open-design-m', upstream_id: 'm', reasoning_mapping: { enabled: true, rules: [] } },
  ] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  a.api.state.upstream = [{ id: 'm' }];
  a.api.state.upstreamFor = 'cred-1';
  a.api.render.renderUpstream();
  // Exercise the batch handlers as well as the single-checkbox handler above.
  a.api.handlers.selectVisibleUpstream(false);
  a.api.handlers.selectVisibleUpstream(true);
  assert.equal(a.api.mappingsDirty(), false);
  assert.equal(a.api.state.modelsDraftActive, false);
  assert.equal(a.api.state.mappings[0].original_id, 'open-design-m');
  b.api.handlers.addMapping();
  b.api.state.mappings[1].id = 'new-from-b';
  b.api.state.mappings[1].upstream_id = 'up/new';
  await b.api.handlers.saveModels();
  await a.api.render.loadStatus();
  assert.deepEqual(plain(a.api.state.mappings.map(row => row.id)), ['open-design-m', 'new-from-b']);
  await a.api.handlers.saveModels();
  assert.deepEqual(backend.models().map(row => row.id), ['open-design-m', 'new-from-b']);
});

test('a checkbox row newly created in another tab is never treated as a local undo', async () => {
  const backend = sharedModelBackend({ models: [] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  for (const tab of [a, b]) {
    tab.api.state.upstream = [{ id: 'm' }];
    tab.api.state.upstreamFor = 'cred-1';
    tab.api.render.renderUpstream();
    const box = tab.view.document.getElementById('upstreamList').children[0].children[0];
    box.checked = true;
    await box.emit('change');
  }
  await b.api.handlers.saveModels();
  b.api.handlers.selectReasoningModel('open-design-m\u0000m');
  await b.api.handlers.setReasoningEnabled(true);
  await b.api.handlers.saveReasoning();
  await a.api.handlers.saveModels();
  await a.api.render.loadStatus();
  const before = backend.posts.length;
  await a.api.handlers.saveModels();
  assert.equal(backend.posts.length, before);
  assert.equal(a.api.state.mappings[0].original_id, undefined);
  assert.match(a.view.document.getElementById('modelState').textContent, /同名|别名/);
  assert.equal(backend.models()[0].reasoning_mapping.enabled, true);
});

test('a completed upstream removal cannot later resurrect the deleted pin', async () => {
  const page = modelSavePage({ models: [{ id: 'custom', upstream_id: 'm' }] });
  await page.api.render.loadStatus();
  page.api.state.upstream = [{ id: 'm' }];
  page.api.state.upstreamFor = 'cred-1';
  page.api.render.renderUpstream();
  page.api.handlers.selectVisibleUpstream(false);
  await page.api.handlers.saveModels();
  page.api.handlers.selectVisibleUpstream(true);
  assert.deepEqual(plain(page.api.state.mappings), [{ id: 'open-design-m', upstream_id: 'm' }]);
});

test('re-checking an upstream whose original server row was replaced keeps the old pin and never binds the replacement', async () => {
  // Legacy backend: there is no whole-list token to catch the replacement row.
  const backend = sharedModelBackend({ legacy: true, models: [
    { id: 'custom', upstream_id: 'up/x', reasoning_mapping: { enabled: true, rules: [{ from: 'low', to: 'max' }] } },
  ] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  a.api.state.upstream = [{ id: 'up/x' }];
  a.api.state.upstreamFor = 'cred-1';
  a.api.render.renderUpstream();
  a.api.handlers.selectVisibleUpstream(false);
  assert.deepEqual(plain(a.api.state.mappings), []);

  // Another page replaces the row with a default alias and its own new policy.
  b.view.document.getElementById('mappingList').children[0].children[3].emit('click');
  await b.api.handlers.saveModels();
  b.api.handlers.addMapping();
  b.api.state.mappings[0].id = 'open-design-up/x';
  b.api.state.mappings[0].upstream_id = 'up/x';
  await b.api.handlers.saveModels();
  b.api.handlers.selectReasoningModel('open-design-up/x\u0000up/x');
  await b.api.handlers.setReasoningEnabled(true);
  await b.api.handlers.addReasoningRule();
  await b.api.handlers.saveReasoning();
  const replacement = backend.models()[0].reasoning_mapping;
  assert.ok(replacement && replacement.enabled);

  a.api.render.applyStatus(statusBody({ models: backend.models(), models_revision: '' }));
  a.api.handlers.selectVisibleUpstream(true);
  assert.deepEqual(plain(a.api.state.mappings.map(row => [row.id, row.original_id])), [['custom', 'custom']],
    'the page restores its own row and pin instead of adopting the default alias');
  assert.match(a.view.document.getElementById('modelState').textContent, /原行.*已不存在/);
  const before = backend.posts.length;
  await a.api.handlers.saveModels();
  assert.equal(backend.posts.length, before, 'the legacy alias match must not be adopted silently');
  assert.deepEqual(backend.models()[0].reasoning_mapping, replacement);
  const adopt = Array.from(a.view.document.getElementById('mappingList').children[0].children)
    .find(node => node.getAttribute('data-action') === 'adopt-server-mapping');
  assert.equal(adopt, undefined, 'no exact server row exists, so no row-level adoption is offered');
  a.api.handlers.revertMappings();
  assert.deepEqual(plain(a.api.state.mappings.map(row => row.id)), ['open-design-up/x'],
    'only the explicit revert adopts the server list');
});

test('a reasoning save adopts the returned whole list so the next model save cannot delete another tab row', async () => {
  const backend = sharedModelBackend({
    models: [{
      id: 'od-x',
      upstream_id: 'upstream/x',
      reasoning_revision: 'reasoning-x',
      reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] },
    }],
  });
  const tabs = twoModelTabs(backend);
  await tabs.a.api.render.loadStatus();
  await tabs.b.api.render.loadStatus();

  await tabs.a.api.handlers.addReasoningRule();
  const releaseReasoning = backend.delayReasoning();
  const savingReasoning = tabs.a.api.handlers.saveReasoning();
  await new Promise(setImmediate);

  // Tab B adds a row while A's reasoning POST is still in flight. A's answer is
  // therefore the first time A learns about the new row and its revision.
  tabs.b.api.handlers.addMapping();
  tabs.b.api.state.mappings[1].id = 'od-y';
  tabs.b.api.state.mappings[1].upstream_id = 'upstream/y';
  await tabs.b.api.handlers.saveModels();
  assert.deepEqual(plain(backend.models().map(model => model.id)), ['od-x', 'od-y']);

  releaseReasoning();
  await savingReasoning;
  const adoptedRevision = backend.revision();
  assert.deepEqual(plain(tabs.a.api.state.serverModels.map(model => model.id)), ['od-x', 'od-y']);
  assert.deepEqual(plain(tabs.a.api.state.mappings.map(row => row.id)), ['od-x', 'od-y'],
    'a clean editor adopts the new whole list together with its token');
  assert.deepEqual(plain(tabs.a.api.state.savedMappings.map(row => row.id)), ['od-x', 'od-y']);
  assert.equal(tabs.a.api.state.modelsRevision, adoptedRevision);
  assert.equal(tabs.a.api.state.modelsDraftActive, false);

  await tabs.a.api.handlers.saveModels();
  const sent = backend.posts[backend.posts.length - 1];
  assert.deepEqual(sent.models.map(row => row.id), ['od-x', 'od-y'],
    'A must not post its pre-reasoning list under the newer revision token');
  assert.equal(sent.expected_models_revision, adoptedRevision);
  assert.deepEqual(plain(backend.models().map(model => model.id)), ['od-x', 'od-y'],
    'the row added by B is still saved');
});

test('a model mapping draft survives a reasoning response and keeps its original CAS revision', async () => {
  const backend = sharedModelBackend({
    models: [{
      id: 'od-x',
      upstream_id: 'upstream/x',
      reasoning_revision: 'reasoning-x',
      reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] },
    }],
  });
  const tabs = twoModelTabs(backend);
  await tabs.a.api.render.loadStatus();
  await tabs.b.api.render.loadStatus();
  const baseRevision = tabs.a.api.state.modelsRevision;

  const alias = tabs.a.view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-x-renamed';
  alias.emit('input');
  assert.equal(tabs.a.api.state.modelsDraftActive, true);
  await tabs.a.api.handlers.addReasoningRule();
  const releaseReasoning = backend.delayReasoning();
  const savingReasoning = tabs.a.api.handlers.saveReasoning();
  await new Promise(setImmediate);

  tabs.b.api.handlers.addMapping();
  tabs.b.api.state.mappings[1].id = 'od-y';
  tabs.b.api.state.mappings[1].upstream_id = 'upstream/y';
  await tabs.b.api.handlers.saveModels();
  releaseReasoning();
  await savingReasoning;

  assert.deepEqual(plain(tabs.a.api.state.mappings.map(row => row.id)), ['od-x-renamed'],
    'the unsaved mapping draft is not replaced by the response list');
  assert.deepEqual(plain(tabs.a.api.state.savedMappings.map(row => row.id)), ['od-x', 'od-y'],
    'the saved baseline follows the response');
  assert.equal(tabs.a.api.state.modelsDraftRevision, baseRevision,
    'the draft keeps the revision it was edited against');
  assert.equal(tabs.a.api.state.modelsDraftActive, true);

  await tabs.a.api.handlers.saveModels();
  const sent = backend.posts[backend.posts.length - 1];
  assert.equal(sent.expected_models_revision, baseRevision,
    'the stale draft revision reaches the server so it can return 409');
  assert.equal(tabs.a.api.state.modelsConflict, true, 'the competing change is surfaced as a conflict');
  assert.deepEqual(plain(tabs.a.api.state.mappings.map(row => row.id)), ['od-x-renamed'],
    'the rejected draft is kept for an explicit overwrite decision');
});

test('two tabs cannot rebase an old pin onto a replacement row and inherit its policy', async () => {
  const original = { enabled: true, rules: [{ from: 'low', to: 'medium' }] };
  const backend = sharedModelBackend({ models: [{ id: 'a', upstream_id: 'up/1', reasoning_revision: 'rev-a', reasoning_mapping: original }] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  const upstream = a.view.document.getElementById('mappingList').children[0].children[2].children[1];
  upstream.value = 'up/2';
  await upstream.emit('input');
  await b.view.document.getElementById('mappingList').children[0].children[3].emit('click');
  await b.api.handlers.saveModels(); // delete the old row in a completed save
  b.api.handlers.addMapping();
  b.api.state.mappings[0].id = 'a';
  b.api.state.mappings[0].upstream_id = 'up/2';
  await b.api.handlers.saveModels();
  b.api.handlers.selectReasoningModel('a\u0000up/2');
  await b.api.handlers.setReasoningEnabled(true);
  await b.api.handlers.addReasoningRule();
  await b.api.handlers.saveReasoning();
  const replacementPolicy = backend.models()[0].reasoning_mapping;
  assert.ok(replacementPolicy && replacementPolicy.enabled);
  assert.notDeepEqual(replacementPolicy, original);

  await a.api.handlers.saveModels(); // A still submits its original list revision
  assert.equal(a.api.state.modelsConflict, true);
  await a.api.render.loadStatus();
  const beforeRetry = backend.posts.length;
  await a.api.handlers.saveModels();
  assert.equal(backend.posts.length, beforeRetry, 'must not POST after dropping the old pin');
  assert.equal(a.api.state.mappings[0].original_id, 'a');
  assert.equal(a.api.state.mappings[0].original_upstream_id, 'up/1');
  assert.deepEqual(backend.models()[0].reasoning_mapping, replacementPolicy);
  assert.match(a.view.document.getElementById('modelState').textContent, /原行.*不存在/);
  assert.equal(a.view.document.getElementById('revertMappings').hidden, false);
  a.api.handlers.revertMappings();
  assert.equal(a.api.state.mappings[0].original_upstream_id, 'up/2', 'only explicit revert adopts the replacement row');
  assert.equal(a.api.mappingsDirty(), false);
});

test('two tabs cannot implicitly bind a new row to a concurrently created alias', async () => {
  const backend = sharedModelBackend({ models: [] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  for (const tab of [a, b]) {
    tab.api.handlers.addMapping();
    tab.api.state.mappings[0].id = 'a';
    tab.api.state.mappings[0].upstream_id = 'up/2';
  }
  await b.api.handlers.saveModels();
  b.api.handlers.selectReasoningModel('a\u0000up/2');
  await b.api.handlers.setReasoningEnabled(true);
  await b.api.handlers.addReasoningRule();
  await b.api.handlers.saveReasoning();
  const policy = backend.models()[0].reasoning_mapping;
  await a.api.handlers.saveModels();
  assert.equal(a.api.state.modelsConflict, true);
  await a.api.render.loadStatus();
  const beforeRetry = backend.posts.length;
  await a.api.handlers.saveModels();
  assert.equal(backend.posts.length, beforeRetry, 'un-pinned new row must not silently inherit the other tab’s policy');
  assert.equal(a.api.state.mappings[0].original_id, undefined);
  assert.deepEqual(backend.models()[0].reasoning_mapping, policy);
  assert.match(a.view.document.getElementById('modelState').textContent, /同名|别名/);
});

test('a draft started before CAS was advertised cannot rebase a new colliding alias', async () => {
  const backend = sharedModelBackend({ models: [], revision: '' });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  for (const tab of [a, b]) {
    tab.api.handlers.addMapping();
    tab.api.state.mappings[0].id = 'a';
    tab.api.state.mappings[0].upstream_id = 'up/2';
  }
  await b.api.handlers.saveModels(); // the backend starts advertising a token
  await a.api.render.loadStatus();
  assert.equal(a.api.state.modelsDraftRevision, '');
  const before = backend.posts.length;
  await a.api.handlers.saveModels(); // require confirmation for a tokenless draft
  assert.equal(a.api.state.modelsConflict, true);
  await a.api.handlers.saveModels(); // even confirmation cannot bind the new alias silently
  assert.equal(backend.posts.length, before);
  assert.equal(a.api.state.modelsDraftRevision, '');
  assert.match(a.view.document.getElementById('modelState').textContent, /同名|别名/);
});

test('a reasoning response preserves a same-looking rename draft and its original identity', async () => {
  const backend = sharedModelBackend({ models: [
    { id: 'r', upstream_id: 'up/r', reasoning_revision: 'rr' },
    { id: 'x', upstream_id: 'up/x', reasoning_revision: 'rx',
      reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } },
  ] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  const originalRevision = a.api.state.modelsRevision;

  const alias = a.view.document.getElementById('mappingList').children[1].children[0].children[1];
  alias.value = 'y';
  await alias.emit('input');
  assert.equal(a.api.state.mappings[1].original_id, 'x');
  await b.view.document.getElementById('mappingList').children[1].children[3].emit('click');
  b.api.handlers.addMapping();
  b.api.state.mappings[1].id = 'y';
  b.api.state.mappings[1].upstream_id = 'up/x';
  await b.api.handlers.saveModels();
  assert.equal(backend.models()[1].reasoning_mapping, undefined, 'B created a distinct row without X policy');

  a.api.handlers.selectReasoningModel('r\u0000up/r');
  await a.api.handlers.addReasoningRule();
  await a.api.handlers.saveReasoning();
  assert.equal(a.api.state.modelsRevision, backend.revision(), `reasoning save must adopt the returned revision: ${a.view.document.getElementById('reasoningState').textContent}`);
  assert.notEqual(a.api.state.modelsRevision, originalRevision, 'the reasoning response must be newer');
  assert.equal(a.api.state.modelsDraftActive, true, 'an equal visible list is not an equal original identity');
  assert.equal(a.api.state.modelsDraftRevision, originalRevision, 'keep the snapshot that owns X');
  assert.equal(a.api.state.mappings[1].original_id, 'x', 'preserve X→Y migration intent');
  assert.equal(a.api.state.mappings[1].original_upstream_id, 'up/x');
  const beforeSave = backend.posts.length;
  await a.api.handlers.saveModels();
  assert.equal(backend.posts.length, beforeSave, 'an unresolved pin is blocked before POST');
  assert.equal(a.api.state.modelsDraftRevision, originalRevision, 'the old CAS base is not replaced');
  assert.equal(a.api.state.mappings[1].original_id, 'x', 'the draft is not rebound to Y');
});

test('a status refresh preserves same-looking rows whose original identities differ', async () => {
  const backend = sharedModelBackend({ models: [
    { id: 'r', upstream_id: 'up/r', reasoning_revision: 'rr' },
    { id: 'x', upstream_id: 'up/x', reasoning_revision: 'rx',
      reasoning_mapping: { enabled: true, rules: [{ from: 'medium', to: 'high' }] } },
  ] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  const originalRevision = a.api.state.modelsRevision;
  const alias = a.view.document.getElementById('mappingList').children[1].children[0].children[1];
  alias.value = 'y';
  await alias.emit('input');
  await b.view.document.getElementById('mappingList').children[1].children[3].emit('click');
  b.api.handlers.addMapping();
  b.api.state.mappings[1].id = 'y';
  b.api.state.mappings[1].upstream_id = 'up/x';
  await b.api.handlers.saveModels();
  await a.api.render.loadStatus();
  assert.equal(a.api.state.modelsDraftActive, true);
  assert.equal(a.api.state.modelsDraftRevision, originalRevision);
  assert.equal(a.api.state.mappings[1].original_id, 'x');
  const beforeSave = backend.posts.length;
  await a.api.handlers.saveModels();
  assert.equal(backend.posts.length, beforeSave, 'an unresolved pin is blocked before POST');
  assert.equal(a.api.state.modelsDraftRevision, originalRevision);
  assert.match(a.view.document.getElementById('modelState').textContent, /原行.*不存在/);
});

test('a reasoning response repaints mapping rows before an operator edits them', async () => {
  const policy = { enabled: true, rules: [{ from: 'medium', to: 'high' }] };
  const backend = sharedModelBackend({ models: [
    { id: 'a', upstream_id: 'up/a', reasoning_revision: 'ra' },
    { id: 'b', upstream_id: 'up/b', reasoning_revision: 'rb', reasoning_mapping: policy },
    { id: 'c', upstream_id: 'up/c', reasoning_revision: 'rc' },
  ] });
  const { a, b } = twoModelTabs(backend);
  await a.api.render.loadStatus();
  await b.api.render.loadStatus();
  await b.view.document.getElementById('mappingList').children[0].children[3].emit('click');
  await b.api.handlers.saveModels();
  a.api.handlers.selectReasoningModel('b\u0000up/b');
  await a.api.handlers.saveReasoning();

  const list = a.view.document.getElementById('mappingList');
  assert.equal(list.children.length, 2, 'the removed row is removed from the actual DOM');
  const alias = list.children[0].children[0].children[1];
  const upstream = list.children[0].children[2].children[1];
  assert.equal(alias.value, 'b');
  assert.equal(upstream.value, 'up/b');
  alias.value = 'b-renamed';
  await alias.emit('input');
  await a.api.handlers.saveModels();
  const sent = backend.posts.at(-1).models[0];
  assert.equal(sent.id, 'b-renamed');
  assert.equal(sent.upstream_id, 'up/b', 'the row submitted is the one the operator saw');
  assert.equal(sent.original_id, 'b');
  assert.deepEqual(backend.models()[0].reasoning_mapping, policy, 'the displayed row keeps its own strategy');
});

test('a candidate key is only used to verify itself and a failed re-validation keeps the verified session', async () => {
  const pendingVerifications = [];
  const upstreamAuth = [];
  let statusCalls = 0;
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/status')) {
        statusCalls += 1;
        if (statusCalls === 1) return respond(200, statusBody({ models_revision: 'list-rev-1' }));
        const request = deferred();
        pendingVerifications.push(request);
        return request.promise;
      }
      if (url.includes('/upstream-models')) {
        upstreamAuth.push(init.headers.Authorization || init.headers['X-Management-Key']);
        return respond(200, { data: [{ id: 'open-design/flash' }] });
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();
  const alias = view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-flash-draft';
  alias.emit('input');
  const draftIds = plain(api.state.mappings.map(row => row.id));

  view.document.getElementById('managementKey').value = 'candidate-management-key';
  const verifyingCandidate = api.handlers.submitAuth({ preventDefault() {} });
  await new Promise(setImmediate);
  assert.equal(pendingVerifications.length, 1);

  // A regular request made while the candidate is being verified must keep using
  // the already verified identity, not the unverified candidate key.
  api.state.selectedCredential = 'cred-1';
  view.document.getElementById('modelCredential').value = 'cred-1';
  await api.handlers.fetchModels();
  assert.deepEqual(upstreamAuth, [`Bearer ${managementKey}`],
    'the candidate key never authenticates an ordinary management request');

  pendingVerifications[0].resolve(respond(401, { error: 'invalid candidate key' }));
  await verifyingCandidate;
  assert.equal(api.state.verified && api.state.verified.headers.Authorization, `Bearer ${managementKey}`,
    'a rejected candidate does not log out the verified session');
  assert.equal(api.state.modelsRevision, 'list-rev-1');
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), draftIds, 'the mapping draft survives');
  assert.equal(api.state.reasoningDrafts.size, 1, 'the reasoning draft survives');
  assert.equal(api.state.candidate, undefined,
    'the candidate key is never stored as page auth');
  assert.match(view.document.getElementById('authState').textContent, /当前已验证会话/,
    'the operator is told the verified session was kept, not logged out');
  assert.equal(view.document.getElementById('accountsPanel').hidden, false);

  // A failed same-key re-validation is also non-destructive.
  view.document.getElementById('managementKey').value = managementKey;
  const verifyingSameKey = api.handlers.submitAuth({ preventDefault() {} });
  await new Promise(setImmediate);
  assert.equal(pendingVerifications.length, 2);
  pendingVerifications[1].resolve(respond(401, { error: 'key now rejected' }));
  await verifyingSameKey;
  assert.equal(api.state.verified && api.state.verified.headers.Authorization, `Bearer ${managementKey}`);
  assert.deepEqual(plain(api.state.mappings.map(row => row.id)), draftIds);
  assert.equal(api.state.reasoningDrafts.size, 1);
});

test('a late /status from a same-key re-validation cannot roll back a newer reasoning save', async () => {
  const pendingVerification = deferred();
  let statusCalls = 0;
  const model = (revision, rules) => ({
    id: 'od-x', upstream_id: 'upstream/x', reasoning_revision: revision,
    reasoning_mapping: { enabled: true, rules },
  });
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      if (url.endsWith('/status')) {
        statusCalls += 1;
        if (statusCalls === 1) {
          return respond(200, statusBody({
            models: [model('reasoning-0', [{ from: 'medium', to: 'high' }])],
            models_revision: 'list-rev-1',
          }));
        }
        return pendingVerification.promise;
      }
      if (url.endsWith('/models/reasoning') && init.method === 'POST') {
        return respond(200, {
          models: [
            model('reasoning-1', [{ from: 'low', to: 'low' }, { from: 'medium', to: 'high' }]),
            { id: 'od-y', upstream_id: 'upstream/y', reasoning_revision: 'reasoning-y' },
          ],
          models_revision: 'list-rev-2',
        });
      }
      return respond(404, { error: 'not found' });
    },
  });
  const api = view.context.OpenDesignConsole;
  api.state.verified = { headers: { Authorization: `Bearer ${managementKey}` }, source: 'manual' };
  await api.render.loadStatus();
  await api.handlers.addReasoningRule();

  // The same-key re-validation starts before the reasoning save, so its /status
  // answer describes the older list even though the save is newer.
  view.document.getElementById('managementKey').value = managementKey;
  const verifying = api.handlers.submitAuth({ preventDefault() {} });
  await new Promise(setImmediate);
  assert.equal(statusCalls, 2);

  await api.handlers.saveReasoning();
  assert.equal(api.state.modelsRevision, 'list-rev-2');
  assert.deepEqual(plain(api.state.serverModels.map(item => item.id)), ['od-x', 'od-y']);
  // A new draft is made after the save; the stale status must not hide its model
  // or roll its baseline back.
  api.handlers.selectReasoningModel('od-y\u0000upstream/y');
  assert.match(api.state.reasoningModelKey, /^od-y/, 'the model added by the save is selectable');
  await api.handlers.addReasoningRule();
  assert.equal(api.state.reasoningDrafts.size, 1);

  pendingVerification.resolve(respond(200, statusBody({
    models: [model('reasoning-0', [{ from: 'medium', to: 'high' }])],
    models_revision: 'list-rev-1',
  })));
  await verifying;
  assert.equal(api.state.modelsRevision, 'list-rev-2', 'the late answer does not move the whole-list token back');
  assert.deepEqual(plain(api.state.serverModels.map(item => item.id)), ['od-x', 'od-y']);
  assert.equal(api.state.serverModels[0].reasoning_revision, 'reasoning-1');
  assert.equal(api.state.reasoningDrafts.size, 1, 'the newer reasoning draft is kept');
  assert.equal(api.render.reasoningDirty(), true);
});

// Authentication races use real console handlers and deferred network replies.
function authRaceFixture() {
  const pending = deferred();
  let intercept = null;
  const status = statusBody({
    models: [{ id: 'od-flash', upstream_id: 'open-design/flash', reasoning_revision: 'reason-1' }],
    models_revision: 'list-1',
    credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
  });
  const view = fakePage({
    fetchImpl: (url, init) => {
      if (intercept && intercept(url, init)) {
        intercept = null;
        return pending.promise;
      }
      if (url.endsWith('/status')) return respond(200, status);
      if (url.endsWith('/credentials') || url.includes('/credentials?')) return respond(200, { id: 'cred-1', label: '主账户' });
      return respond(404, { error: 'unexpected request' });
    },
  });
  const api = view.context.OpenDesignConsole;
  return { view, api, pending, hold: matches => { intercept = matches; } };
}

async function authRaceLogin(fixture, value = managementKey) {
  fixture.view.document.getElementById('managementKey').value = value;
  await fixture.api.handlers.submitAuth({ preventDefault() {} });
}

async function authRaceDrafts({ view, api }) {
  await api.handlers.addReasoningRule();
  const alias = view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'unsaved-alias';
  await alias.emit('input');
  assert.equal(api.state.reasoningDrafts.size, 1);
  assert.equal(api.state.modelsDraftActive, true);
}

test('auth race: same-key revalidation fences old management rejections at real handlers', async t => {
  const cases = [
    ['status', url => url.endsWith('/status'), f => f.api.render.loadStatus(), 'authState'],
    ['model save', url => url.endsWith('/models'), f => f.api.handlers.saveModels(), 'modelState'],
    ['reasoning save', url => url.endsWith('/models/reasoning'), f => f.api.handlers.saveReasoning(), 'reasoningState'],
    ['account add', url => url.endsWith('/credentials'), f => {
      f.view.document.getElementById('accountKey').value = key;
      return f.api.handlers.addAccount({ preventDefault() {} });
    }, 'accountState'],
    ['account delete', url => url.includes('/credentials?'), f => f.api.handlers.deleteAccount(f.api.state.status.credentials[0]), 'accountState'],
    ['account toggle', url => url.endsWith('/credentials'), f => f.api.handlers.toggleAccount(f.api.state.status.credentials[0]), 'accountState'],
    ['model fetch', url => url.includes('/upstream-models'), f => {
      f.api.handlers.selectCredential('cred-1');
      return f.api.handlers.fetchModels();
    }, 'modelState', 'management'],
    ['model coverage', url => url.includes('/model-coverage'), f => {
      f.api.handlers.selectCredential('cred-1');
      f.api.state.upstreamFor = 'cred-1';
      return f.api.handlers.checkModelCoverage({ id: 'open-design/flash' });
    }, 'modelState'],
    ['quota', url => url.includes('/quota?'), f => f.api.handlers.refreshQuota(f.api.state.status.credentials[0]), 'accountState'],
    ['quota resume', url => url.endsWith('/quota-pause/resume'), f => f.api.handlers.resumeQuotaPause(f.api.state.status.credentials[0]), 'accountState'],
    ['quota recheck', url => url.endsWith('/quota-pause/recheck'), f => f.api.handlers.recheckQuotaPause(f.api.state.status.credentials[0]), 'accountState'],
    ['optional config', url => url.endsWith('/config'), f => f.api.handlers.loadConfig(), 'configState'],
    ['optional logs', url => url.includes('/request-logs'), f => f.api.handlers.loadRequestLogs({ reset: true }), 'logsState'],
  ];
  for (const [name, matches, invoke, messageID, source] of cases) {
    for (const status of [401, 403]) await t.test(`${name} ${status}`, async () => {
      const f = authRaceFixture();
      await authRaceLogin(f);
      await authRaceDrafts(f);
      f.hold(matches);
      const flight = invoke(f);
      await new Promise(setImmediate);
      assert.ok(f.view.calls.some(call => matches(call.url, call.init)), 'the real handler sent its request');
      await authRaceLogin(f);
      const authMessage = f.view.document.getElementById('authState').textContent;
      const beforeMessage = f.view.document.getElementById(messageID).textContent;
      const beforeMappings = plain(f.api.state.mappings);
      const beforeDrafts = plain([...f.api.state.reasoningDrafts]);
      const beforeStorage = [...f.view.sessionStorage.map];
      f.pending.resolve(respond(status, { error: source ? { source, message: 'old management rejection' } : 'old management rejection' }));
      await flight;
      assert.equal(f.api.state.verified?.headers.Authorization, `Bearer ${managementKey}`);
      assert.equal(f.view.document.getElementById('accountsPanel').hidden, false);
      assert.deepEqual(plain(f.api.state.mappings), beforeMappings);
      assert.deepEqual(plain([...f.api.state.reasoningDrafts]), beforeDrafts);
      assert.deepEqual([...f.view.sessionStorage.map], beforeStorage);
      assert.equal(f.view.document.getElementById('authState').textContent, authMessage);
      assert.equal(f.view.document.getElementById(messageID).textContent, beforeMessage, 'old rejection must not present a current error');
      assert.equal(f.api.state.busy, false, 'the completed operation releases its own lock');
      assert.ok(!f.view.calls.some(call => call.url.includes(managementKey) || String(call.init.body || '').includes(managementKey)), 'management key stays out of URLs and bodies');
    });
  }
});

test('auth race: a current-generation rejection still logs out, upstream rejection does not', async t => {
  for (const [name, source, expectLogout] of [['management', 'management', true], ['upstream', 'upstream', false]]) {
    for (const status of [401, 403]) await t.test(`${name} ${status}`, async () => {
      const f = authRaceFixture();
      await authRaceLogin(f);
      await authRaceDrafts(f);
      await authRaceLogin(f);
      f.hold(url => url.endsWith('/models'));
      const flight = f.api.handlers.saveModels();
      await new Promise(setImmediate);
      f.pending.resolve(respond(status, { error: { message: 'rejected', source } }));
      await flight;
      assert.equal(f.api.state.verified === null, expectLogout);
      if (expectLogout) {
        assert.match(f.view.document.getElementById('authState').textContent, new RegExp(String(status)));
        assert.equal(f.view.sessionStorage.keys().length, 0);
      } else {
        assert.equal(f.api.state.reasoningDrafts.size, 1);
        assert.match(f.view.document.getElementById('modelState').textContent, /上游/);
      }
    });
  }
});

test('auth race: fresh management errors still revoke from every auth exit', async t => {
  const cases = [
    ['status', url => url.endsWith('/status'), f => f.api.render.loadStatus()],
    ['account add', url => url.endsWith('/credentials'), f => {
      f.view.document.getElementById('accountKey').value = key;
      return f.api.handlers.addAccount({ preventDefault() {} });
    }],
    ['account delete', url => url.includes('/credentials?'), f => f.api.handlers.deleteAccount(f.api.state.status.credentials[0])],
    ['account toggle', url => url.endsWith('/credentials'), f => f.api.handlers.toggleAccount(f.api.state.status.credentials[0])],
    ['reasoning save', url => url.endsWith('/models/reasoning'), f => f.api.handlers.saveReasoning()],
    ['model fetch (explicit management source)', url => url.includes('/upstream-models'), f => {
      f.api.handlers.selectCredential('cred-1');
      return f.api.handlers.fetchModels();
    }, 'management'],
    ['coverage', url => url.includes('/model-coverage'), f => {
      f.api.handlers.selectCredential('cred-1');
      f.api.state.upstreamFor = 'cred-1';
      return f.api.handlers.checkModelCoverage({ id: 'open-design/flash' });
    }],
    ['quota', url => url.includes('/quota?'), f => f.api.handlers.refreshQuota(f.api.state.status.credentials[0])],
    ['quota resume', url => url.endsWith('/quota-pause/resume'), f => f.api.handlers.resumeQuotaPause(f.api.state.status.credentials[0])],
    ['quota recheck', url => url.endsWith('/quota-pause/recheck'), f => f.api.handlers.recheckQuotaPause(f.api.state.status.credentials[0])],
    ['optional config', url => url.endsWith('/config'), f => f.api.handlers.loadConfig()],
  ];
  for (const [name, matches, invoke, source] of cases) await t.test(name, async () => {
    const f = authRaceFixture();
    await authRaceLogin(f);
    await authRaceDrafts(f);
    await authRaceLogin(f);
    f.hold(matches);
    const flight = invoke(f);
    await new Promise(setImmediate);
    assert.ok(f.view.calls.some(call => matches(call.url, call.init)));
    f.pending.resolve(respond(401, { error: source ? { source, message: 'current key rejected' } : 'current key rejected' }));
    await flight;
    assert.equal(f.api.state.verified, null);
    assert.match(f.view.document.getElementById('authState').textContent, /401/);
    assert.equal(f.view.sessionStorage.keys().length, 0);
  });
});

test('auth race: an upstream rejection remains an upstream error after same-key revalidation', async t => {
  for (const status of [401, 403]) await t.test(String(status), async () => {
    const f = authRaceFixture();
    await authRaceLogin(f);
    f.api.handlers.selectCredential('cred-1');
    f.hold(url => url.includes('/upstream-models'));
    const flight = f.api.handlers.fetchModels();
    await new Promise(setImmediate);
    await authRaceLogin(f);
    f.pending.resolve(respond(status, { error: { source: 'upstream', message: 'account rejected' } }));
    await flight;
    assert.ok(f.api.state.verified);
    assert.match(f.view.document.getElementById('modelState').textContent, /上游/);
    assert.match(f.view.document.getElementById('modelState').textContent, /管理认证仍有效/);
  });
});

test('auth race: a failed same-key verification does not supersede an earlier rejection', async () => {
  const write = deferred();
  let statuses = 0;
  const view = fakePage({ fetchImpl: url => {
    if (url.endsWith('/models')) return write.promise;
    if (url.endsWith('/status')) return ++statuses === 1
      ? respond(200, statusBody({ models_revision: 'list-1' }))
      : respond(401, { error: 'candidate rejected' });
    return respond(404, {});
  } });
  const api = view.context.OpenDesignConsole;
  const input = view.document.getElementById('managementKey');
  input.value = managementKey;
  await api.handlers.submitAuth({ preventDefault() {} });
  await api.handlers.addReasoningRule();
  const pending = api.handlers.saveModels();
  await new Promise(setImmediate);
  input.value = managementKey;
  await api.handlers.submitAuth({ preventDefault() {} });
  assert.ok(api.state.verified, 'a failed candidate alone does not revoke the session');
  assert.equal(api.state.reasoningDrafts.size, 1);
  write.resolve(respond(401, { error: 'verified key rejected' }));
  await pending;
  assert.equal(api.state.verified, null, 'a failed verification did not establish a newer generation');
  assert.match(view.document.getElementById('authState').textContent, /401/);
});

test('auth race: stale status success and transport failures cannot replace a later verification', async t => {
  for (const result of ['success', 'server error', 'transport error']) await t.test(result, async () => {
    const gate = deferred();
    let statusCalls = 0;
    const view = fakePage({ fetchImpl: url => {
      if (!url.endsWith('/status')) return respond(404, {});
      statusCalls += 1;
      if (statusCalls === 2) return gate.promise;
      return respond(200, statusBody({ credentials: [{ id: 'fresh', label: '新账户' }], models_revision: 'fresh' }));
    } });
    const api = view.context.OpenDesignConsole;
    const login = async () => {
      view.document.getElementById('managementKey').value = managementKey;
      await api.handlers.submitAuth({ preventDefault() {} });
    };
    await login();
    const old = api.render.loadStatus();
    await new Promise(setImmediate);
    await login();
    if (result === 'success') gate.resolve(respond(200, statusBody({ credentials: [{ id: 'old', label: '旧账户' }], models_revision: 'old' })));
    else if (result === 'server error') gate.resolve(respond(500, { error: 'old failure' }));
    else gate.reject(new Error('old network failure'));
    await old;
    assert.ok(api.state.verified);
    assert.equal(api.state.status.credentials[0].id, 'fresh');
    assert.equal(api.state.modelsRevision, 'fresh');
    assert.match(view.document.getElementById('authState').textContent, /验证成功/);
  });
});

test('auth race: logout invalidates attempt A without letting its finally unlock B', async t => {
  for (const aStatus of [200, 401]) await t.test(`A returns ${aStatus}`, async () => {
    const pending = [];
    const view = fakePage({ fetchImpl: (url, init) => {
      const gate = deferred();
      pending.push({ gate, url, headers: init.headers });
      return gate.promise;
    } });
    const api = view.context.OpenDesignConsole;
    const input = view.document.getElementById('managementKey');
    input.value = 'key-A';
    const a = api.handlers.submitAuth({ preventDefault() {} });
    await new Promise(setImmediate);
    api.handlers.clearAuth();
    input.value = 'key-B';
    const b = api.handlers.submitAuth({ preventDefault() {} });
    await new Promise(setImmediate);
    assert.equal(pending.length, 2);
    pending[0].gate.resolve(respond(aStatus, aStatus === 200 ? statusBody({ models_revision: 'A' }) : { error: 'A rejected' }));
    await a;
    assert.equal(api.state.authSubmitting, true, 'B still owns the submit lock');
    assert.equal(view.document.getElementById('authSubmit').disabled, true);
    input.value = 'key-C';
    const blocked = api.handlers.submitAuth({ preventDefault() {} });
    await new Promise(setImmediate);
    assert.equal(pending.length, 2, 'C cannot start while B is pending');
    await blocked;
    pending[1].gate.resolve(respond(200, statusBody({ models_revision: 'B' })));
    await b;
    assert.equal(api.state.verified?.headers.Authorization, 'Bearer key-B');
    assert.equal(api.state.modelsRevision, 'B');
    assert.equal(api.state.authSubmitting, false);
    input.value = 'key-C';
    const c = api.handlers.submitAuth({ preventDefault() {} });
    await new Promise(setImmediate);
    assert.equal(pending.length, 3, 'C may start after B finishes');
    pending[2].gate.resolve(respond(200, statusBody({ models_revision: 'C' })));
    await c;
    assert.equal(api.state.verified?.headers.Authorization, 'Bearer key-C');
    assert.equal(api.state.modelsRevision, 'C');
    for (const request of pending) {
      assert.ok(!/key-[ABC]/.test(request.url), 'candidate keys never appear in URLs');
    }
  });
});

test('auth race: a superseded verification cannot overwrite a later key or its busy operation', async () => {
  const aGate = deferred();
  const writeGate = deferred();
  let statusCalls = 0;
  const view = fakePage({ fetchImpl: (url, init) => {
    if (url.endsWith('/status')) return ++statusCalls === 1 ? aGate.promise : respond(200, statusBody({ models_revision: 'B' }));
    if (url.endsWith('/models')) return writeGate.promise;
    return respond(404, {});
  } });
  const api = view.context.OpenDesignConsole;
  const input = view.document.getElementById('managementKey');
  input.value = 'key-A';
  const a = api.handlers.submitAuth({ preventDefault() {} });
  await new Promise(setImmediate);
  api.handlers.clearAuth();
  input.value = 'key-B';
  await api.handlers.submitAuth({ preventDefault() {} });
  api.handlers.addMapping();
  api.state.mappings.at(-1).id = 'draft';
  api.state.mappings.at(-1).upstream_id = 'up/draft';
  const write = api.handlers.saveModels();
  await new Promise(setImmediate);
  assert.equal(api.state.busy, true);
  aGate.resolve(respond(200, statusBody({ models_revision: 'A' })));
  await a;
  assert.equal(api.state.verified?.headers.Authorization, 'Bearer key-B');
  assert.equal(api.state.modelsRevision, 'B');
  assert.equal(api.state.busy, true, 'old verification cannot release the new write');
  assert.equal(api.state.modelsSaving, true);
  writeGate.resolve(respond(200, { models: [], models_revision: 'B2' }));
  await write;
  assert.equal(api.state.modelsRevision, 'B2');
});

test('auth race: each follow-up status request uses its own generation', async t => {
  for (const phase of ['before revalidation', 'after revalidation']) await t.test(phase, async () => {
    const writeGate = deferred();
    const statusGate = deferred();
    let statusCount = 0;
    const view = fakePage({ fetchImpl: (url, init) => {
      if (url.endsWith('/credentials') && init.method === 'POST') return writeGate.promise;
      if (url.endsWith('/status')) {
        statusCount += 1;
        if (phase === 'before revalidation' && statusCount === 2) return statusGate.promise;
        if (phase === 'after revalidation' && statusCount === 3) return statusGate.promise;
        return respond(200, statusBody({ models_revision: 'list-1' }));
      }
      return respond(404, {});
    } });
    const api = view.context.OpenDesignConsole;
    const login = async () => {
      view.document.getElementById('managementKey').value = managementKey;
      await api.handlers.submitAuth({ preventDefault() {} });
    };
    await login();
    view.document.getElementById('accountKey').value = key;
    const flight = api.handlers.addAccount({ preventDefault() {} });
    await new Promise(setImmediate);
    if (phase === 'before revalidation') {
      writeGate.resolve(respond(200, { id: 'cred-1', label: '主账户' }));
      await new Promise(setImmediate);
      assert.equal(statusCount, 2, 'the follow-up is in flight before revalidation');
      await login();
    } else {
      await login();
      writeGate.resolve(respond(200, { id: 'cred-1', label: '主账户' }));
      await new Promise(setImmediate);
      assert.equal(statusCount, 3, 'the follow-up started after revalidation');
    }
    statusGate.resolve(respond(401, { error: 'status rejected' }));
    await flight;
    assert.equal(api.state.verified === null, phase === 'after revalidation');
    assert.equal(api.state.busy, false);
    if (phase === 'before revalidation') assert.match(view.document.getElementById('authState').textContent, /验证成功/);
    else assert.match(view.document.getElementById('authState').textContent, /401/);
  });
});

test('auth race: a stale fetch cannot unlock a newer request after selection changes', async () => {
  const requests = [];
  const view = fakePage({ fetchImpl: url => {
    if (url.endsWith('/status')) return respond(200, statusBody({ credentials: [
      { id: 'cred-1', label: '主账户' }, { id: 'cred-2', label: '备用账户' },
    ] }));
    const gate = deferred();
    requests.push(gate);
    return gate.promise;
  } });
  const api = view.context.OpenDesignConsole;
  const login = async () => {
    view.document.getElementById('managementKey').value = managementKey;
    await api.handlers.submitAuth({ preventDefault() {} });
  };
  await login();
  api.handlers.selectCredential('cred-1');
  const a = api.handlers.fetchModels();
  await new Promise(setImmediate);
  await login();
  api.handlers.selectCredential('cred-2');
  const b = api.handlers.fetchModels();
  await new Promise(setImmediate);
  assert.equal(requests.length, 2);
  const text = view.document.getElementById('modelState').textContent;
  requests[0].resolve(respond(403, { error: 'old rejection' }));
  await a;
  assert.ok(api.state.verified);
  assert.equal(api.state.busy, true);
  assert.equal(view.document.getElementById('modelState').textContent, text);
  requests[1].resolve(respond(200, { data: [{ id: 'model-b' }] }));
  await b;
  assert.equal(api.state.busy, false);
  assert.equal(api.state.upstreamFor, 'cred-2');
});

test('auth race: an old session write cannot release a newer session write lock', async () => {
  const writes = [];
  const view = fakePage({ fetchImpl: url => {
    if (url.endsWith('/status')) return respond(200, statusBody({ models_revision: 'list-1' }));
    if (url.endsWith('/models')) {
      const gate = deferred();
      writes.push(gate);
      return gate.promise;
    }
    return respond(404, {});
  } });
  const api = view.context.OpenDesignConsole;
  const input = view.document.getElementById('managementKey');
  input.value = 'old-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  const old = api.handlers.saveModels();
  await new Promise(setImmediate);
  input.value = 'new-key';
  await api.handlers.submitAuth({ preventDefault() {} });
  const current = api.handlers.saveModels();
  await new Promise(setImmediate);
  assert.equal(writes.length, 2);
  writes[0].resolve(respond(401, { error: 'old key rejected' }));
  await old;
  assert.equal(api.state.verified?.headers.Authorization, 'Bearer new-key');
  assert.equal(api.state.busy, true);
  assert.equal(api.state.modelsSaving, true);
  assert.equal(view.document.getElementById('saveModels').disabled, true);
  writes[1].resolve(respond(200, { models: [], models_revision: 'new-saved' }));
  await current;
  assert.equal(api.state.busy, false);
  assert.equal(api.state.modelsRevision, 'new-saved');
});

test('auth race: a current rejection cancels an in-flight verification and its owner', async () => {
  const gates = [];
  const view = fakePage({ fetchImpl: url => {
    if (!url.endsWith('/status')) return respond(401, { error: 'expired' });
    if (!gates.length) { gates.push(null); return respond(200, statusBody()); }
    const gate = deferred();
    gates.push(gate);
    return gate.promise;
  } });
  const api = view.context.OpenDesignConsole;
  const input = view.document.getElementById('managementKey');
  input.value = managementKey;
  await api.handlers.submitAuth({ preventDefault() {} });
  input.value = 'key-A';
  const a = api.handlers.submitAuth({ preventDefault() {} });
  await new Promise(setImmediate);
  await api.handlers.loadConfig();
  assert.equal(api.state.verified, null);
  input.value = 'key-B';
  const b = api.handlers.submitAuth({ preventDefault() {} });
  await new Promise(setImmediate);
  gates[1].resolve(respond(200, statusBody({ models_revision: 'A' })));
  await a;
  assert.equal(api.state.authSubmitting, true);
  gates[2].resolve(respond(200, statusBody({ models_revision: 'B' })));
  await b;
  assert.equal(api.state.verified?.headers.Authorization, 'Bearer key-B');
  assert.equal(api.state.modelsRevision, 'B');
});

test('auth race: asynchronous auth resolution carries only a local request generation', async () => {
  const auth = deferred();
  const reply = deferred();
  const view = fakePage({ fetchImpl: () => reply.promise });
  const client = view.context.OpenDesignAPI.createClient({
    base: '/v0/management/open-design', fetchImpl: view.fetch, resolveAuth: () => auth.promise,
  });
  const result = client.status().catch(error => error);
  const resolvedAuth = { headers: { Authorization: `Bearer ${managementKey}` }, authGeneration: 17 };
  auth.resolve(resolvedAuth);
  await new Promise(setImmediate);
  resolvedAuth.authGeneration = 99;
  resolvedAuth.headers.Authorization = 'Bearer changed-after-send';
  reply.resolve(respond(403, { error: 'denied' }));
  const error = await result;
  assert.equal(error.authGeneration, 17);
  assert.equal(error.status, 403);
  assert.equal(view.calls[0].init.headers.Authorization, `Bearer ${managementKey}`);
  assert.ok(!JSON.stringify({ url: view.calls[0].url, ...view.calls[0].init, headers: {} }).includes(managementKey));
  assert.ok(!JSON.stringify(view.calls[0].init).includes('authGeneration'));
  assert.equal(view.calls[0].url.includes(managementKey), false);
});

test('account race: a delayed same-key proof cannot undo an acknowledged save or hide its failed refresh', async t => {
  for (const status of [401, 403, 500]) await t.test(`follow-up ${status}`, async () => {
    const verification = deferred(), refresh = deferred();
    let statusCalls = 0;
    const oldStatus = statusBody({ credentials: [{ id: 'cred-1', label: '原账户' }] });
    const view = fakePage({ fetchImpl: (url, init) => {
      if (url.endsWith('/status')) {
        statusCalls++;
        if (statusCalls === 2) return verification.promise;
        if (statusCalls === 3) return refresh.promise;
        return respond(200, oldStatus);
      }
      if (url.endsWith('/credentials') && init.method === 'POST') return respond(201, { id: 'cred-2', label: '新账户' });
      return respond(404, {});
    } });
    const api = view.context.OpenDesignConsole;
    const login = () => {
      view.document.getElementById('managementKey').value = managementKey;
      return api.handlers.submitAuth({ preventDefault() {} });
    };
    await login();
    const revalidating = login();
    await new Promise(setImmediate);
    view.document.getElementById('accountLabel').value = '新账户';
    view.document.getElementById('accountKey').value = key;
    view.document.getElementById('accountControlKey').value = 'fixture-control-secret';
    const saving = api.handlers.addAccount({ preventDefault() {} });
    await new Promise(setImmediate);
    assert.equal(statusCalls, 3, 'the acknowledged POST has launched its follow-up');
    assert.deepEqual(plain(api.state.status.credentials.map(item => item.id)), ['cred-1', 'cred-2']);
    assert.match(view.document.getElementById('accountState').textContent, /账户已保存/);
    verification.resolve(respond(200, oldStatus));
    await revalidating;
    assert.deepEqual(plain(api.state.status.credentials.map(item => item.id)), ['cred-1', 'cred-2'], 'an older proof cannot undo the saved account');
    assert.match(view.document.getElementById('accountState').textContent, /账户已保存/, 'verification must not erase a completed operation');
    refresh.resolve(respond(status, { error: 'refresh rejected' }));
    await saving;
    assert.ok(api.state.verified, 'an obsolete rejection must not revoke the reverified key');
    const notice = view.document.getElementById('accountState').textContent;
    assert.match(notice, /账户已保存/);
    assert.match(notice, /刷新状态/);
    assert.match(notice, /不要重复添加/);
    assert.deepEqual(plain(api.state.status.credentials.map(item => item.id)), ['cred-1', 'cred-2']);
    assert.equal(api.state.busy, false);
    assert.equal(view.document.getElementById('accountKey').value, '');
    assert.equal(view.document.getElementById('accountControlKey').value, '');
    assert.ok(!JSON.stringify(api.state).includes(key));
    assert.ok(!JSON.stringify(api.state).includes('fixture-control-secret'));
    assert.ok(!view.calls.some(call => [key, managementKey, 'fixture-control-secret'].some(secret => call.url.includes(secret))));
    assert.ok(!notice.includes(key) && !notice.includes(managementKey) && !notice.includes('fixture-control-secret'));
    assert.equal(view.calls.filter(call => call.url.endsWith('/credentials')).length, 1, 'there is no automatic retry');
  });
});

test('account race: an older refresh cannot roll back a newer status or erase a newer business notice', async t => {
  for (const result of [200, 500]) await t.test(`follow-up ${result}`, async () => {
    const refresh = deferred(), quota = deferred();
    let statusCalls = 0;
    const original = { id: 'cred-1', label: '原账户', billing_configured: true, workspace_id: 'ws-1' };
    const view = fakePage({ fetchImpl: (url, init) => {
      if (url.endsWith('/status')) {
        statusCalls++;
        if (statusCalls === 2) return refresh.promise;
        return respond(200, statusBody({ credentials: statusCalls === 1
          ? [original] : [original, { id: 'cred-2', label: '新状态中的账户', disabled: true }] }));
      }
      if (url.includes('/quota?')) return quota.promise;
      if (url.endsWith('/credentials') && init.method === 'POST') return respond(201, { id: 'cred-2', label: '刚保存的账户' });
      return respond(404, {});
    } });
    const api = view.context.OpenDesignConsole;
    const login = () => {
      view.document.getElementById('managementKey').value = managementKey;
      return api.handlers.submitAuth({ preventDefault() {} });
    };
    await login();
    const checkingQuota = api.handlers.refreshQuota(api.state.status.credentials[0]);
    await new Promise(setImmediate);
    view.document.getElementById('accountKey').value = key;
    const saving = api.handlers.addAccount({ preventDefault() {} });
    await new Promise(setImmediate);
    assert.equal(statusCalls, 2);
    await login();
    assert.match(view.document.getElementById('accountState').textContent, /账户已保存/);
    quota.resolve(respond(200, quotaContract()));
    await checkingQuota;
    const newerNotice = view.document.getElementById('accountState').textContent;
    assert.match(newerNotice, /额度已更新/);
    refresh.resolve(respond(result, result === 200
      ? statusBody({ credentials: [original, { id: 'cred-2', label: '旧快照', disabled: false }] })
      : { error: 'old refresh failed' }));
    await saving;
    const updated = api.state.status.credentials.find(item => item.id === 'cred-2');
    assert.equal(updated.label, '新状态中的账户');
    assert.equal(updated.disabled, true);
    const notice = view.document.getElementById('accountState').textContent;
    assert.ok(notice.includes(newerNotice), 'a late refresh must not erase a newer business notice');
    if (result === 500) {
      assert.match(notice, /账户已保存/);
      assert.match(notice, /刷新状态/);
      assert.match(notice, /不要重复添加/);
    }
    assert.match(view.document.getElementById('authState').textContent, /验证成功/);
    assert.ok(api.state.verified);
  });
});

test('account race: acknowledged edit, delete and toggle survive an older proof and stale refresh rejection', async t => {
  for (const action of ['edit', 'delete', 'toggle']) await t.test(action, async () => {
    const verification = deferred(), refresh = deferred();
    let statusCalls = 0;
    const oldStatus = statusBody({ credentials: [{ id: 'cred-1', label: '原账户', disabled: false }] });
    const view = fakePage({ fetchImpl: (url, init) => {
      if (url.endsWith('/status')) {
        statusCalls++;
        return statusCalls === 2 ? verification.promise : statusCalls === 3 ? refresh.promise : respond(200, oldStatus);
      }
      if (url.includes('/credentials')) return respond(200, action === 'edit'
        ? { id: 'cred-1', label: '改名' } : { id: 'cred-1', label: '原账户', disabled: true });
      return respond(404, {});
    } });
    const api = view.context.OpenDesignConsole;
    const login = () => {
      view.document.getElementById('managementKey').value = managementKey;
      return api.handlers.submitAuth({ preventDefault() {} });
    };
    await login();
    const proof = login();
    await new Promise(setImmediate);
    const item = api.state.status.credentials[0];
    let flight;
    if (action === 'edit') {
      api.handlers.editAccount(item);
      view.document.getElementById('accountLabel').value = '改名';
      flight = api.handlers.addAccount({ preventDefault() {} });
    } else if (action === 'delete') flight = api.handlers.deleteAccount(item);
    else flight = api.handlers.toggleAccount(item);
    await new Promise(setImmediate);
    verification.resolve(respond(200, oldStatus));
    await proof;
    refresh.resolve(respond(401, { error: 'old rejection' }));
    await flight;
    assert.ok(api.state.verified);
    if (action === 'edit') assert.equal(api.state.status.credentials[0].label, '改名');
    else if (action === 'delete') assert.equal(api.state.status.credentials.length, 0);
    else assert.equal(api.state.status.credentials[0].disabled, true);
    const notice = view.document.getElementById('accountState').textContent;
    assert.match(notice, action === 'edit' ? /已保存/ : action === 'delete' ? /已删除/ : /已停用/);
    assert.match(notice, /刷新状态/);
    assert.equal(api.state.busy, false);
  });
});

test('status race: newer status results fence older success and errors without hiding current auth rejection', async t => {
  for (const result of [200, 500, 401, 403]) await t.test(String(result), async () => {
    const old = deferred(), newer = deferred();
    let count = 0;
    const view = fakePage({ fetchImpl: url => {
      if (!url.endsWith('/status')) return respond(404, {});
      count++;
      if (count === 2) return old.promise;
      if (count === 3) return newer.promise;
      return respond(200, statusBody());
    } });
    const api = view.context.OpenDesignConsole;
    view.document.getElementById('managementKey').value = managementKey;
    await api.handlers.submitAuth({ preventDefault() {} });
    const first = api.render.loadStatus();
    const second = api.render.loadStatus();
    await new Promise(setImmediate);
    newer.resolve(respond(200, statusBody({ credentials: [{ id: 'fresh', label: '新账户' }] })));
    await second;
    old.resolve(respond(result, result === 200
      ? statusBody({ credentials: [{ id: 'old', label: '旧账户' }] }) : { error: 'old response' }));
    await first;
    if (result === 401 || result === 403) {
      assert.equal(api.state.verified, null, 'current-generation auth errors must still revoke the session');
      assert.match(view.document.getElementById('authState').textContent, new RegExp(String(result)));
    } else {
      assert.ok(api.state.verified);
      assert.equal(api.state.status.credentials[0].id, 'fresh');
      assert.doesNotMatch(view.document.getElementById('authState').textContent, /读取状态失败/);
    }
  });
});

test('reasoning race: current auth rejection is handled even after a status invalidates its result', async t => {
  for (const status of [401, 403]) await t.test(String(status), async () => {
    const reasoning = deferred();
    const view = fakePage({ fetchImpl: url => {
      if (url.endsWith('/models/reasoning')) return reasoning.promise;
      if (url.endsWith('/status')) return respond(200, statusBody({ models_revision: 'list-1' }));
      return respond(404, {});
    } });
    const api = view.context.OpenDesignConsole;
    view.document.getElementById('managementKey').value = managementKey;
    await api.handlers.submitAuth({ preventDefault() {} });
    await api.handlers.addReasoningRule();
    const saving = api.handlers.saveReasoning();
    await new Promise(setImmediate);
    await api.render.loadStatus();
    assert.match(view.document.getElementById('reasoningState').textContent, /结果已失效/);
    reasoning.resolve(respond(status, { error: 'current management rejection' }));
    await saving;
    assert.equal(api.state.verified, null);
    assert.match(view.document.getElementById('authState').textContent, new RegExp(String(status)));
    assert.equal(view.sessionStorage.keys().length, 0);
  });
});

test('auth race: old untagged upstream-model 401 stays ambiguous after same-key revalidation', async () => {
  const f = authRaceFixture();
  await authRaceLogin(f);
  f.api.handlers.selectCredential('cred-1');
  f.hold(url => url.includes('/upstream-models'));
  const flight = f.api.handlers.fetchModels();
  await new Promise(setImmediate);
  await authRaceLogin(f);
  f.pending.resolve(respond(401, { error: 'legacy backend rejection' }));
  await flight;
  assert.ok(f.api.state.verified);
  const notice = f.view.document.getElementById('modelState').textContent;
  assert.match(notice, /401/);
  assert.match(notice, /OpenDesign.*[Kk]ey/);
  assert.doesNotMatch(notice, /管理密钥无效或已过期/);
});

test('account race: current follow-up 401 reports the acknowledged save on the auth screen', async () => {
  let calls = 0;
  const view = fakePage({ fetchImpl: url => {
    if (url.endsWith('/status')) return ++calls === 1 ? respond(200, statusBody()) : respond(401, { error: 'expired' });
    if (url.endsWith('/credentials')) return respond(201, { id: 'cred-2', label: '新账户' });
    return respond(404, {});
  } });
  const api = view.context.OpenDesignConsole;
  view.document.getElementById('managementKey').value = managementKey;
  await api.handlers.submitAuth({ preventDefault() {} });
  view.document.getElementById('accountKey').value = key;
  await api.handlers.addAccount({ preventDefault() {} });
  assert.equal(api.state.verified, null);
  const notice = view.document.getElementById('authState').textContent;
  assert.match(notice, /账户已保存/);
  assert.match(notice, /刷新状态/);
  assert.match(notice, /不要重复添加/);
});

test('api.saveModels keeps the legacy body and only adds a revision when explicitly given one', async () => {
  const calls = [];
  const view = fakePage({
    fetchImpl: async (url, init = {}) => {
      calls.push({ url, init });
      return respond(200, { models: [] });
    },
  });
  const client = view.context.OpenDesignAPI.createClient({
    base: '/v0/management/open-design',
    resolveAuth: () => ({ headers: { Authorization: `Bearer ${managementKey}` } }),
    fetchImpl: view.fetch,
  });
  const models = [{ id: 'od-x', upstream_id: 'upstream/x' }];
  await client.saveModels(models);
  assert.deepEqual(JSON.parse(calls[0].init.body), { models },
    'an old call that only passes the list keeps the exact legacy body and gets no CAS');
  await client.saveModels(models, 'list-rev-7');
  assert.deepEqual(JSON.parse(calls[1].init.body), { models, expected_models_revision: 'list-rev-7' });
  await client.saveModels(models, '');
  assert.deepEqual(JSON.parse(calls[2].init.body), { models },
    'an empty revision does not fabricate a whole-list check');
});

test('an unpinned new row colliding with a same-alias same-upstream row offers a per-row adopt control', async () => {
  const page = modelSavePage();
  await page.api.render.loadStatus();
  page.api.handlers.addMapping();
  const draft = page.api.state.mappings[page.api.state.mappings.length - 1];
  draft.id = 'od-ghost';
  draft.upstream_id = 'open-design/ghost';
  page.api.render.renderMappings();

  // Another tab writes the exact same alias+upstream before this page refreshes.
  page.setModels([...pinnedModels(), { id: 'od-ghost', upstream_id: 'open-design/ghost' }]);
  page.setRevision('list-rev-2');
  await page.api.render.loadStatus();
  assert.equal(page.api.state.mappings.at(-1).id, 'od-ghost', 'the dirty draft survives the refresh');
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 0, 'the same-alias collision blocks the save');
  assert.match(page.view.document.getElementById('modelState').textContent, /同名别名冲突/);

  const rows = page.view.document.getElementById('mappingList').children;
  const conflictRow = rows[rows.length - 1];
  const adopt = Array.from(conflictRow.children)
    .find(node => node.getAttribute('data-action') === 'adopt-server-mapping');
  assert.ok(adopt, 'the unpinned conflict row exposes the promised adopt control');
  await adopt.emit('click');
  assert.match(page.confirmations.at(-1), /未绑定身份/);
  assert.equal(page.api.state.mappings.at(-1).original_id, 'od-ghost');
  assert.equal(page.api.state.mappings.at(-1).original_upstream_id, 'open-design/ghost');
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1, 'the adopted row can be saved');
});

test('a legacy draft judged against a newly advertised revision still offers the per-row adopt control', async () => {
  const page = modelSavePage({ revision: '' });
  await page.api.render.loadStatus();
  page.api.handlers.addMapping();
  const draft = page.api.state.mappings[page.api.state.mappings.length - 1];
  draft.id = 'od-ghost';
  draft.upstream_id = 'open-design/ghost';
  page.api.render.renderMappings();

  // The draft started on a tokenless backend. Another tab adds the same
  // alias+upstream and the backend begins advertising a whole-list revision.
  page.setModels([...pinnedModels(), { id: 'od-ghost', upstream_id: 'open-design/ghost' }]);
  page.setRevision('list-rev-2');
  await page.api.render.loadStatus();
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 0, 'the legacy draft must first confirm the new whole-list token');
  assert.match(page.view.document.getElementById('modelState').textContent, /现在提供整表版本校验/);

  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 0, 'the same-alias collision blocks the second save');
  assert.match(page.view.document.getElementById('modelState').textContent, /同名别名冲突/);
  const rows = page.view.document.getElementById('mappingList').children;
  const conflictRow = rows[rows.length - 1];
  const adopt = Array.from(conflictRow.children)
    .find(node => node.getAttribute('data-action') === 'adopt-server-mapping');
  assert.ok(adopt, 'the gate revision, not the empty draft revision, drives the per-row control');
  await adopt.emit('click');
  assert.equal(page.api.state.mappings.at(-1).original_id, 'od-ghost');
});

test('a legacy draft confirmed against a new revision judges the revision it will send', async () => {
  const page = modelSavePage({ revision: '' });
  await page.api.render.loadStatus();
  // Alias-only rename keeps the row pinned, so the confirmed save carries the
  // original identity and the backend preserves the policy.
  const alias = page.view.document.getElementById('mappingList').children[0].children[0].children[1];
  alias.value = 'od-flash-fast';
  await alias.emit('input');
  assert.equal(page.api.state.modelsDraftRevision, '', 'the draft started before the token existed');

  page.setModels(pinnedModels());
  page.setRevision('list-rev-2');
  await page.api.render.loadStatus();
  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 0, 'the first click only arms the overwrite confirmation');

  await page.api.handlers.saveModels();
  assert.equal(page.posts.length, 1);
  assert.equal(page.posts[0].expected_models_revision, 'list-rev-2',
    'the confirmed save sends the server revision, not the empty draft token');
  assert.equal(page.posts[0].models[0].original_id, 'od-flash');
  assert.ok(!page.confirmations.some(text => text.includes('不会沿用原来的思考强度映射')),
    'a rename the backend can preserve must not warn that the policy is lost');
  assert.ok(page.confirmations.some(text => /覆盖服务器/.test(text)));
  assert.match(page.view.document.getElementById('modelState').textContent, /已保存 2 个模型映射/);
});

// ---- account list density: pause sorting, pagination and search -----------

function activeCredential(id, label = id) {
  return { id, label, disabled: false, billing_configured: true, workspace_id: `ws-${id}` };
}

function pausedCredential(id, label = id, resumeInMs = 3600_000) {
  return {
    id, label, disabled: false, billing_configured: true, workspace_id: `ws-${id}`,
    quota_paused: true, effective_disabled: true,
    quota_pause: {
      reason: 'quota_exhausted', source: 'upstream',
      resume_at: new Date(Date.now() + resumeInMs).toISOString(),
      manual_disabled: false, model_id: 'open-design/flash',
    },
  };
}

test('额度暂停账户自动排到列表末尾并以红色卡片显示，且仍能读取额度', async () => {
  const credentials = [
    activeCredential('active-1', '正常一'),
    pausedCredential('paused-1', '暂停一'),
    activeCredential('active-2', '正常二'),
    pausedCredential('paused-2', '暂停二'),
  ];
  const { view, consoleApi, calls } = quotaPage(async url => {
    if (url.includes('/quota')) {
      const id = new URL(url, 'https://cpa.example').searchParams.get('credential_id');
      return respond(200, quotaContract({ credential_id: id }));
    }
    return respond(404, { error: 'not found' });
  }, credentials);
  const rows = [...view.document.getElementById('accountList').children];
  assert.deepEqual(rows.map(row => row.getAttribute('data-credential-id')),
    ['active-1', 'active-2', 'paused-1', 'paused-2'],
    '暂停账户必须自动移到列表末尾，无需额度查询');
  assert.ok(rows[2].className.includes('paused'), '暂停行使用红色卡片样式');
  assert.equal(rows[2].getAttribute('data-account-paused'), 'true');
  assert.match(rows[2].textContent, /额度耗尽 · 自动暂停/);
  assert.match(rows[2].textContent, /约 .*后复查/);
  assert.equal(view.document.getElementById('accountPausedCount').textContent, '2 个额度暂停');
  assert.equal(view.document.getElementById('accountPausedCount').hidden, false);
  assert.equal(calls.filter(call => call.url.includes('/quota')).length, 0,
    '渲染暂停排序本身不得触发额度查询');

  consoleApi.handlers.activateTab('credentials');
  await new Promise(resolve => setTimeout(resolve, 40));
  const queried = calls.filter(call => call.url.includes('/quota'))
    .map(call => new URL(call.url, 'https://cpa.example').searchParams.get('credential_id'));
  assert.deepEqual(queried, ['active-1', 'active-2', 'paused-1', 'paused-2'],
    '额度查询覆盖包括暂停账户在内的全部已配置账户，正常账户优先');
  const pausedRow = [...view.document.getElementById('accountList').children]
    .find(row => row.getAttribute('data-credential-id') === 'paused-1');
  assert.match(pausedRow.textContent, /\$12\.345/, '暂停账户仍显示钱包余额');
  assert.match(pausedRow.textContent, /剩余 800/, '暂停账户仍显示窗口剩余额度');
});

test('额度读取失败或未配置时，暂停卡片仍给出明确状态', async () => {
  const credentials = [
    { ...pausedCredential('paused-nokey', '无授权'), billing_configured: false },
    pausedCredential('paused-fail', '查询失败'),
  ];
  const { view, consoleApi } = quotaPage(async url => {
    if (url.includes('/quota')) return respond(200, quotaContract({
      state: 'error',
      wallet: { state: 'error', data: null, error: { kind: 'unauthorized', message: 'control key 未获官方接受' } },
      funding: { state: 'not_requested', data: null, error: null },
    }));
    return respond(404, { error: 'not found' });
  }, credentials);

  const rows = () => [...view.document.getElementById('accountList').children];
  const byId = id => rows().find(row => row.getAttribute('data-credential-id') === id);
  assert.match(byId('paused-nokey').textContent, /未配置额度查询/, '未配置授权的暂停账户说明不能查询');
  assert.match(byId('paused-nokey').textContent, /额度耗尽 · 自动暂停/, '缺少额度授权不影响暂停徽标');

  await consoleApi.handlers.refreshQuota({ id: 'paused-fail', label: '查询失败', billing_configured: true, workspace_id: 'ws-1' });
  assert.match(byId('paused-fail').textContent, /查询失败/, '查询失败的暂停账户显示失败而不是空白');
});

test('刷新账户状态能看到后台恢复，不发起上游额度查询', async () => {
  const active = activeCredential('cred-1');
  const { view, consoleApi, calls } = quotaPage(async url => {
    if (url.endsWith('/status')) return respond(200, statusBody({ credentials: [active] }));
    return respond(404, { error: 'not found' });
  }, [pausedCredential('cred-1')]);
  assert.match(quotaRow(view).textContent, /额度耗尽 · 自动暂停/);
  await consoleApi.handlers.refreshAccountStatus();
  assert.doesNotMatch(quotaRow(view).textContent, /额度耗尽 · 自动暂停/);
  assert.match(view.document.getElementById('accountRefreshState').textContent, /账户状态已更新/);
  assert.equal(calls.filter(call => call.url.endsWith('/status')).length, 1);
  assert.equal(calls.filter(call => call.url.includes('/quota')).length, 0);
  assert.match(pageHTML(), /id="refreshAccountStatus"/);
});

test('刷新账户状态失败时保留当前会话和草稿', async () => {
  const { view, consoleApi } = quotaPage(async url => {
    if (url.endsWith('/status')) return respond(500, { error: 'temporary error' });
    return respond(404, { error: 'not found' });
  }, [pausedCredential('cred-1')]);
  consoleApi.state.mappings[0].id = 'unsaved-draft';
  await consoleApi.handlers.refreshAccountStatus();
  assert.ok(consoleApi.state.verified, 'a transient failure must not log out the page');
  assert.equal(consoleApi.state.mappings[0].id, 'unsaved-draft');
  assert.match(quotaRow(view).textContent, /额度耗尽 · 自动暂停/);
  assert.match(view.document.getElementById('accountRefreshState').textContent, /刷新账户状态失败/);
});

test('后台恢复后丢弃旧额度快照，不把旧窗口计入可用总额', async () => {
  const { consoleApi, calls } = quotaPage(async url => {
    if (url.endsWith('/status')) return respond(200, statusBody({ credentials: [activeCredential('cred-1')] }));
    if (url.includes('/quota')) return respond(200, quotaContract());
    return respond(404, { error: 'not found' });
  }, [pausedCredential('cred-1')]);
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '暂停', billing_configured: true, workspace_id: 'ws-1' });
  assert.ok(consoleApi.state.quotas['cred-1']);
  const quotaCalls = calls.filter(call => call.url.includes('/quota')).length;
  await consoleApi.handlers.refreshAccountStatus();
  assert.equal(consoleApi.state.quotas['cred-1'], undefined);
  assert.equal(calls.filter(call => call.url.includes('/quota')).length, quotaCalls,
    'status refresh does not query official quota');
});

test('未看到暂停过程时按新额度版本丢弃旧快照', async () => {
  const initial = { ...activeCredential('cred-1'), billing_revision: 'before-recovery' };
  const recovered = { ...initial, billing_revision: 'after-recovery' };
  const { consoleApi } = quotaPage(async url => {
    if (url.endsWith('/status')) return respond(200, statusBody({ credentials: [recovered] }));
    if (url.includes('/quota')) return respond(200, quotaContract());
    return respond(404, { error: 'not found' });
  }, [initial]);
  await consoleApi.handlers.refreshQuota(initial);
  assert.ok(consoleApi.state.quotas['cred-1']);
  await consoleApi.handlers.refreshAccountStatus();
  assert.equal(consoleApi.state.quotas['cred-1'], undefined);
});

test('刷新状态不打断串行额度查询，后续账户采用最新启停状态', async () => {
  const initial = [
    { ...activeCredential('a'), billing_revision: 'old' },
    activeCredential('b'), activeCredential('c'),
  ];
  const next = [
    { ...initial[0], billing_revision: 'new' }, initial[1], { ...initial[2], disabled: true },
  ];
  const gate = deferred();
  const { consoleApi, calls } = quotaPage(async url => {
    if (url.endsWith('/status')) return respond(200, statusBody({ credentials: next }));
    if (url.includes('/quota')) {
      const id = new URL(url, 'https://cpa.example').searchParams.get('credential_id');
      if (id === 'a') return gate.promise;
      return respond(200, quotaContract({ credential_id: id }));
    }
    return respond(404, { error: 'not found' });
  }, initial);
  consoleApi.handlers.activateTab('credentials');
  await new Promise(resolve => setTimeout(resolve, 10));
  await consoleApi.handlers.refreshAccountStatus();
  const queried = () => calls.filter(call => call.url.includes('/quota'))
    .map(call => new URL(call.url, 'https://cpa.example').searchParams.get('credential_id'));
  assert.deepEqual(queried(), ['a'], 'status refresh must not overlap the pending account');
  gate.resolve(respond(200, quotaContract({ credential_id: 'a' })));
  await new Promise(resolve => setTimeout(resolve, 40));
  assert.deepEqual(queried(), ['a', 'b'], 'continue serially, but skip the newly disabled account');
  assert.equal(consoleApi.state.quotas.a, undefined, 'discard the invalidated in-flight snapshot');
  assert.ok(consoleApi.state.quotas.b);
});

test('查询在途时刷新状态并重新打开标签，新的自动查询仍等待旧请求', async () => {
  const initial = [
    { ...activeCredential('a'), billing_revision: 'old' }, activeCredential('b'),
  ];
  const next = [{ ...initial[0], billing_revision: 'new' }, initial[1]];
  const gate = deferred();
  let firstA = true;
  const { consoleApi, calls } = quotaPage(async url => {
    if (url.endsWith('/status')) return respond(200, statusBody({ credentials: next }));
    if (url.includes('/quota')) {
      const id = new URL(url, 'https://cpa.example').searchParams.get('credential_id');
      if (id === 'a' && firstA) {
        firstA = false;
        return gate.promise;
      }
      return respond(200, quotaContract({ credential_id: id }));
    }
    return respond(404, { error: 'not found' });
  }, initial);
  const queried = () => calls.filter(call => call.url.includes('/quota'))
    .map(call => new URL(call.url, 'https://cpa.example').searchParams.get('credential_id'));
  consoleApi.handlers.activateTab('credentials');
  await new Promise(resolve => setTimeout(resolve, 10));
  await consoleApi.handlers.refreshAccountStatus();
  consoleApi.handlers.activateTab('credentials');
  await new Promise(resolve => setTimeout(resolve, 10));
  assert.deepEqual(queried(), ['a'], 'the new walk must wait for the previous in-flight fetch');
  gate.resolve(respond(200, quotaContract({ credential_id: 'a' })));
  await new Promise(resolve => setTimeout(resolve, 50));
  assert.deepEqual(queried(), ['a', 'a', 'b']);
  assert.ok(consoleApi.state.quotas.a, 'the new revision must be queried after the old one ends');
});

test('窗口摘要标示查询时间、失败时旧数据与已到的重置时间', async () => {
  const quota = quotaContract();
  quota.funding.state = 'stale';
  quota.funding.fetched_at = '2026-01-01T00:00:00Z';
  quota.funding.data.codingPlan.windows[0].resetsAt = '2026-01-02T00:00:00Z';
  const { view, consoleApi } = quotaPage(async url => {
    if (url.includes('/quota')) return respond(200, quota);
    return respond(404, { error: 'not found' });
  }, [pausedCredential('cred-1')]);
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '暂停账户', billing_configured: true, workspace_id: 'ws-1' });
  const summary = quotaRow(view).children[2].children[0].textContent;
  assert.match(summary, /剩余 800/);
  assert.match(summary, /上次数据/);
  assert.match(summary, /查询 .*2026/);
  assert.match(summary, /重置时间已到，请刷新/);
});

test('账户列表按每页 10 个翻页，暂停账户保持在各页末尾', () => {
  const credentials = [pausedCredential('paused-1', '暂停一')];
  for (let i = 0; i < 11; i += 1) credentials.push(activeCredential(`active-${i}`, `正常 ${i}`));
  credentials.push(pausedCredential('paused-2', '暂停二'));
  const { view, consoleApi } = quotaPage(async () => respond(404, { error: 'not found' }), credentials);

  const list = () => view.document.getElementById('accountList');
  const ids = () => [...list().children].map(row => row.getAttribute('data-credential-id'));
  assert.equal(list().children.length, 10, '第一页只显示 10 个账户');
  assert.deepEqual(ids(), credentials.filter(item => !item.quota_paused).slice(0, 10).map(item => item.id));
  assert.match(view.document.getElementById('accountPageInfo').textContent, /第 1 \/ 2 页/);
  assert.equal(view.document.getElementById('accountsPrev').disabled, true);
  assert.equal(view.document.getElementById('accountsNext').disabled, false);

  consoleApi.handlers.nextAccountsPage();
  assert.deepEqual(ids(), ['active-10', 'paused-1', 'paused-2']);
  assert.match(view.document.getElementById('accountPageInfo').textContent, /第 2 \/ 2 页/);
  assert.match(view.document.getElementById('accountPageInfo').textContent, /11–13 \/ 13 个/);
  assert.ok(list().children[1].className.includes('paused'));
  assert.ok(list().children[2].className.includes('paused'));
  assert.equal(view.document.getElementById('accountsPrev').disabled, false);
  assert.equal(view.document.getElementById('accountsNext').disabled, true);

  consoleApi.handlers.previousAccountsPage();
  assert.equal(list().children.length, 10);
  assert.equal(view.document.getElementById('accountPageInfo').textContent.includes('第 1 / 2 页'), true);

  // The pause badge jumps straight to the first page containing a paused row.
  consoleApi.handlers.showPausedAccounts();
  assert.equal(view.document.getElementById('accountPageInfo').textContent.includes('第 2 / 2 页'), true);
  assert.ok(list().children[1].className.includes('paused'));
});

test('账户搜索按名称或 ID 过滤并把分页重置到第一页', () => {
  const credentials = [pausedCredential('paused-1', '额度耗尽账户')];
  for (let i = 0; i < 11; i += 1) credentials.push(activeCredential(`active-${i}`, `正常 ${i}`));
  const { view, consoleApi } = quotaPage(async () => respond(404, { error: 'not found' }), credentials);
  consoleApi.handlers.nextAccountsPage();
  assert.equal(view.document.getElementById('accountPageInfo').textContent.includes('第 2 / 2 页'), true);

  consoleApi.handlers.filterAccounts('额度耗尽');
  const rows = [...view.document.getElementById('accountList').children];
  assert.equal(rows.length, 1);
  assert.equal(rows[0].getAttribute('data-credential-id'), 'paused-1');
  assert.match(view.document.getElementById('accountFilterInfo').textContent, /匹配 1 个账户/);
  assert.match(view.document.getElementById('accountPageInfo').textContent, /第 1 \/ 1 页/);

  consoleApi.handlers.filterAccounts('active-3');
  assert.deepEqual([...view.document.getElementById('accountList').children].map(row => row.getAttribute('data-credential-id')), ['active-3']);

  consoleApi.handlers.filterAccounts('');
  assert.equal(view.document.getElementById('accountList').children.length, 10);
  assert.equal(view.document.getElementById('accountFilterInfo').textContent, '');
});

test('额度详情默认折叠为紧凑摘要，展开状态在倒计时重绘后保留', async () => {
  const { view, consoleApi } = quotaPage(async url => {
    if (url.includes('/quota')) return respond(200, quotaContract());
    return respond(404, { error: 'not found' });
  });
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  const details = () => view.document.getElementById('accountList').children[0].children[2];
  assert.equal(details().tagName, 'DETAILS');
  assert.equal(details().open, false, '详情默认折叠');
  const summary = details().children[0];
  assert.equal(summary.tagName, 'SUMMARY');
  assert.match(summary.textContent, /\$12\.345/, '摘要保留钱包余额');
  assert.match(summary.textContent, /20\.00%/, '摘要保留窗口使用率');
  assert.match(summary.textContent, /剩余 800/, '摘要保留剩余额度');
  assert.match(summary.textContent, /重置于/, '摘要保留重置时间');
  assert.doesNotMatch(summary.textContent, /本次预检未指定模型/, '长说明应在展开的详情里');

  details().open = true;
  await details().emit('toggle');
  consoleApi.render.renderAccounts();
  assert.equal(details().open, true, '本地倒计时重绘不得收起用户展开的详情');
});
