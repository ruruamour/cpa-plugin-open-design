/* OpenDesign Bridge management console.
 *
 * All user-provided values are rendered with textContent / value (never as HTML).
 * The OpenDesign API key is sent once and then cleared from the page; it is never
 * persisted by this console. Management keys are persisted only by ui/auth.js
 * after a successful verification.
 */
(() => {
  'use strict';

  const Core = globalThis.OpenDesignCore;
  const Auth = globalThis.OpenDesignAuth;
  const API = globalThis.OpenDesignAPI;
  const DEFAULT_BASE_URL = (Core && Core.DEFAULT_BASE_URL) || 'https://amr-link.open-design.ai/v1';
  const BASE_FALLBACK = '/v0/management/open-design';

  function $(id) {
    return document.getElementById(id);
  }

  function managementBase() {
    try {
      const own = Auth && Auth.endpoint ? Auth.endpoint() : null;
      if (own && own.apiPath) return own.apiPath;
    } catch { /* Fall back to the injected meta below. */ }
    const meta = document.querySelector('meta[name="cpa-api-base"]');
    return meta && meta.content ? meta.content : BASE_FALLBACK;
  }

  const state = {
    status: { version: '', base_url: DEFAULT_BASE_URL, credentials: [], models: [] },
    verified: null,
    candidate: null,
    selectedCredential: '',
    upstream: [],
    upstreamFor: '',
    mappings: [],
    busy: false,
  };

  // Model-list requests are generation-checked: a slow response must never
  // overwrite the list for a credential the operator has since switched to.
  let modelRequestSeq = 0;
  let activeModelRequest = 0;

  let client = null;
  function api() {
    if (!client) {
      client = API.createClient({
        base: managementBase(),
        resolveAuth: () => state.candidate || state.verified || { headers: {} },
      });
    }
    return client;
  }

  function hasHeaders(headers) {
    return Boolean(headers && (headers.Authorization || headers['X-Management-Key']));
  }

  function el(tag, options = {}, children = []) {
    const node = document.createElement(tag);
    if (options.className) node.className = options.className;
    if (options.text !== undefined) node.textContent = String(options.text);
    if (options.type) node.type = options.type;
    if (options.value !== undefined) node.value = String(options.value);
    if (options.placeholder !== undefined) node.setAttribute('placeholder', String(options.placeholder));
    if (options.checked) node.checked = true;
    if (options.disabled) node.disabled = true;
    if (options.hidden) node.hidden = true;
    if (options.attrs) {
      for (const [name, value] of Object.entries(options.attrs)) {
        if (value !== undefined && value !== null) node.setAttribute(name, String(value));
      }
    }
    if (typeof options.on === 'function') node.addEventListener('click', options.on);
    else if (options.on) {
      for (const [name, handler] of Object.entries(options.on)) node.addEventListener(name, handler);
    }
    const extras = options.children ? options.children : children;
    if (extras) node.append(...extras.filter(Boolean));
    return node;
  }

  function message(id, text, kind = '') {
    const node = $(id);
    if (!node) return;
    node.textContent = text ? String(text) : '';
    node.className = `state${kind ? ` ${kind}` : ''}`;
    node.hidden = !text;
  }

  function showPanels(connected) {
    for (const id of ['accountsPanel', 'modelsPanel']) {
      const node = $(id);
      if (node) node.hidden = !connected;
    }
  }

  function setBusy(value) {
    state.busy = Boolean(value);
    for (const id of ['authSubmit', 'accountSubmit', 'fetchModels', 'saveModels', 'addMapping', 'modelCredential']) {
      const node = $(id);
      if (node) node.disabled = state.busy;
    }
  }

  // Drops any in-flight model list (releasing its busy slot) and clears the
  // upstream view. Called when the selection changes, credentials change or the
  // operator logs out, so stale results cannot be applied to the new selection.
  function invalidateModelRequest() {
    modelRequestSeq += 1;
    if (activeModelRequest) {
      activeModelRequest = 0;
      setBusy(false);
    }
    state.upstream = [];
    state.upstreamFor = '';
  }

  function authLabel(source) {
    if (source === 'cpa-login') return '已复用 CPA 登录';
    if (source === 'session' || source === 'remembered') return '已使用保存的管理密钥';
    return '已验证';
  }

  function markConnected(source) {
    const badge = $('authBadge');
    if (badge) {
      badge.textContent = authLabel(source);
      badge.className = 'badge good';
    }
    showPanels(true);
  }

  function showAuth(text, kind = 'error') {
    state.verified = null;
    state.candidate = null;
    invalidateModelRequest();
    showPanels(false);
    const badge = $('authBadge');
    if (badge) {
      badge.textContent = '未验证';
      badge.className = 'badge';
    }
    const input = $('managementKey');
    if (input && typeof input.focus === 'function' && text) input.focus();
    message('authState', text, kind);
  }

  function formatError(error) {
    return error && error.message ? error.message : '请求失败';
  }

  function isAuthStatus(status) {
    return status === 401 || status === 403;
  }

  async function loadStatus(options = {}) {
    try {
      const raw = await api().status();
      applyStatus(raw);
      const source = options.source || (state.verified && state.verified.source) || '';
      if (state.verified) state.verified.source = source;
      state.candidate = null;
      markConnected(source);
      message('authState', '');
      return true;
    } catch (error) {
      if (isAuthStatus(error.status)) {
        const rejected = state.verified && state.verified.headers;
        if (rejected && state.verified.source !== 'manual') {
          try { Auth.clearAuth(rejected); } catch { /* Keep the page usable. */ }
        }
        showAuth(error.status === 401
          ? '401：管理密钥无效或已过期，请重新输入当前 CPA 入口的管理密钥。'
          : '403：管理请求被拒绝，请确认管理密钥和来源 IP。');
      } else {
        showAuth(`读取状态失败：${formatError(error)}`);
      }
      return false;
    }
  }

  function applyStatus(raw) {
    state.status = Core.normalizeStatus(raw);
    state.mappings = state.status.models.map(model => ({ ...model }));
    if (state.selectedCredential && !state.status.credentials.some(item => item.id === state.selectedCredential)) {
      state.selectedCredential = '';
      invalidateModelRequest();
    }
    renderMeta();
    renderAccounts();
    renderModels();
  }

  function renderMeta() {
    const version = $('version');
    if (version) version.textContent = state.status.version ? `版本 ${state.status.version}` : '版本 --';
    const base = $('baseUrl');
    if (base) base.textContent = state.status.base_url || DEFAULT_BASE_URL;
  }

  function button(label, action, options = {}) {
    return el('button', {
      text: label,
      className: options.className || '',
      attrs: { type: 'button', 'data-action': action },
      disabled: options.disabled,
      on: options.on,
    });
  }

  function renderAccounts() {
    const list = $('accountList');
    if (!list) return;
    const items = state.status.credentials;
    const count = $('accountCount');
    if (count) count.textContent = `${items.length} 个账户`;
    if (!items.length) {
      list.replaceChildren(el('p', { className: 'empty', text: '还没有账户。填写标签和 API key 后添加。' }));
    } else {
      const rows = items.map(item => {
        const detail = el('div', { className: 'row-main' }, [
          el('span', { className: 'name', text: item.label }),
          el('span', {
            className: `badge ${item.disabled ? 'off' : 'on'}`,
            text: item.disabled ? '已停用' : '已启用',
          }),
        ]);
        const actions = el('div', { className: 'row-actions' }, [
          button('获取模型', 'fetch-account', { on: () => selectAccount(item.id) }),
          button(item.disabled ? '启用' : '停用', 'toggle-account', { on: () => toggleAccount(item) }),
          button('删除', 'delete-account', { className: 'danger', on: () => deleteAccount(item) }),
        ]);
        return el('div', { className: 'row', attrs: { 'data-credential-id': item.id } }, [detail, actions]);
      });
      list.replaceChildren(...rows);
    }
    renderCredentialOptions();
  }

  function renderCredentialOptions() {
    const select = $('modelCredential');
    if (!select) return;
    const options = [el('option', { value: '', text: '请选择账户' })];
    for (const item of state.status.credentials) {
      options.push(el('option', {
        value: item.id,
        text: item.disabled ? `${item.label}（已停用）` : item.label,
        disabled: item.disabled,
      }));
    }
    select.replaceChildren(...options);
    select.value = state.selectedCredential;
  }

  function renderModels() {
    renderCredentialOptions();
    renderUpstream();
    renderMappings();
  }

  function renderUpstream() {
    const list = $('upstreamList');
    if (!list) return;
    if (!state.upstreamFor) {
      list.replaceChildren(el('p', { className: 'empty', text: '选择账户后点击「获取模型列表」。' }));
      return;
    }
    if (!state.upstream.length) {
      list.replaceChildren(el('p', { className: 'empty', text: 'OpenDesign 没有返回模型。' }));
      return;
    }
    const rows = state.upstream.map(item => {
      const box = el('input', { type: 'checkbox', checked: Core.hasUpstream(state.mappings, item.id) });
      box.addEventListener('change', () => {
        state.mappings = box.checked
          ? Core.addUpstream(state.mappings, item.id)
          : Core.removeUpstream(state.mappings, item.id);
        renderMappings();
      });
      const parts = [
        box,
        el('span', { className: 'mono', text: item.id }),
        item.owned_by ? el('span', { className: 'muted small', text: item.owned_by }) : null,
      ];
      return el('label', { className: 'check upstream' }, parts);
    });
    list.replaceChildren(...rows);
  }

  function renderMappings() {
    const list = $('mappingList');
    if (!list) return;
    if (!state.mappings.length) {
      list.replaceChildren(el('p', { className: 'empty', text: '还没有模型映射。可从上方勾选，或手动添加一行。' }));
      return;
    }
    const rows = state.mappings.map((model, index) => {
      const alias = el('input', {
        className: 'control mono',
        value: model.id,
        placeholder: '对外模型名',
        attrs: { 'aria-label': '对外模型名' },
      });
      alias.addEventListener('input', () => { state.mappings[index].id = alias.value; });
      const upstream = el('input', {
        className: 'control mono',
        value: model.upstream_id,
        placeholder: '上游模型 ID',
        attrs: { 'aria-label': '上游模型 ID' },
      });
      upstream.addEventListener('input', () => { state.mappings[index].upstream_id = upstream.value; });
      const remove = button('删除', 'remove-mapping', {
        className: 'danger compact',
        on: () => {
          state.mappings.splice(index, 1);
          renderModels();
        },
      });
      return el('div', { className: 'mapping-row' }, [
        el('label', { className: 'field' }, [el('span', { text: '对外别名' }), alias]),
        el('span', { className: 'arrow', text: '→' }),
        el('label', { className: 'field' }, [el('span', { text: '上游模型 ID' }), upstream]),
        remove,
      ]);
    });
    list.replaceChildren(...rows);
  }

  function selectAccount(id) {
    selectCredential(id);
    const panel = $('modelsPanel');
    if (panel && typeof panel.scrollIntoView === 'function') panel.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  async function addAccount(event) {
    if (event) event.preventDefault();
    if (state.busy) return;
    const labelInput = $('accountLabel');
    const keyInput = $('accountKey');
    const label = labelInput ? labelInput.value.trim() : '';
    const key = keyInput ? keyInput.value.trim() : '';
    if (!key) {
      message('accountState', '请输入 OpenDesign API key。', 'error');
      return;
    }
    setBusy(true);
    message('accountState', '正在保存账户…');
    try {
      await api().addCredential(label, key);
      if (keyInput) keyInput.value = '';
      if (labelInput) labelInput.value = '';
      message('accountState', '账户已保存，API key 已从页面清空。');
      await refreshStatus();
    } catch (error) {
      if (isAuthStatus(error.status)) {
        showAuth(error.status === 401 ? '401：管理密钥无效或已过期，请重新验证后再添加账户。' : `403：${formatError(error)}`);
      } else {
        message('accountState', `保存失败：${formatError(error)}`, 'error');
      }
    } finally {
      setBusy(false);
    }
  }

  async function deleteAccount(item) {
    if (state.busy) return;
    if (typeof confirm === 'function' && !confirm(`删除账户「${item.label}」？此操作会删除 CPA 中保存的 OpenDesign key。`)) return;
    setBusy(true);
    message('accountState', '正在删除账户…');
    try {
      await api().deleteCredential(item.id);
      if (state.selectedCredential === item.id) {
        state.selectedCredential = '';
        invalidateModelRequest();
      }
      message('accountState', '账户已删除。');
      await refreshStatus();
    } catch (error) {
      if (isAuthStatus(error.status)) showAuth(`401/403：${formatError(error)}`);
      else message('accountState', `删除失败：${formatError(error)}`, 'error');
    } finally {
      setBusy(false);
    }
  }

  async function toggleAccount(item) {
    if (state.busy) return;
    setBusy(true);
    message('accountState', item.disabled ? '正在启用账户…' : '正在停用账户…');
    try {
      await api().updateCredential(item.id, { disabled: !item.disabled });
      if (!item.disabled && state.selectedCredential === item.id) {
        state.selectedCredential = '';
        invalidateModelRequest();
      }
      message('accountState', item.disabled ? '账户已启用。' : '账户已停用。');
      await refreshStatus();
    } catch (error) {
      if (isAuthStatus(error.status)) showAuth(`401/403：${formatError(error)}`);
      else message('accountState', `更新失败：${formatError(error)}`, 'error');
    } finally {
      setBusy(false);
    }
  }

  async function refreshStatus() {
    const raw = await api().status();
    applyStatus(raw);
    markConnected((state.verified && state.verified.source) || '');
    return raw;
  }

  async function fetchModels() {
    if (state.busy) return;
    const select = $('modelCredential');
    const credentialID = select ? select.value : state.selectedCredential;
    if (!credentialID) {
      message('modelState', '请先选择一个账户。', 'error');
      return;
    }
    const requestToken = ++modelRequestSeq;
    const stillCurrent = () => requestToken === modelRequestSeq && state.selectedCredential === credentialID;
    state.selectedCredential = credentialID;
    activeModelRequest = requestToken;
    setBusy(true);
    message('modelState', '正在读取 OpenDesign 模型列表…');
    try {
      const raw = await api().upstreamModels(credentialID);
      // A late response for an earlier selection (or a logged-out page) is
      // discarded: it must not replace the list shown for the current one.
      if (!stillCurrent()) return;
      state.upstream = Core.normalizeUpstreamModels(raw);
      state.upstreamFor = credentialID;
      renderUpstream();
      message('modelState', `已读取 ${state.upstream.length} 个上游模型。勾选后设置对外别名，再保存映射。`);
    } catch (error) {
      if (!stillCurrent()) return;
      if (error.status === 401) {
        message('modelState', '401：管理认证失效，或该 OpenDesign API key 无效。请先重新验证管理密钥，再检查账户 key。', 'error');
      } else if (isAuthStatus(error.status)) {
        showAuth(`403：${formatError(error)}`);
      } else {
        message('modelState', `读取模型失败：${formatError(error)}`, 'error');
      }
    } finally {
      // Only the request that owns the busy slot may release it; an invalidated
      // request has already handed the slot to a newer fetch.
      if (activeModelRequest === requestToken) {
        activeModelRequest = 0;
        setBusy(false);
      }
    }
  }

  async function saveModels() {
    if (state.busy) return;
    let models;
    try {
      models = Core.buildModelsPayload(state.mappings);
    } catch (error) {
      message('modelState', `无法保存：${error.message}`, 'error');
      return;
    }
    setBusy(true);
    message('modelState', '正在保存模型映射…');
    try {
      const raw = await api().saveModels(models);
      const saved = raw && Array.isArray(raw.models) ? raw.models : models;
      state.status.models = saved.map(item => ({ id: String(item.id || '').trim(), upstream_id: String(item.upstream_id || '').trim() }))
        .filter(item => item.id && item.upstream_id);
      state.mappings = state.status.models.map(item => ({ ...item }));
      renderMappings();
      message('modelState', `已保存 ${state.status.models.length} 个模型映射。`);
    } catch (error) {
      if (isAuthStatus(error.status)) {
        showAuth(error.status === 401 ? '401：管理密钥无效或已过期，模型映射未保存。' : `403：${formatError(error)}`);
      } else {
        message('modelState', `保存失败：${formatError(error)}`, 'error');
      }
    } finally {
      setBusy(false);
    }
  }

  // Changing the picker always invalidates the in-flight model list.
  function selectCredential(id) {
    state.selectedCredential = id;
    invalidateModelRequest();
    const select = $('modelCredential');
    if (select && select.value !== id) select.value = id;
    renderUpstream();
    message('modelState', '');
  }

  function addMapping() {
    state.mappings.push({ id: '', upstream_id: '' });
    renderMappings();
  }

  function bind() {
    const authForm = $('authForm');
    if (authForm) authForm.addEventListener('submit', submitAuth);
    const clear = $('authClear');
    if (clear) clear.addEventListener('click', clearAuth);
    const accountForm = $('accountForm');
    if (accountForm) accountForm.addEventListener('submit', addAccount);
    const fetchButton = $('fetchModels');
    if (fetchButton) fetchButton.addEventListener('click', fetchModels);
    const saveButton = $('saveModels');
    if (saveButton) saveButton.addEventListener('click', saveModels);
    const addButton = $('addMapping');
    if (addButton) addButton.addEventListener('click', addMapping);
    const select = $('modelCredential');
    if (select) select.addEventListener('change', () => selectCredential(select.value));
  }

  async function submitAuth(event) {
    if (event) event.preventDefault();
    if (state.busy) return;
    const keyInput = $('managementKey');
    const typeSelect = $('authType');
    const rememberInput = $('rememberAuth');
    const key = keyInput ? keyInput.value.trim() : '';
    const type = typeSelect ? typeSelect.value : 'bearer';
    const remember = rememberInput ? rememberInput.checked === true : false;
    if (!key) {
      message('authState', '请输入当前 CPA 入口的管理密钥。', 'error');
      return;
    }
    state.candidate = { headers: Auth.candidateHeaders(key, type) };
    setBusy(true);
    message('authState', '正在验证管理密钥…');
    try {
      const status = await api().status();
      // Persist only after CPA accepted the key.
      Auth.saveAuth(key, type, remember);
      state.verified = { headers: state.candidate.headers, source: 'manual' };
      state.candidate = null;
      if (keyInput) keyInput.value = '';
      applyStatus(status);
      markConnected('manual');
      message('modelState', '');
      message('accountState', '');
      message('authState', '管理密钥验证成功，已开始读取账户和模型。', 'ok');
    } catch (error) {
      state.candidate = null;
      state.verified = null;
      showPanels(false);
      const badge = $('authBadge');
      if (badge) {
        badge.textContent = '未验证';
        badge.className = 'badge';
      }
      message('authState', isAuthStatus(error.status)
        ? `${error.status}：管理密钥未通过验证，请检查后重试。`
        : `验证失败：${formatError(error)}`, 'error');
    } finally {
      setBusy(false);
    }
  }

  function clearAuth() {
    try { Auth.clearAuth(); } catch { /* Ignore blocked storage. */ }
    state.verified = null;
    state.candidate = null;
    invalidateModelRequest();
    const keyInput = $('managementKey');
    if (keyInput) keyInput.value = '';
    const remember = $('rememberAuth');
    if (remember) remember.checked = false;
    message('authState', '已清除本页保存的管理密钥，请重新验证。', 'ok');
    showAuth('已清除本页保存的管理密钥，请重新验证。', 'ok');
  }

  async function bootstrap() {
    renderMeta();
    if (Auth.rememberEnabled && Auth.rememberEnabled()) {
      const remember = $('rememberAuth');
      if (remember) remember.checked = true;
    }
    const resolved = Auth.resolveHeaders();
    if (!hasHeaders(resolved.headers)) {
      showAuth('需要管理密钥才能读取 OpenDesign 账户和模型。可复用同源 CPA 已保存的登录，或在此输入当前入口的管理密钥。', '');
      return;
    }
    state.verified = { headers: resolved.headers, source: resolved.source };
    await loadStatus({ source: resolved.source });
  }

  const consoleAPI = {
    state,
    bootstrap,
    handlers: {
      submitAuth,
      clearAuth,
      addAccount,
      deleteAccount,
      toggleAccount,
      selectAccount,
      selectCredential,
      fetchModels,
      saveModels,
      addMapping,
    },
    render: {
      applyStatus,
      renderMeta,
      renderAccounts,
      renderCredentialOptions,
      renderUpstream,
      renderMappings,
      renderModels,
      showAuth,
      loadStatus,
    },
    managementBase,
  };
  globalThis.OpenDesignConsole = consoleAPI;

  if (!globalThis.__OPEN_DESIGN_NO_BOOTSTRAP__) {
    bind();
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bootstrap);
    else bootstrap();
  }
})();
