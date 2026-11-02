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
    upstreamQuery: '',
    mappings: [],
    savedMappings: [],
    accountEdit: null,
    authCollapsed: false,
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

  function mappingKey(models) {
    return JSON.stringify((Array.isArray(models) ? models : []).map(model => [
      String(model && model.id || '').trim(),
      String(model && model.upstream_id || '').trim(),
    ]));
  }

  function cloneMappings(models) {
    return (Array.isArray(models) ? models : []).map(model => ({ id: model.id, upstream_id: model.upstream_id }));
  }

  // True while the editor holds rows that differ from what the server reports.
  function mappingsDirty() {
    return mappingKey(state.mappings) !== mappingKey(state.savedMappings);
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
    for (const id of ['authSubmit', 'accountSubmit', 'accountCancel', 'fetchModels', 'saveModels', 'addMapping', 'revertMappings', 'modelCredential', 'selectVisible', 'clearVisible']) {
      const node = $(id);
      if (node) node.disabled = state.busy;
    }
    for (const id of ['accountList', 'upstreamList', 'mappingList']) {
      setRowButtonsDisabled($(id), state.busy);
    }
    syncFetchButton();
    syncModelTools();
    // Rebuild account rows on release so disabled accounts keep their
    // 「获取模型」 button disabled (the sweep above cannot know it).
    if (!state.busy) renderAccounts();
  }

  // Row buttons are recreated on every render, so busy transitions also walk the
  // rendered rows instead of relying on a full re-render.
  function setRowButtonsDisabled(container, disabled) {
    if (!container) return;
    const stack = [...(container.children || [])];
    while (stack.length) {
      const node = stack.pop();
      if (node.tagName === 'BUTTON') node.disabled = Boolean(disabled);
      if (node.children) stack.push(...node.children);
    }
  }

  function setDisabled(id, value) {
    const node = $(id);
    if (node) node.disabled = Boolean(value);
  }

  function selectedCredentialItem() {
    const id = state.selectedCredential;
    if (!id) return null;
    return state.status.credentials.find(item => item.id === id) || null;
  }

  function syncFetchButton() {
    const item = selectedCredentialItem();
    setDisabled('fetchModels', state.busy || !state.selectedCredential || Boolean(item && item.disabled));
  }

  function syncModelTools() {
    const hasUpstream = state.upstreamFor !== '' && state.upstream.length > 0;
    setDisabled('selectVisible', state.busy || !hasUpstream);
    setDisabled('clearVisible', state.busy || !hasUpstream);
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
    state.upstreamQuery = '';
    const search = $('upstreamSearch');
    if (search) search.value = '';
    syncModelTools();
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
    resetAccountForm();
    setAuthCollapsed(false);
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

  // Newer backends tag upstream (OpenDesign key) failures with source='upstream'.
  // Only that exact marker counts, so a management 401/403 is never mistaken for
  // a bad account key. Older backends send no source and keep the old meaning.
  function isUpstreamSource(error) {
    return Boolean(error && error.source === 'upstream');
  }

  function isManagementAuthError(error) {
    return Boolean(error) && isAuthStatus(error.status) && !isUpstreamSource(error);
  }

  function upstreamErrorDetail(error) {
    const code = error && typeof error.code === 'string' ? error.code.trim() : '';
    return code ? `（${code}）` : '';
  }

  // Some management failures finish part of the work before reporting an error:
  // the mapping can be persisted while the host refresh fails, and a delete can
  // disable the credential (withdrawing its models) before the unlink fails. The
  // backend messages say so explicitly; keep that fact visible instead of letting
  // the operator retry blindly.
  function partialCompletion(text) {
    const value = String(text || '').toLowerCase();
    if (value.includes('saved but the host model refresh failed')) return 'mapping-persisted';
    if (value.includes('now disabled') || value.includes('models are withdrawn')) return 'credential-disabled';
    return '';
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
      // /status is a management-only route, so a 401/403 here always means the
      // management key, never an upstream account key.
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
    const incoming = Core.normalizeStatus(raw);
    const serverModels = cloneMappings(incoming.models);
    // A status refresh (account add/update, background reload) must not silently
    // discard mapping edits the operator has not saved yet. The saved baseline is
    // always refreshed; the draft is replaced only when it is not dirty.
    const dirty = mappingsDirty();
    state.status = incoming;
    state.status.models = cloneMappings(serverModels);
    state.savedMappings = cloneMappings(serverModels);
    if (!dirty) state.mappings = cloneMappings(serverModels);
    if (state.accountEdit && !incoming.credentials.some(item => item.id === state.accountEdit.id)) {
      resetAccountForm();
      message('accountState', '正在编辑的账户已被删除，已取消编辑。', '');
    }
    if (state.selectedCredential && !incoming.credentials.some(item => item.id === state.selectedCredential)) {
      state.selectedCredential = '';
      invalidateModelRequest();
    }
    renderMeta();
    renderAccounts();
    renderModels();
    if (dirty) message('modelState', '账户信息已刷新；模型映射还有未保存的修改，草稿已保留。', '');
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
      className: options.className || 'button',
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
        const editing = Boolean(state.accountEdit && state.accountEdit.id === item.id);
        const detail = el('div', { className: 'row-main' }, [
          el('span', { className: 'name', text: item.label }),
          el('span', {
            className: `badge ${item.disabled ? 'off' : 'on'}`,
            text: item.disabled ? '已停用' : '已启用',
          }),
          editing ? el('span', { className: 'badge', text: '正在编辑' }) : null,
        ]);
        const actions = el('div', { className: 'row-actions' }, [
          button('获取模型', 'fetch-account', {
            className: 'button compact',
            disabled: item.disabled,
            on: () => selectAccount(item),
          }),
          button('编辑', 'edit-account', {
            className: 'button compact',
            on: () => editAccount(item),
          }),
          button(item.disabled ? '启用' : '停用', 'toggle-account', {
            className: 'button compact',
            on: () => toggleAccount(item),
          }),
          button('删除', 'delete-account', {
            className: 'button compact danger',
            on: () => deleteAccount(item),
          }),
        ]);
        return el('div', { className: 'row', attrs: { 'data-credential-id': item.id } }, [detail, actions]);
      });
      list.replaceChildren(...rows);
    }
    if (state.busy) setRowButtonsDisabled(list, true);
    renderCredentialOptions();
    syncFetchButton();
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

  function visibleUpstream() {
    return Core.filterUpstream(state.upstream, state.upstreamQuery);
  }

  function updateUpstreamCount() {
    const node = $('upstreamCount');
    if (!node) return;
    if (!state.upstreamFor) {
      node.textContent = '0 个上游模型';
      return;
    }
    const visible = visibleUpstream().length;
    const total = state.upstream.length;
    node.textContent = state.upstreamQuery
      ? `显示 ${visible} / ${total} 个上游模型`
      : `${total} 个上游模型`;
  }

  function updateDirtyBadge() {
    const dirty = mappingsDirty();
    const badge = $('mappingDirty');
    if (badge) badge.hidden = !dirty;
    const revert = $('revertMappings');
    if (revert) revert.hidden = !dirty;
    return dirty;
  }

  function renderUpstream() {
    const list = $('upstreamList');
    if (!list) return;
    if (!state.upstreamFor) {
      list.replaceChildren(el('p', { className: 'empty', text: '选择账户后点击「获取模型列表」。已停用账户不能拉取。' }));
      updateUpstreamCount();
      syncModelTools();
      return;
    }
    if (!state.upstream.length) {
      list.replaceChildren(el('p', { className: 'empty', text: 'OpenDesign 没有返回模型。' }));
      updateUpstreamCount();
      syncModelTools();
      return;
    }
    const visible = visibleUpstream();
    if (!visible.length) {
      list.replaceChildren(el('p', { className: 'empty', text: '没有匹配的上游模型，请调整搜索关键词。' }));
      updateUpstreamCount();
      syncModelTools();
      return;
    }
    const rows = visible.map(item => {
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
    updateUpstreamCount();
    syncModelTools();
  }

  function renderMappings() {
    const list = $('mappingList');
    if (!list) return;
    const dirty = updateDirtyBadge();
    if (!state.mappings.length) {
      list.replaceChildren(el('p', { className: 'empty', text: '还没有模型映射。可从上方勾选，或手动添加一行。' }));
      return;
    }
    const rows = state.mappings.map((model, index) => {
      const alias = el('input', {
        className: 'control mono',
        value: model.id,
        placeholder: '对外模型名',
        attrs: { 'aria-label': `第 ${index + 1} 行对外模型名`, autocomplete: 'off' },
      });
      alias.addEventListener('input', () => {
        state.mappings[index].id = alias.value;
        updateDirtyBadge();
      });
      const upstream = el('input', {
        className: 'control mono',
        value: model.upstream_id,
        placeholder: '上游模型 ID',
        attrs: { 'aria-label': `第 ${index + 1} 行上游模型 ID`, autocomplete: 'off' },
      });
      upstream.addEventListener('input', () => {
        state.mappings[index].upstream_id = upstream.value;
        updateDirtyBadge();
      });
      const remove = button('删除', 'remove-mapping', {
        className: 'button compact danger',
        on: () => {
          state.mappings.splice(index, 1);
          renderModels();
        },
      });
      return el('div', { className: 'mapping-row', attrs: { 'data-mapping-index': index } }, [
        el('label', { className: 'field' }, [el('span', { text: '对外别名' }), alias]),
        el('span', { className: 'arrow', text: '→' }),
        el('label', { className: 'field' }, [el('span', { text: '上游模型 ID' }), upstream]),
        remove,
      ]);
    });
    list.replaceChildren(...rows);
    if (state.busy) setRowButtonsDisabled(list, true);
    // Keep checkbox state in sync after edits/removals without re-rendering the
    // upstream list (which would drop the search field's focus).
    syncUpstreamChecks();
    if (dirty) updateDirtyBadge();
  }

  function syncUpstreamChecks() {
    // children is an HTMLCollection in browsers: no Array methods, so always
    // spread it before searching (the old code called children.find here).
    for (const node of Array.from($('upstreamList')?.children || [])) {
      if (node.tagName !== 'LABEL') continue;
      const kids = Array.from(node.children || []);
      const model = kids.find(child => child.tagName === 'SPAN' && child.className === 'mono');
      const box = kids.find(child => child.tagName === 'INPUT');
      if (model && box) box.checked = Core.hasUpstream(state.mappings, model.textContent);
    }
  }

  function selectVisibleUpstream(selected) {
    const ids = visibleUpstream().map(item => item.id);
    if (!ids.length) {
      message('modelState', '当前没有可操作的上游模型。', 'error');
      return;
    }
    state.mappings = selected
      ? Core.addUpstreams(state.mappings, ids)
      : Core.removeUpstreams(state.mappings, ids);
    renderMappings();
    message('modelState', selected
      ? `已勾选当前可见的 ${ids.length} 个上游模型，仍在编辑中，需保存。`
      : `已取消当前可见的 ${ids.length} 个上游模型，仍在编辑中，需保存。`, '');
  }

  function filterUpstream(query) {
    state.upstreamQuery = String(query || '');
    renderUpstream();
  }

  function selectAccount(item) {
    if (state.busy) return;
    if (item.disabled) {
      message('modelState', `账户「${item.label}」已停用，不能拉取模型列表。请先启用。`, 'error');
      return;
    }
    selectCredential(item.id);
    const panel = $('modelsPanel');
    if (panel && typeof panel.scrollIntoView === 'function') panel.scrollIntoView({ behavior: 'smooth', block: 'start' });
    // 「获取模型」 on the account row is a real fetch, not just a selection.
    return fetchModels();
  }

  function accountFormNodes() {
    return {
      title: $('accountFormTitle'),
      hint: $('accountFormHint'),
      submit: $('accountSubmit'),
      cancel: $('accountCancel'),
      label: $('accountLabel'),
      key: $('accountKey'),
    };
  }

  function resetAccountForm() {
    state.accountEdit = null;
    const nodes = accountFormNodes();
    if (nodes.title) nodes.title.textContent = '添加账户';
    if (nodes.hint) nodes.hint.textContent = '填写标签和 API key 后添加。';
    if (nodes.submit) nodes.submit.textContent = '添加账户';
    if (nodes.cancel) nodes.cancel.hidden = true;
    if (nodes.label) nodes.label.value = '';
    if (nodes.key) {
      nodes.key.value = '';
      nodes.key.setAttribute('placeholder', '粘贴 OpenDesign Cloud API key');
    }
    renderAccounts();
  }

  function editAccount(item) {
    if (state.busy) return;
    state.accountEdit = { id: item.id, label: item.label };
    const nodes = accountFormNodes();
    if (nodes.title) nodes.title.textContent = `编辑账户「${item.label}」`;
    if (nodes.hint) nodes.hint.textContent = 'API key 不能回显；留空表示保留原 Key，保存后输入框会清空。';
    if (nodes.submit) nodes.submit.textContent = '保存修改';
    if (nodes.cancel) nodes.cancel.hidden = false;
    if (nodes.label) nodes.label.value = item.label;
    if (nodes.key) {
      nodes.key.value = '';
      nodes.key.setAttribute('placeholder', '留空则保留原 Key');
    }
    message('accountState', `正在编辑「${item.label}」，留空 API key 将保留原 Key。`, '');
    renderAccounts();
    if (nodes.label && typeof nodes.label.focus === 'function') nodes.label.focus();
  }

  function cancelAccountEdit(event) {
    if (event) event.preventDefault();
    if (!state.accountEdit) return;
    const label = state.accountEdit.label;
    resetAccountForm();
    message('accountState', `已取消编辑「${label}」，API key 输入已清空。`, '');
  }

  async function addAccount(event) {
    if (event) event.preventDefault();
    if (state.busy) return;
    const labelInput = $('accountLabel');
    const keyInput = $('accountKey');
    const label = labelInput ? labelInput.value.trim() : '';
    const key = keyInput ? keyInput.value.trim() : '';
    const editing = state.accountEdit;
    if (!editing && !key) {
      message('accountState', '请输入 OpenDesign API key。', 'error');
      return;
    }
    if (editing && !label) {
      message('accountState', '编辑账户时标签不能为空。', 'error');
      return;
    }
    setBusy(true);
    message('accountState', editing ? '正在保存账户修改…' : '正在保存账户…');
    try {
      if (editing) {
        // Reuses the existing POST /credentials contract. An empty key is omitted
        // so the backend keeps the stored key; the API key is never read back.
        const body = { label };
        if (key) body.api_key = key;
        await api().updateCredential(editing.id, body);
        const wasEditing = editing.label;
        resetAccountForm();
        message('accountState', key
          ? `账户「${label}」已保存，API key 已更新并从页面清空。`
          : `账户「${label}」已保存（${wasEditing} → ${label}），原 API key 保留并从页面清空。`);
      } else {
        await api().addCredential(label, key);
        if (keyInput) keyInput.value = '';
        if (labelInput) labelInput.value = '';
        message('accountState', '账户已保存，API key 已从页面清空。');
      }
      await refreshStatus();
      renderAccounts();
    } catch (error) {
      if (isManagementAuthError(error)) {
        showAuth(error.status === 401 ? '401：管理密钥无效或已过期，请重新验证后再保存账户。' : `403：${formatError(error)}`);
      } else {
        message('accountState', `${editing ? '保存修改' : '保存'}失败：${formatError(error)}`, 'error');
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
      if (state.accountEdit && state.accountEdit.id === item.id) {
        resetAccountForm();
        message('accountState', `账户「${item.label}」已删除，编辑已取消。`);
      } else {
        message('accountState', '账户已删除。');
      }
      await refreshStatus();
    } catch (error) {
      if (isManagementAuthError(error)) {
        showAuth(`401/403：${formatError(error)}`);
      } else if (partialCompletion(formatError(error)) === 'credential-disabled') {
        // The backend already disabled the credential before the unlink failed:
        // its models are withdrawn but the record remains. Reflect that state
        // instead of implying the account is untouched.
        if (state.accountEdit && state.accountEdit.id === item.id) resetAccountForm();
        message('accountState', `删除未完成：账户已停用且模型已撤下，但凭据文件未删除（${formatError(error)}）。请刷新状态确认；可重试删除。`, 'error');
        try { await refreshStatus(); } catch { /* Keep the partial-failure message visible. */ }
      } else {
        message('accountState', `删除失败：${formatError(error)}`, 'error');
      }
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
      if (isManagementAuthError(error)) showAuth(`401/403：${formatError(error)}`);
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
    const account = state.status.credentials.find(item => item.id === credentialID);
    if (account && account.disabled) {
      message('modelState', `账户「${account.label}」已停用，不能拉取模型列表。请先启用。`, 'error');
      return;
    }
    const requestToken = ++modelRequestSeq;
    const stillCurrent = () => requestToken === modelRequestSeq && state.selectedCredential === credentialID;
    state.selectedCredential = credentialID;
    activeModelRequest = requestToken;
    setBusy(true);
    message('modelState', `正在通过账户「${account ? account.label : credentialID}」读取 OpenDesign 模型列表…`);
    try {
      const raw = await api().upstreamModels(credentialID);
      // A late response for an earlier selection (or a logged-out page) is
      // discarded: it must not replace the list shown for the current one.
      if (!stillCurrent()) return;
      state.upstream = Core.normalizeUpstreamModels(raw);
      state.upstreamFor = credentialID;
      state.upstreamQuery = '';
      const search = $('upstreamSearch');
      if (search) search.value = '';
      renderUpstream();
      message('modelState', state.upstream.length
        ? `已读取 ${state.upstream.length} 个上游模型。可搜索筛选或全选当前可见项，再设置对外别名并保存映射。`
        : '账户可用，但 OpenDesign 没有返回模型。可稍后重试或手动添加映射。');
    } catch (error) {
      if (!stillCurrent()) return;
      if (isUpstreamSource(error) && isAuthStatus(error.status)) {
        // The management session is fine; the selected OpenDesign account key was
        // rejected upstream. Keep CPA auth and any loaded list, and point at the
        // account instead of asking for the management key again.
        const detail = upstreamErrorDetail(error);
        message('modelState', error.status === 401
          ? `账户「${account ? account.label : credentialID}」的 OpenDesign Key 被上游拒绝${detail}。请编辑该账户替换 Key 后重试；管理认证仍有效。`
          : `上游拒绝读取该账户的模型列表（403）${detail}。请确认该 OpenDesign Key 的权限；管理认证仍有效。`, 'error');
      } else if (error.status === 401) {
        // Older backend without source: keep the previous wording; the operator
        // is told to check both the management key and the account key.
        message('modelState', '401：管理认证失效，或该 OpenDesign API key 无效。请先重新验证管理密钥，再检查账户 key。', 'error');
      } else if (isAuthStatus(error.status)) {
        // Older backend without source, or a genuinely rejected management key.
        showAuth(error.status === 401
          ? '401：管理密钥无效或已过期，请重新输入当前 CPA 入口的管理密钥。'
          : `403：${formatError(error)}`);
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
    if (!models.length) {
      message('modelState', '没有可保存的模型映射。请先勾选上游模型或手动添加一行；如需清空请先删除所有行。', 'error');
      return;
    }
    setBusy(true);
    message('modelState', '正在保存模型映射…');
    try {
      const raw = await api().saveModels(models);
      const saved = raw && Array.isArray(raw.models) ? raw.models : models;
      const normalized = saved.map(item => ({ id: String(item.id || '').trim(), upstream_id: String(item.upstream_id || '').trim() }))
        .filter(item => item.id && item.upstream_id);
      state.status.models = cloneMappings(normalized);
      state.savedMappings = cloneMappings(normalized);
      state.mappings = cloneMappings(normalized);
      renderModels();
      message('modelState', `已保存 ${normalized.length} 个模型映射。`);
    } catch (error) {
      if (isAuthStatus(error.status) && !isUpstreamSource(error)) {
        showAuth(error.status === 401 ? '401：管理密钥无效或已过期，模型映射未保存。' : `403：${formatError(error)}`);
      } else if (isUpstreamSource(error)) {
        message('modelState', `保存失败（上游）：${formatError(error)}`, 'error');
      } else if (partialCompletion(formatError(error)) === 'mapping-persisted') {
        // The mapping is already on disk; only the host catalog refresh failed.
        // The saved baseline moves forward so the draft badge tells the truth.
        state.savedMappings = cloneMappings(models);
        state.status.models = cloneMappings(models);
        renderMappings();
        message('modelState', `模型映射已保存，但宿主刷新模型目录失败：${formatError(error)}。可先刷新状态确认，再重试保存或启用账户触发刷新。`, 'error');
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
    syncFetchButton();
    message('modelState', '');
  }

  function addMapping() {
    state.mappings.push({ id: '', upstream_id: '' });
    renderMappings();
    message('modelState', '已手动添加一行映射，填写对外别名和上游模型 ID 后保存。', '');
  }

  // Explicitly discard the draft; status refreshes never do this on their own.
  function revertMappings() {
    if (state.busy || !mappingsDirty()) return;
    state.mappings = cloneMappings(state.savedMappings);
    renderModels();
    message('modelState', '已还原为最近一次保存的模型映射。', '');
  }

  function setAuthCollapsed(collapsed) {
    state.authCollapsed = Boolean(collapsed);
    const body = $('authBody');
    if (body) body.hidden = state.authCollapsed;
    const toggle = $('authToggle');
    if (toggle) {
      toggle.textContent = state.authCollapsed ? '展开' : '收起';
      toggle.setAttribute('aria-expanded', state.authCollapsed ? 'false' : 'true');
    }
  }

  function toggleAuthPanel() {
    setAuthCollapsed(!state.authCollapsed);
  }

  function bind() {
    const authForm = $('authForm');
    if (authForm) authForm.addEventListener('submit', submitAuth);
    const clear = $('authClear');
    if (clear) clear.addEventListener('click', clearAuth);
    const authToggle = $('authToggle');
    if (authToggle) authToggle.addEventListener('click', toggleAuthPanel);
    const accountForm = $('accountForm');
    if (accountForm) accountForm.addEventListener('submit', addAccount);
    const cancel = $('accountCancel');
    if (cancel) cancel.addEventListener('click', cancelAccountEdit);
    const fetchButton = $('fetchModels');
    if (fetchButton) fetchButton.addEventListener('click', fetchModels);
    const saveButton = $('saveModels');
    if (saveButton) saveButton.addEventListener('click', saveModels);
    const addButton = $('addMapping');
    if (addButton) addButton.addEventListener('click', addMapping);
    const revertButton = $('revertMappings');
    if (revertButton) revertButton.addEventListener('click', revertMappings);
    const select = $('modelCredential');
    if (select) select.addEventListener('change', () => selectCredential(select.value));
    const search = $('upstreamSearch');
    if (search) search.addEventListener('input', () => filterUpstream(search.value));
    const selectVisible = $('selectVisible');
    if (selectVisible) selectVisible.addEventListener('click', () => selectVisibleUpstream(true));
    const clearVisible = $('clearVisible');
    if (clearVisible) clearVisible.addEventListener('click', () => selectVisibleUpstream(false));
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
      message('authState', isManagementAuthError(error)
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
    state.selectedCredential = '';
    resetAccountForm();
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
    setAuthCollapsed(false);
    renderAccounts();
    renderModels();
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
    const ok = await loadStatus({ source: resolved.source });
    // Reclaim vertical space once the reused login worked; the panel stays
    // one click away and reopening never changes authentication behaviour.
    if (ok) setAuthCollapsed(true);
  }

  const consoleAPI = {
    state,
    bootstrap,
    mappingsDirty,
    handlers: {
      submitAuth,
      clearAuth,
      addAccount,
      cancelAccountEdit,
      editAccount,
      deleteAccount,
      toggleAccount,
      selectAccount,
      selectCredential,
      fetchModels,
      saveModels,
      addMapping,
      revertMappings,
      filterUpstream,
      selectVisibleUpstream,
      toggleAuthPanel,
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
      resetAccountForm,
      setAuthCollapsed,
      syncFetchButton,
      syncModelTools,
      updateDirtyBadge,
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
