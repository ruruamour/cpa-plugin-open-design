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

function fakePage({ url = 'https://cpa.example/v0/resource/plugins/open-design/console', persistent = {}, session = {}, fetchImpl } = {}) {
  const nodes = new Map();
  const document = {
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
    confirm: () => true,
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
  // Quota display is implemented against /tmp/open-design-quota-contract.md, but
  // pricing, exchange-rate math and success-rate claims are still out of scope.
  assert.ok(!/price|价格|汇率|成功率|\$\s*\d+\s*(per|\/)\s*(1k|1m|million|token)|还能用\s*\d+\s*天/i.test(page),
    'unimplemented pricing/success-rate/\"N days left\" claims must not appear');
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
  consoleApi.state.candidate = null;
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
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.normalizeStatus(statusBody()).credentials)),
    [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: false, workspace_id: '' }],
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(core.normalizeStatus(statusBody({
      credentials: [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
    })).credentials)),
    [{ id: 'cred-1', label: '主账户', disabled: false, billing_configured: true, workspace_id: 'ws-1' }],
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
const quotaButtons = view => Array.from(quotaRow(view).children[2].children).flatMap(node => Array.from(node.children || []))
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

  const refresh = quotaButtons(view).find(node => node.getAttribute('data-action') === 'refresh-quota');
  assert.ok(refresh);
  await refresh.emit('click');
  const quotaCall = calls.find(call => call.url.includes('/quota'));
  assert.match(quotaCall.url, /\/quota\?credential_id=cred-1&refresh=1$/);
  assert.equal(quotaCall.method, 'GET');
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
              resetsAt: '2026-11-03T05:00:00Z',
              forecast: { state: 'estimated', seconds_to_exhaust: 3600, exhausts_at: '2026-11-03T01:00:00Z', basis: 'recent_consumption' },
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
  assert.match(text, /按最近消费速度估算/);
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
  assert.ok(!staleText.includes('按最近消费速度估算'), 'stale snapshots must not keep showing the old prediction');
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
  assert.match(html, /不是推理 Key/);
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

test('a quota route 401/403 never clears management auth or the account list', async () => {
  const { view, consoleApi } = quotaPage(async () => respond(401, { error: { kind: 'unauthorized', message: 'control key rejected' } }));
  await consoleApi.handlers.refreshQuota({ id: 'cred-1', label: '主账户', billing_configured: true, workspace_id: 'ws-1' });
  assert.ok(consoleApi.state.verified, 'management auth is untouched');
  assert.equal(view.document.getElementById('accountsPanel').hidden, false);
  assert.match(view.document.getElementById('accountState').textContent, /管理登录未受影响/);
  assert.ok(!view.document.getElementById('accountState').textContent.includes('管理密钥无效'));
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
  assert.match(estimated.text, /按最近消费速度估算/);
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

  // A control key without a workspace is rejected before the request.
  view.document.getElementById('accountLabel').value = '缺 workspace';
  view.document.getElementById('accountKey').value = 'inference-key-123456';
  view.document.getElementById('accountControlKey').value = 'control-key-123456';
  const before = calls.length;
  await consoleApi.handlers.addAccount({ preventDefault() {} });
  assert.equal(calls.length, before, 'no request when the control key has no workspace');
  assert.match(view.document.getElementById('accountState').textContent, /还需要 Workspace ID/);
});
