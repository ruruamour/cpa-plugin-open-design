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
    // Quota snapshots are keyed by credential id. They are only fetched when the
    // operator asks (no polling, no automatic per-account upstream request).
    quotas: {},
    quotaBusy: {},
    quotaGeneration: 0,
    quotaRevisions: {},
    busy: false,
  };

  // Logout invalidates every request; editing one credential invalidates only
  // its own requests so concurrent refreshes for other accounts can finish.
  function invalidateQuota(credentialID) {
    if (credentialID === undefined) {
      state.quotaGeneration += 1;
      state.quotaRevisions = {};
      state.quotas = {};
      state.quotaBusy = {};
      return;
    }
    state.quotaRevisions[credentialID] = (state.quotaRevisions[credentialID] || 0) + 1;
    delete state.quotas[credentialID];
    delete state.quotaBusy[credentialID];
  }

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
    invalidateQuota();
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

  // Quota snapshots belong to one credential configuration. When the billing
  // fields change elsewhere (or an account disappears), the old snapshot and any
  // in-flight refresh must be dropped instead of being shown for the new setup.
  function billingSignature(item) {
    return `${item.billing_configured === true ? '1' : '0'}|${item.workspace_id || ''}`;
  }

  function invalidateChangedQuotas(credentials) {
    const previous = state.billingSignatures || {};
    const next = {};
    for (const item of credentials) {
      next[item.id] = billingSignature(item);
      if (Object.prototype.hasOwnProperty.call(previous, item.id) && previous[item.id] !== next[item.id]) {
        invalidateQuota(item.id);
      }
    }
    for (const id of Object.keys(previous)) {
      if (!Object.prototype.hasOwnProperty.call(next, id)) invalidateQuota(id);
    }
    state.billingSignatures = next;
  }

  function applyStatus(raw) {
    const incoming = Core.normalizeStatus(raw);
    invalidateChangedQuotas(incoming.credentials);
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

  // Update countdown/forecast expiry locally; this timer never issues a request.
  let quotaViewTimer = null;
  function scheduleQuotaView() {
    if (quotaViewTimer) clearTimeout(quotaViewTimer);
    quotaViewTimer = null;
    const now = Date.now();
    let next = Infinity;
    for (const quota of Object.values(state.quotas)) {
      const times = [quota.next_refresh_at];
      for (const window of quota.funding?.data?.codingPlan?.windows || []) {
        times.push(window.resetsAt, window.forecast?.exhausts_at);
      }
      for (const value of times) {
        const at = Date.parse(value);
        if (at > now) next = Math.min(next, at);
      }
    }
    if (Number.isFinite(next)) {
      quotaViewTimer = setTimeout(() => {
        quotaViewTimer = null;
        renderAccounts();
      }, Math.min(next - now + 25, 2147483647));
      quotaViewTimer.unref?.();
    }
  }

  function renderAccounts() {
    scheduleQuotaView();
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
          item.billing_configured ? el('span', { className: 'badge', text: '额度已配置' }) : null,
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
        return el('div', { className: 'row', attrs: { 'data-credential-id': item.id } }, [
          detail,
          actions,
          renderQuota(item),
        ]);
      });
      list.replaceChildren(...rows);
    }
    if (state.busy) setRowButtonsDisabled(list, true);
    // The clear-billing checkbox only makes sense while editing an account that
    // actually has billing credentials stored.
    const clearRow = $('accountClearBillingRow');
    if (clearRow) clearRow.hidden = !(state.accountEdit && state.accountEdit.billing_configured);
    renderCredentialOptions();
    syncFetchButton();
  }

  const QUOTA_STATE_TEXT = {
    not_configured: '未配置额度查询',
    needs_refresh: '尚未查询',
    not_requested: '未查询（缺 Workspace ID）',
    available: '已更新',
    partial: '部分可用',
    stale: '数据可能过时',
    error: '查询失败',
  };

  function quotaStateBadge(stateName) {
    const className = stateName === 'available' ? 'badge good'
      : (stateName === 'stale' || stateName === 'partial' ? 'badge warn'
        : (stateName === 'error' ? 'badge off' : 'badge'));
    return el('span', { className, text: QUOTA_STATE_TEXT[stateName] || '未知状态' });
  }

  function quotaErrorText(prefix, error) {
    if (!error) return prefix;
    const messages = {
      unauthorized: '额度授权无效或已过期，请编辑账户更换 controlKey',
      forbidden: '该账户或工作区没有额度查询权限',
      rate_limited: '上游限制查询频率，请稍后重试',
      timeout: '上游查询超时，请稍后重试',
      malformed: '上游响应或额度配置不符合官方格式',
      network: '暂时无法连接 OpenDesign',
      upstream: 'OpenDesign 额度服务暂时不可用',
      stopped: '额度查询已停止',
    };
    const kind = error.kind ? `（${error.kind}）` : '';
    const detail = messages[error.kind] || error.message;
    return `${prefix}${kind}${detail ? `：${detail}` : ''}`;
  }

  function quotaWalletCard(item, quota) {
    const wallet = quota.wallet;
    const configured = quota.billing_configured !== false && state.status.credentials.some(c => c.id === item.id && c.billing_configured);
    const card = el('div', { className: 'quota-card', attrs: { 'data-quota-part': 'wallet' } }, [
      el('h4', { text: '钱包余额（USD）' }),
      quotaStateBadge(wallet.state || (configured ? 'needs_refresh' : 'not_configured')),
    ]);
    if (!configured) {
      card.append(el('p', { className: 'quota-note', text: '未配置官方 controlKey，不查询余额。可在编辑账户中填写。' }));
      return card;
    }
    if (wallet.state === 'available' && wallet.data && wallet.data.balanceUsd) {
      card.append(el('div', { className: 'quota-value', text: Core.formatUSD(wallet.data.balanceUsd) }));
      card.append(el('p', { className: 'quota-meta', text: `查询时间：${Core.formatTime(wallet.fetched_at) || '未知'}` }));
      card.append(el('p', { className: 'quota-meta', text: wallet.data.updatedAt
        ? `上游更新时间：${Core.formatTime(wallet.data.updatedAt)}`
        : '上游未提供更新时间' }));
      return card;
    }
    if (wallet.state === 'error') {
      card.append(el('p', { className: 'quota-note', text: quotaErrorText('首次查询失败', wallet.error) }));
      card.append(el('p', { className: 'quota-hint', text: '可稍后重试；未显示余额不代表余额为 0。' }));
      return card;
    }
    if (wallet.state === 'stale') {
      if (wallet.data && wallet.data.balanceUsd) {
        card.append(el('div', { className: 'quota-value', text: Core.formatUSD(wallet.data.balanceUsd) }));
        card.append(el('p', { className: 'quota-meta', text: `上次成功查询：${Core.formatTime(wallet.fetched_at) || '未知'}（当前可能已变化）` }));
      }
      card.append(el('p', { className: 'quota-note', text: quotaErrorText('刷新失败，保留上次数据', wallet.error) }));
      return card;
    }
    card.append(el('p', { className: 'quota-note', text: wallet.state === 'needs_refresh'
      ? '尚未查询额度，点「刷新额度」读取。'
      : '当前没有可显示的余额数据。' }));
    return card;
  }

  function quotaWindowBlock(window) {
    const block = el('div', { className: 'quota-window' });
    const title = window.durationSeconds ? `窗口 ${Core.formatDuration(window.durationSeconds)}` : '额度窗口';
    block.append(el('div', { className: 'quota-window-head' }, [
      el('span', { className: 'quota-window-title', text: title }),
      el('span', { className: 'quota-meta', text: ({ anchored_recurring: '固定窗口', activity_triggered: '活动触发窗口' })[window.resetMode] || '重置方式未知' }),
    ]));
    const used = Core.formatCredits(window.usedCredits);
    const limit = Core.formatCredits(window.limitCredits);
    const remaining = Core.formatCredits(window.remainingCredits);
    if (used && limit) {
      block.append(el('p', { className: 'quota-meta', text: `已用 ${used} / ${limit} credits` }));
    } else {
      block.append(el('p', { className: 'quota-meta', text: '已用/上限：未知' }));
    }
    if (remaining) {
      block.append(el('p', { className: 'quota-value', text: `${remaining} credits` }));
      block.append(el('p', { className: 'quota-meta', text: '剩余 credits' }));
    } else {
      block.append(el('p', { className: 'quota-note', text: '剩余 credits：未知' }));
    }
    const meter = Core.creditsPercent(window.usedCredits, window.limitCredits);
    if (meter) {
      const bar = el('div', { className: `meter${meter.basisPoints >= 8000 ? ' warn' : ''}` }, [
        el('span', { attrs: { style: `width:${meter.text}` }, text: '' }),
      ]);
      block.append(bar);
      block.append(el('p', { className: 'quota-meta', text: `已用 ${meter.text}` }));
    }
    if (window.resetsAt) {
      block.append(el('p', { className: 'quota-meta', text: `重置于 ${Core.formatTime(window.resetsAt)}` }));
    } else {
      block.append(el('p', { className: 'quota-meta', text: '重置时间：未知' }));
    }
    const resetAt = Date.parse(window.resetsAt);
    const exhaustsAt = Date.parse(window.forecast?.exhausts_at);
    const expired = resetAt <= Date.now() || exhaustsAt <= Date.now();
    const forecast = Core.forecastView(expired ? { state: 'expired' } : window.forecast);
    if (forecast.text) block.append(el('p', { className: `quota-forecast${forecast.tone === 'warn' ? ' warn' : ''}`, text: forecast.text }));
    return block;
  }

  function quotaFundingCard(item, quota) {
    const funding = quota.funding;
    const hasWorkspace = Boolean(quota.workspace_id);
    const card = el('div', { className: 'quota-card', attrs: { 'data-quota-part': 'funding' } }, [
      el('h4', { text: '套餐用量' }),
      quotaStateBadge(funding.state || (hasWorkspace ? 'needs_refresh' : 'not_requested')),
    ]);
    if (!hasWorkspace) {
      card.append(el('p', { className: 'quota-note', text: '未填写 Workspace ID，无法查询套餐窗口；这不影响推理。' }));
      return card;
    }
    if (funding.state === 'error') {
      card.append(el('p', { className: 'quota-note', text: quotaErrorText('首次查询失败', funding.error) }));
      return card;
    }
    if (funding.state === 'stale') {
      card.append(el('p', { className: 'quota-note', text: quotaErrorText('刷新失败，保留上次数据', funding.error) }));
      card.append(el('p', { className: 'quota-meta', text: `上次成功查询：${Core.formatTime(funding.fetched_at) || '未知'}` }));
      // A stale component must not keep presenting the old prediction.
      if (funding.data && funding.data.codingPlan) {
        const plan = funding.data.codingPlan;
        card.append(el('p', { className: 'quota-meta', text: `套餐：${plan.tier || '未知'}` }));
        for (const window of plan.windows) {
          const block = quotaWindowBlock({ ...window, forecast: { state: '' } });
          card.append(block);
        }
      }
      return card;
    }
    if (funding.state === 'needs_refresh') {
      card.append(el('p', { className: 'quota-note', text: '尚未查询套餐用量，点「刷新额度」读取。' }));
      return card;
    }
    const plan = funding.data && funding.data.codingPlan ? funding.data.codingPlan : null;
    if (!plan || !plan.windows.length) {
      card.append(el('p', { className: 'quota-note', text: '上游没有返回套餐窗口数据。' }));
      return card;
    }
    const fundingName = ({ coding_plan: '套餐额度', wallet: '钱包', gateway: '网关' })[funding.data.funding] || '未知';
    card.append(el('p', { className: 'quota-meta', text: `套餐：${plan.tier || '未知'} · 资金方式：${fundingName}` }));
    if (funding.data.fundingCheck) {
      const checkName = ({ available: '可用', insufficient: '不足', unknown: '未知' })[funding.data.fundingCheck] || '未知';
      card.append(el('p', { className: 'quota-meta', text: `资金检查：${checkName}` }));
    }
    for (const window of plan.windows) card.append(quotaWindowBlock(window));
    card.append(el('p', { className: 'quota-note', text: `窗口数据查询时间：${Core.formatTime(funding.fetched_at) || '未知'}` }));
    return card;
  }

  function quotaRefreshHint(quota) {
    const next = quota && quota.next_refresh_at ? Date.parse(quota.next_refresh_at) : NaN;
    if (Number.isNaN(next)) return '额度查询不会自动轮询；只有点「刷新额度」才会向上游请求。';
    const seconds = Math.round((next - Date.now()) / 1000);
    if (seconds <= 0) return '可以刷新额度。';
    return `两次上游查询至少间隔 60 秒，约 ${seconds} 秒后可再次刷新。`;
  }

  function renderQuota(item) {
    const quota = state.quotas[item.id];
    const busy = Boolean(state.quotaBusy[item.id]);
    const head = el('div', { className: 'quota-head' }, [
      el('span', { className: 'quota-window-title', text: '额度' }),
      quota ? quotaStateBadge(quota.state) : el('span', { className: 'badge', text: item.billing_configured ? '未查询' : '未配置' }),
      el('span', { className: 'hint', text: quota ? quotaRefreshHint(quota) : '额度查询可选，不会自动请求。' }),
    ]);
    const refresh = button(busy ? '刷新中…' : '刷新额度', 'refresh-quota', {
      className: 'button compact',
      disabled: busy || !item.billing_configured,
      on: () => refreshQuota(item),
    });
    head.append(refresh);
    const section = el('div', { className: 'quota', attrs: { 'data-quota-id': item.id } }, [head]);
    if (quota && quota.state === 'stale') {
      section.append(el('p', { className: 'quota-note', text: '当前显示的是上次成功查询的数据；上游暂时不可用时不显示旧预测。' }));
    }
    const cards = el('div', { className: 'quota-grid' });
    if (!item.billing_configured) {
      cards.append(el('div', { className: 'quota-card' }, [
        el('h4', { text: '额度查询未配置' }),
        el('p', { className: 'quota-note', text: '点「编辑」填写官方 controlKey（可选 Workspace ID）后可查询余额和套餐用量。未配置不影响模型推理。' }),
      ]));
    } else if (!quota) {
      cards.append(el('div', { className: 'quota-card' }, [
        el('h4', { text: '尚未查询' }),
        el('p', { className: 'quota-note', text: '点「刷新额度」读取。本页不会自动向上游查询。' }),
      ]));
    } else {
      cards.append(quotaWalletCard(item, quota));
      cards.append(quotaFundingCard(item, quota));
    }
    section.append(cards);
    if (quota && quota.error_note) section.append(el('p', { className: 'quota-note', text: quota.error_note }));
    return section;
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
      billing: $('billingFields'),
      controlKey: $('accountControlKey'),
      workspaceID: $('accountWorkspaceID'),
      clearBilling: $('accountClearBilling'),
    };
  }

  function readBillingInput() {
    const nodes = accountFormNodes();
    return {
      controlKey: nodes.controlKey ? nodes.controlKey.value.trim() : '',
      workspaceID: nodes.workspaceID ? nodes.workspaceID.value.trim() : '',
      clear: Boolean(nodes.clearBilling && nodes.clearBilling.checked),
    };
  }

  // The control key is written once and never read back into the page.
  function clearBillingInputs() {
    const nodes = accountFormNodes();
    if (nodes.controlKey) {
      nodes.controlKey.value = '';
      nodes.controlKey.setAttribute('placeholder', '粘贴 OpenDesign 官方 control key');
    }
    if (nodes.workspaceID) nodes.workspaceID.value = '';
    if (nodes.clearBilling) nodes.clearBilling.checked = false;
    if (nodes.billing) nodes.billing.open = false;
  }

  function resetAccountForm() {
    state.accountEdit = null;
    const nodes = accountFormNodes();
    const clearRow = $('accountClearBillingRow');
    if (clearRow) clearRow.hidden = true;
    if (nodes.title) nodes.title.textContent = '添加账户';
    if (nodes.hint) nodes.hint.textContent = '填写标签和 API key 后添加。';
    if (nodes.submit) nodes.submit.textContent = '添加账户';
    if (nodes.cancel) nodes.cancel.hidden = true;
    if (nodes.label) nodes.label.value = '';
    if (nodes.key) {
      nodes.key.value = '';
      nodes.key.setAttribute('placeholder', '粘贴 OpenDesign Cloud API key');
    }
    clearBillingInputs();
    renderAccounts();
  }

  function editAccount(item) {
    if (state.busy) return;
    state.accountEdit = { id: item.id, label: item.label, billing_configured: item.billing_configured === true };
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
    clearBillingInputs();
    if (nodes.controlKey) {
      nodes.controlKey.setAttribute('placeholder', item.billing_configured
        ? '已配置：留空则保留原 controlKey'
        : '粘贴 OpenDesign 官方 control key');
    }
    if (nodes.workspaceID) nodes.workspaceID.value = item.workspace_id || '';
    const clearRow = $('accountClearBillingRow');
    if (clearRow) clearRow.hidden = !item.billing_configured;
    if (nodes.billing) nodes.billing.open = Boolean(item.billing_configured || item.workspace_id);
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
    const billing = readBillingInput();
    if (billing.clear && billing.controlKey) {
      message('accountState', '已勾选「清除额度授权」，不会再保存新的 controlKey。取消勾选后再保存。', 'error');
      return;
    }
    setBusy(true);
    message('accountState', editing ? '正在保存账户修改…' : '正在保存账户…');
    try {
      if (editing) {
        // Reuses the existing POST /credentials contract. Empty values are omitted
        // so the backend keeps what is stored; keys are never read back.
        const body = { label };
        if (key) body.api_key = key;
        if (billing.clear) body.clear_billing = true;
        else {
          if (billing.controlKey) body.control_key = billing.controlKey;
          if (billing.workspaceID) body.workspace_id = billing.workspaceID;
        }
        await api().updateCredential(editing.id, body);
        const wasEditing = editing.label;
        invalidateQuota(editing.id);
        resetAccountForm();
        const billingNote = billing.clear
          ? '已清除额度授权，推理 Key 保留。'
          : (billing.controlKey || billing.workspaceID ? '额度查询配置已保存。' : '');
        message('accountState', `${key
          ? `账户「${label}」已保存，API key 已更新并从页面清空。`
          : `账户「${label}」已保存（${wasEditing} → ${label}），原 API key 保留并从页面清空。`}${billingNote}`);
      } else {
        const extra = {};
        if (billing.controlKey) extra.control_key = billing.controlKey;
        if (billing.workspaceID) extra.workspace_id = billing.workspaceID;
        await api().addCredential(label, key, extra);
        if (keyInput) keyInput.value = '';
        if (labelInput) labelInput.value = '';
        resetAccountForm();
        message('accountState', billing.controlKey
          ? '账户已保存，API key 与 controlKey 都已从页面清空。点「刷新额度」才会查询额度。'
          : '账户已保存，API key 已从页面清空。额度查询配置可随后通过「编辑」添加。');
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
      invalidateQuota(item.id);
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

  // Explicit refresh only. A cached GET without refresh=1 never reaches upstream,
  // so opening/refreshing the page cannot spend quota calls.
  async function refreshQuota(item) {
    if (!item || !item.billing_configured) {
      message('accountState', '该账户还没有配置官方 controlKey，无法查询额度。点「编辑」添加后再试。', 'error');
      return;
    }
    if (state.quotaBusy[item.id]) return;
    const generation = state.quotaGeneration;
    const revision = state.quotaRevisions[item.id] || 0;
    const stillCurrent = () => generation === state.quotaGeneration && revision === (state.quotaRevisions[item.id] || 0);
    state.quotaBusy[item.id] = true;
    renderAccounts();
    try {
      const raw = await api().quota(item.id, { refresh: true });
      if (!stillCurrent()) return;
      const quota = Core.normalizeQuota(raw);
      if (quota.credential_id !== item.id) throw new Error('额度响应与当前账户不一致，请重新查询。');
      if (quota.state === 'needs_refresh') {
        // Throttled or skipped: keep any previous snapshot, just explain it.
        const previous = state.quotas[item.id];
        if (previous) previous.next_refresh_at = quota.next_refresh_at;
        else state.quotas[item.id] = quota;
        message('accountState', `额度刷新已节流：${quotaRefreshHint(quota)}`, '');
      } else {
        state.quotas[item.id] = quota;
        message('accountState', quota.state === 'error'
          ? `账户「${item.label}」额度查询失败：${quotaErrorText('未取得数据', quota.error)}；管理登录未受影响。`
          : `账户「${item.label}」额度已更新（${QUOTA_STATE_TEXT[quota.state] || quota.state}）。`, quota.state === 'error' ? 'error' : '');
      }
      renderAccounts();
    } catch (error) {
      if (!stillCurrent()) return;
      if (isManagementAuthError(error)) {
        showAuth(error.status === 401
          ? '401：管理密钥无效或已过期，请重新验证后再查询额度。'
          : '403：管理请求被拒绝，请确认管理密钥和来源 IP。');
      } else {
        // A transport failure leaves the last snapshot uncertain too. Preserve
        // its timestamp, but withdraw any forecast until a successful refresh.
        const previous = state.quotas[item.id];
        if (previous) {
          previous.state = 'stale';
          for (const part of [previous.wallet, previous.funding]) {
            if (part && part.data) part.state = 'stale';
          }
        }
        message('accountState', `账户「${item.label}」额度查询失败：${formatError(error)}。请稍后重试。`, 'error');
      }
    } finally {
      if (stillCurrent()) {
        delete state.quotaBusy[item.id];
        renderAccounts();
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
    if (!models.length && typeof confirm === 'function' && !confirm('清空所有模型映射？保存后将从 CPA 撤回本插件的模型。')) return;
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
      setAuthCollapsed(true);
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
    invalidateQuota();
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
    resetAccountForm();
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
      refreshQuota,
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
