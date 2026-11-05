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
    authSubmitting: false,
    selectedCredential: '',
    upstream: [],
    upstreamFor: '',
    upstreamQuery: '',
    mappings: [],
    savedMappings: [],
    // Whole-list CAS for the model editor. Newer backends advertise a
    // models_revision token; the first edit pins it as modelsDraftRevision so a
    // status refresh or another tab cannot move the base under the draft.
    modelsRevision: '',
    modelsDraftRevision: '',
    modelsDraftActive: false,
    modelsConflict: false,
    // The token the server had when the conflict was raised, and the token a
    // later status read supplied. A retry is only allowed once seen === live
    // revision, so a 409 can never spin on the same stale token.
    modelsConflictRevision: '',
    modelsConflictSeenRevision: '',
    // True while a POST /models write is in flight. A status answer landing in
    // that window must not replace the rows the response is about to confirm.
    modelsSaving: false,
    // Models exactly as the server describes them, including each saved
    // thinking-strength policy. Kept apart from the editor rows above, which
    // carry only id and upstream_id and drive the dirty badge.
    serverModels: [],
    reasoningModelKey: '',
    // key -> { enabled, rules, baseRevision, conflict }. baseRevision is the
    // revision the draft was first edited against; it is not moved by a status
    // refresh, so a save can never silently overwrite another tab's change.
    reasoningDrafts: new Map(),
    reasoningSaving: false,
    // Bumped by every logout/status refresh and by a model-list save. A reasoning
    // save captures it and discards its own late response once it no longer
    // matches, so a stale answer can neither revive the draft nor overwrite a
    // newer list.
    reasoningGeneration: 0,
    // Bumped whenever the server model list is replaced from a response. A
    // status answer requested before a newer list was applied is stale for the
    // model side and must not roll its reasoning revisions back either.
    serverModelsGeneration: 0,
    // Bumped whenever the management identity changes (logout, key switch,
    // rejected auth). Responses captured under an older session are discarded
    // so a delayed status/refresh cannot repaint the new session.
    sessionGeneration: 0,
    accountEdit: null,
    authCollapsed: false,
    // Quota snapshots are keyed by credential id. They are only fetched when the
    // operator asks (no polling, no automatic per-account upstream request).
    quotas: {},
    quotaBusy: {},
    quotaGeneration: 0,
    quotaRevisions: {},
    // Set while the credentials tab walks the accounts one by one. It is cleared
    // by a generation bump, so a logout or a credential edit stops the walk.
    quotaAutoGeneration: 0,
    quotaAutoRunning: false,
    // Model coverage is a separate cache. It carries only official preflight
    // signals and never shares quota snapshots or quota busy state.
    modelCoverage: {},
    modelCoverageBusy: {},
    modelCoverageGeneration: 0,
    modelCoverageRevisions: {},
    busy: false,
    // Optional routes are opt-in: only an explicit operator action fills these.
    // Switching tabs never triggers a request.
    activeTab: 'overview',
    logQuery: { offset: 0, limit: 50, status: '', model: '', credentialID: '', attempted: '' },
    requestLogs: [],
    logPage: { loaded: false, total: 0, offset: 0, limit: 50, has_more: false, next_offset: null, retention: null },
    requestStats: null,
    config: null,
    runtime: null,
    errorRules: null,
    modelTest: null,
    modelTestGeneration: 0,
    optionalBusy: {},
    optionalGeneration: 0,
    themePreference: 'system',
  };

  function modelCoverageKey(credentialID, modelID) {
    return `${String(credentialID)}\u0000${String(modelID)}`;
  }

  function invalidateModelCoverageRequests() {
    state.modelCoverageGeneration += 1;
    state.modelCoverageBusy = {};
  }

  function invalidateModelCoverage(credentialID) {
    if (credentialID === undefined) {
      state.modelCoverageGeneration += 1;
      state.modelCoverageRevisions = {};
      state.modelCoverage = {};
      state.modelCoverageBusy = {};
      return;
    }
    state.modelCoverageRevisions[credentialID] = (state.modelCoverageRevisions[credentialID] || 0) + 1;
    const prefix = `${String(credentialID)}\u0000`;
    for (const key of Object.keys(state.modelCoverage)) {
      if (key.startsWith(prefix)) delete state.modelCoverage[key];
    }
    for (const key of Object.keys(state.modelCoverageBusy)) {
      if (key.startsWith(prefix)) delete state.modelCoverageBusy[key];
    }
  }

  // Logout invalidates every request; editing one credential invalidates only
  // its own quota and model-coverage requests so other accounts can finish.
  function invalidateQuota(credentialID) {
    // Any invalidation also cancels an in-flight walk: a logout clears every
    // snapshot, and editing one account must not continue with stale rows.
    state.quotaAutoGeneration += 1;
    state.quotaAutoRunning = false;
    if (credentialID === undefined) {
      state.quotaGeneration += 1;
      state.quotaRevisions = {};
      state.quotas = {};
      state.quotaBusy = {};
      invalidateModelCoverage();
      return;
    }
    state.quotaRevisions[credentialID] = (state.quotaRevisions[credentialID] || 0) + 1;
    delete state.quotas[credentialID];
    delete state.quotaBusy[credentialID];
    invalidateModelCoverage(credentialID);
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

  // Editor rows may carry the identity of the server row they were read from.
  // Both fields travel with the row so an alias-only rename can still be linked
  // to its stored thinking-strength policy; a row created in the console never
  // has them. They are never invented here: only editorRowsFromServer attaches
  // them, and only from an actual server snapshot.
  function cloneMappings(models) {
    return (Array.isArray(models) ? models : []).map(model => {
      const row = { id: model.id, upstream_id: model.upstream_id };
      const originalID = String(model && model.original_id || '').trim();
      const originalUpstreamID = String(model && model.original_upstream_id || '').trim();
      if (originalID && originalUpstreamID) {
        row.original_id = originalID;
        row.original_upstream_id = originalUpstreamID;
      }
      return row;
    });
  }

  // The editor's view of one server snapshot: every row is pinned to the exact
  // id/upstream pair it was read as, which is what the backend resolves an
  // original identity against. A manual row or an upstream checkbox item is
  // created through Core.addUpstream/addMapping and therefore stays unpinned.
  function editorRowsFromServer(models) {
    const rows = [];
    for (const model of Array.isArray(models) ? models : []) {
      const id = String(model && model.id || '').trim();
      const upstreamID = String(model && model.upstream_id || '').trim();
      if (!id || !upstreamID) continue;
      rows.push({ id, upstream_id: upstreamID, original_id: id, original_upstream_id: upstreamID });
    }
    return rows;
  }

  // A pinned identity is only meaningful against the snapshot named by the
  // whole-list token that is about to be sent. When an explicit overwrite moves
  // the draft onto a newer snapshot, re-resolve every pin there: keep it when
  // that exact row still exists (so a rename still keeps its policy) and drop
  // it when the row is gone, instead of posting an identity the backend would
  // reject on every retry.
  function rebaseMappingOriginals(models) {
    const snapshot = Array.isArray(state.serverModels) ? state.serverModels : [];
    return (Array.isArray(models) ? models : []).map(model => {
      const row = { id: model.id, upstream_id: model.upstream_id };
      const originalID = String(model && model.original_id || '').trim();
      const originalUpstreamID = String(model && model.original_upstream_id || '').trim();
      if (!originalID || !originalUpstreamID) return row;
      const stillPresent = snapshot.some(item =>
        String(item && item.id || '').trim() === originalID &&
        String(item && item.upstream_id || '').trim() === originalUpstreamID);
      if (stillPresent) {
        row.original_id = originalID;
        row.original_upstream_id = originalUpstreamID;
      }
      return row;
    });
  }

  // Rows whose stored thinking-strength policy is about to be dropped. An
  // alias-only rename on a backend with a whole-list token keeps the policy (the
  // original identity is matched by upstream too), so it is not reported there.
  // A real upstream change always drops it: the backend clears the old mapping
  // by default and this console never opts in silently. On an old backend there
  // is no original identity to send, so even a rename cannot be linked to its
  // stored row and the policy is lost as well. A row whose original identity is
  // no longer in the snapshot also loses it, because the backend refuses to
  // resolve an identity it cannot see.
  function mappingsLosingReasoning(models = state.mappings, revision = modelsSaveRevision()) {
    const snapshot = Array.isArray(state.serverModels) ? state.serverModels : [];
    const lost = [];
    for (const row of Array.isArray(models) ? models : []) {
      const originalID = String(row && row.original_id || '').trim();
      const originalUpstreamID = String(row && row.original_upstream_id || '').trim();
      const alias = String(row && row.id || '').trim();
      const upstreamID = String(row && row.upstream_id || '').trim();
      if (!originalID || !originalUpstreamID || !alias || !upstreamID) continue;
      const original = snapshot.find(item =>
        String(item && item.id || '').trim() === originalID &&
        String(item && item.upstream_id || '').trim() === originalUpstreamID);
      if (!original) {
        // The pinned row disappeared from the server snapshot this save is based
        // on, so the identity cannot be matched at all.
        lost.push(`${originalID} → ${alias}（原行已不存在）`);
        continue;
      }
      if (!original.reasoning_mapping) continue;
      if (upstreamID !== originalUpstreamID) {
        lost.push(`${originalID} → ${upstreamID}（上游已改）`);
      } else if (alias !== originalID && !String(revision || '').trim()) {
        lost.push(`${originalID} → ${alias}（当前后端不支持按原行关联）`);
      }
    }
    return lost;
  }

  // Alias-only renames whose policy the server actually kept, verified against
  // the echoed list. Used only for a success note; never inferred from intent.
  function renamedRowsKeepingReasoning(submitted, models) {
    const byKey = new Map((Array.isArray(models) ? models : []).map(model => [reasoningModelKey(model), model]));
    const kept = [];
    for (const row of Array.isArray(submitted) ? submitted : []) {
      const originalID = String(row && row.original_id || '').trim();
      const originalUpstreamID = String(row && row.original_upstream_id || '').trim();
      const alias = String(row && row.id || '').trim();
      const upstreamID = String(row && row.upstream_id || '').trim();
      if (!originalID || !originalUpstreamID || !alias || !upstreamID) continue;
      if (alias === originalID || upstreamID !== originalUpstreamID) continue;
      const saved = byKey.get(reasoningModelKey({ id: alias, upstream_id: upstreamID }));
      if (saved && saved.reasoning_mapping) kept.push(`${originalID} → ${alias}`);
    }
    return kept;
  }

  // The backend rejects a whole-table save whose rows cannot be told apart:
  // two rows claiming the same original row, or a row whose alias equals
  // another row's original alias. That is exactly what an alias swap (a ↔ b)
  // produces. Refusing locally keeps the draft editable instead of arming a
  // retry gate the server can never accept.
  function mappingIdentityConflict(models = state.mappings) {
    const rows = Array.isArray(models) ? models : [];
    const claimed = new Map();
    for (const row of rows) {
      const originalID = String(row && row.original_id || '').trim();
      const originalUpstreamID = String(row && row.original_upstream_id || '').trim();
      if (!originalID || !originalUpstreamID) continue;
      const key = `${originalID}\u0000${originalUpstreamID}`;
      if (claimed.has(key)) return true;
      claimed.set(key, row);
    }
    const originalIDs = new Map();
    for (const [key, row] of claimed) originalIDs.set(key.split('\u0000')[0], row);
    for (const row of rows) {
      const alias = String(row && row.id || '').trim();
      const owner = originalIDs.get(alias);
      if (owner && owner !== row) return true;
    }
    return false;
  }

  // The first edit pins the server's whole-list revision so a later status
  // refresh cannot move it under the operator. A conflict stays armed until a
  // save succeeds or the draft is explicitly reverted, even if the rows are
  // edited back to their old values.
  function beginMappingsDraft() {
    if (state.modelsDraftActive) return;
    state.modelsDraftActive = true;
    state.modelsDraftRevision = state.modelsRevision;
    state.modelsConflict = false;
    state.modelsConflictRevision = '';
    state.modelsConflictSeenRevision = '';
  }

  function resetMappingsDraft(revision = state.modelsRevision) {
    state.modelsDraftActive = false;
    state.modelsDraftRevision = revision || '';
    state.modelsConflict = false;
    state.modelsConflictRevision = '';
    state.modelsConflictSeenRevision = '';
  }

  function modelsSaveRevision() {
    return state.modelsDraftActive ? state.modelsDraftRevision : state.modelsRevision;
  }

  // Called after every mapping edit. Reverting the rows to the saved baseline
  // releases the draft (an old backend then accepts a fresh status list again),
  // but a pending conflict stays armed: content equality must never skip the
  // explicit overwrite confirmation.
  function noteMappingsEdited() {
    if (!state.modelsDraftActive) beginMappingsDraft();
    if (!state.modelsConflict && !mappingsDirty()) resetMappingsDraft();
  }

  // Compare management identities by the key they carry, never by writing the
  // key anywhere. Re-validating the same key keeps the current session and its
  // unsaved drafts; only a different key starts a fresh session.
  function authKeyOf(headers) {
    if (!headers || typeof headers !== 'object') return '';
    if (headers.Authorization) return String(headers.Authorization).replace(/^Bearer\s+/i, '').trim();
    if (headers['X-Management-Key']) return String(headers['X-Management-Key']).trim();
    return '';
  }

  function sameAuthIdentity(left, right) {
    const previous = authKeyOf(left);
    return previous !== '' && previous === authKeyOf(right);
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
    for (const id of ['accountsPanel', 'modelsPanel', 'reasoningPanel']) {
      const node = $(id);
      if (node) node.hidden = !connected;
    }
    for (const id of ['credentialsLocked', 'modelsLocked']) {
      const node = $(id);
      if (node) node.hidden = Boolean(connected);
    }
    renderOverview();
  }

  function setBusy(value) {
    state.busy = Boolean(value);
    // The auth form is deliberately not disabled by the shared busy flag: a
    // 401 from a model write or a logout must always let the operator re-verify.
    for (const id of ['accountSubmit', 'accountCancel', 'fetchModels', 'saveModels', 'addMapping', 'revertMappings', 'modelCredential', 'selectVisible', 'clearVisible']) {
      const node = $(id);
      if (node) node.disabled = state.busy;
    }
    setDisabled('authSubmit', state.authSubmitting);
    for (const id of ['accountList']) {
      setRowButtonsDisabled($(id), state.busy);
    }
    // Only a model-list save owns these rows: other busy operations (fetch,
    // account edits) must not freeze edits the operator is making in parallel.
    for (const id of ['upstreamList', 'mappingList']) {
      setRowControlsDisabled($(id), state.modelsSaving);
    }
    // Reasoning rows stay locked while a reasoning save is in flight: releasing
    // an unrelated busy flag must not re-enable their delete buttons and let an
    // edit disappear into the pending response.
    setRowButtonsDisabled($('reasoningRows'), state.busy || state.reasoningSaving);
    syncFetchButton();
    syncModelTools();
    syncReasoningControls();
    // Rebuild account rows on release so disabled accounts keep their
    // 「获取模型」 button disabled (the sweep above cannot know it).
    if (!state.busy) renderAccounts();
  }

  // Row controls are recreated on every render, so busy transitions also walk the
  // rendered rows instead of relying on a full re-render.
  function setRowButtonsDisabled(container, disabled) {
    setRowControlsDisabled(container, disabled, ['BUTTON']);
  }

  // During a save the mapping inputs and upstream checkboxes must be locked too:
  // letting the operator keep editing rows the response will replace would lose
  // those edits when the POST succeeds.
  function setRowControlsDisabled(container, disabled, tags = ['BUTTON', 'INPUT', 'SELECT', 'TEXTAREA']) {
    if (!container) return;
    const wanted = new Set(tags);
    const stack = [...(container.children || [])];
    while (stack.length) {
      const node = stack.pop();
      if (wanted.has(node.tagName)) node.disabled = Boolean(disabled);
      if (node.children) stack.push(...node.children);
    }
  }

  function setDisabled(id, value) {
    const node = $(id);
    if (node) node.disabled = Boolean(value);
  }

  // --- tabs ----------------------------------------------------------------
  //
  // Switching a tab only toggles hidden/aria-selected. It never calls fetch, so
  // opening the console or browsing sections cannot hit the optional routes.

  const TABS = [
    { name: 'overview', button: 'tabOverview', panel: 'overviewPanel' },
    { name: 'credentials', button: 'tabCredentials', panel: 'credentialsPanel' },
    { name: 'models', button: 'tabModels', panel: 'modelsTab' },
    { name: 'logs', button: 'tabLogs', panel: 'logsPanel' },
    { name: 'config', button: 'tabConfig', panel: 'configPanel' },
  ];

  function tabEntry(name) {
    const wanted = String(name || '');
    return TABS.find(item => item.name === wanted) || TABS[0];
  }

  function activateTab(name, options = {}) {
    const target = tabEntry(name);
    state.activeTab = target.name;
    for (const item of TABS) {
      const selected = item.name === target.name;
      const buttonNode = $(item.button);
      const panelNode = $(item.panel);
      if (buttonNode) {
        buttonNode.setAttribute('aria-selected', selected ? 'true' : 'false');
        buttonNode.setAttribute('tabindex', selected ? '0' : '-1');
      }
      if (panelNode) panelNode.hidden = !selected;
    }
    if (options.focus) {
      const active = $(target.button);
      if (active && typeof active.focus === 'function') active.focus();
    }
    // Leaving the tab cancels the walk before anything else can run, so a slow
    // response cannot start the next account after the operator moved on.
    if (target.name !== 'credentials') cancelQuotaAutoLoad();
    if (target.name === 'credentials') void autoLoadQuotaForAccounts();
    return target.name;
  }

  function tabFromEvent(event) {
    const target = event && event.currentTarget;
    return target && target.getAttribute ? target.getAttribute('data-tab') : '';
  }

  function handleTabKey(event) {
    if (!event || !event.key) return;
    const index = TABS.findIndex(item => item.name === state.activeTab);
    let next = -1;
    if (event.key === 'ArrowRight' || event.key === 'ArrowDown') next = (index + 1) % TABS.length;
    else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') next = (index - 1 + TABS.length) % TABS.length;
    else if (event.key === 'Home') next = 0;
    else if (event.key === 'End') next = TABS.length - 1;
    if (next < 0) return;
    if (typeof event.preventDefault === 'function') event.preventDefault();
    activateTab(TABS[next].name, { focus: true });
  }

  function renderFacts(container, facts, emptyText = '') {
    if (!container) return;
    if (!facts.length) {
      container.replaceChildren(el('p', { className: 'empty', text: emptyText || '没有可显示的字段。' }));
      return;
    }
    container.replaceChildren(...facts.map(fact => el('div', { className: 'fact' }, [
      el('span', { className: 'fact-label', text: fact.label }),
      el('span', { className: 'fact-value', text: fact.value }),
    ])));
  }

  // --- theme ---------------------------------------------------------------
  //
  // The page defaults to the system colour scheme; an explicit, accessible
  // toggle can pin light/dark without touching authentication or data state.

  const THEME_STORAGE_KEY = 'open-design-theme';
  let themeMedia = null;

  function themeSystemDark() {
    try {
      return typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches === true;
    } catch { return false; }
  }

  function readStoredTheme() {
    try { return localStorage.getItem(THEME_STORAGE_KEY); } catch { return null; }
  }

  function persistTheme(preference) {
    try {
      if (preference === 'system') localStorage.removeItem(THEME_STORAGE_KEY);
      else localStorage.setItem(THEME_STORAGE_KEY, preference);
    } catch { /* Storage can be blocked; the in-memory preference still applies. */ }
  }

  function renderThemeToggle() {
    const button = $('themeToggle');
    if (!button) return;
    const preference = Core.normalizeThemePreference(state.themePreference);
    const resolved = Core.resolvedTheme(preference, themeSystemDark());
    const label = Core.themePreferenceLabel(preference, resolved);
    button.textContent = `主题：${label}`;
    button.setAttribute('aria-label', `切换颜色主题，当前：${label}`);
  }

  function applyThemePreference(value, options = {}) {
    state.themePreference = Core.normalizeThemePreference(value);
    const resolved = Core.resolvedTheme(state.themePreference, themeSystemDark());
    const root = document.documentElement;
    if (root && root.setAttribute) {
      root.setAttribute('data-theme', state.themePreference);
      root.setAttribute('data-theme-resolved', resolved);
    }
    if (options.persist) persistTheme(state.themePreference);
    renderThemeToggle();
    return state.themePreference;
  }

  function cycleThemePreference() {
    return applyThemePreference(Core.nextThemePreference(state.themePreference), { persist: true });
  }

  function initTheme() {
    if (!themeMedia && typeof matchMedia === 'function') {
      try {
        themeMedia = matchMedia('(prefers-color-scheme: dark)');
        const changed = () => {
          if (state.themePreference === 'system') applyThemePreference('system');
        };
        if (themeMedia.addEventListener) themeMedia.addEventListener('change', changed);
        else if (themeMedia.addListener) themeMedia.addListener(changed);
      } catch { /* The CSS media query still follows the system. */ }
    }
    applyThemePreference(readStoredTheme() || 'system', { persist: false });
  }

  function retentionScopeText(retention) {
    if (!retention) return '';
    const range = retention.oldest && retention.newest
      ? `${Core.formatTime(retention.oldest)} ~ ${Core.formatTime(retention.newest)}`
      : '';
    const window = retention.retention_days !== null ? `保留 ${retention.retention_days} 天` : '保留天数未知';
    const cap = retention.max_entries !== null ? `最多 ${retention.max_entries} 条` : '最多条数未知';
    const parts = [`统计范围：${window}，${cap}`];
    if (range) parts.push(`数据时间 ${range}`);
    parts.push(`当前 ${retention.entries} 条`);
    return parts.join(' · ');
  }

  function overviewFacts() {
    const credentials = Array.isArray(state.status.credentials) ? state.status.credentials : [];
    const enabled = credentials.filter(item => !item.disabled).length;
    const models = Array.isArray(state.status.models) ? state.status.models : [];
    const connected = Boolean(state.verified);
    return [
      { label: '管理认证', value: connected ? '已验证' : '未验证' },
      { label: '账户', value: connected ? `${credentials.length} 个（${enabled} 个启用）` : '--' },
      { label: '模型映射', value: connected ? `${models.length} 个` : '--' },
      { label: '版本', value: state.status.version || '--' },
      { label: '上游地址', value: state.status.base_url || DEFAULT_BASE_URL },
    ];
  }

  function renderOverview() {
    const connected = Boolean(state.verified);
    const badge = $('overviewBadge');
    if (badge) {
      badge.textContent = connected ? '已验证' : '未验证';
      badge.className = connected ? 'badge good' : 'badge';
    }
    renderFacts($('overviewFacts'), overviewFacts());
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
    invalidateModelCoverageRequests();
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

  function resetManagementData() {
    state.optionalGeneration += 1;
    // A new management identity owns no previous drafts or CAS tokens, and every
    // request captured under the old identity (including quota walks and their
    // 401s) must be invalidated instead of logging the new session out.
    state.sessionGeneration += 1;
    invalidateQuota();
    // An upstream-model fetch from the replaced session must not keep a slot or
    // a selection in the new one.
    invalidateModelRequest();
    state.optionalBusy = {};
    syncOptionalBusyControls();
    state.status = { version: '', base_url: DEFAULT_BASE_URL, credentials: [], models: [] };
    state.selectedCredential = '';
    state.upstream = [];
    state.upstreamFor = '';
    state.upstreamQuery = '';
    state.mappings = [];
    state.savedMappings = [];
    state.modelsRevision = '';
    state.modelsDraftRevision = '';
    state.modelsDraftActive = false;
    state.modelsConflict = false;
    state.modelsConflictRevision = '';
    state.modelsConflictSeenRevision = '';
    state.modelsSaving = false;
    state.serverModels = [];
    state.reasoningModelKey = '';
    state.reasoningDrafts.clear();
    state.reasoningSaving = false;
    state.reasoningGeneration += 1;
    state.serverModelsGeneration += 1;
    state.requestLogs = [];
    state.logQuery = { offset: 0, limit: 50, status: '', model: '', credentialID: '', attempted: '' };
    state.logPage = { loaded: false, total: 0, offset: 0, limit: 50, has_more: false, next_offset: null, retention: null };
    state.requestStats = null;
    state.config = null;
    state.runtime = null;
    state.errorRules = null;
    errorRulesView.loaded = false;
    errorRulesView.dirty = false;
    errorRulesView.generation += 1;
    state.modelTest = null;
    state.modelTestGeneration += 1;
    const status = $('logsFilterStatus');
    if (status) status.value = '';
    const attempted = $('logsFilterAttempted');
    if (attempted) attempted.value = '';
    const model = $('logsFilterModel');
    if (model) model.value = '';
    const credential = $('logsFilterCredential');
    if (credential) credential.value = '';
    const limit = $('logsFilterLimit');
    if (limit) limit.value = '50';
    renderAccounts();
    renderModels();
    renderOverview();
    renderRequestLogs();
    renderRequestStats();
    renderConfig();
    renderRuntime();
    renderErrorRules();
    renderModelTest();
    // Release the busy lock owned by the replaced session and re-enable every
    // control (notably the auth form) without waiting for a stale async finally.
    setBusy(false);
    message('logsState', '');
    message('configState', '');
    message('modelTestState', '');
  }

  function clearRejectedManagementAuth() {
    const rejected = state.verified && state.verified.headers;
    if (!rejected) return;
    try { Auth.clearAuth(rejected); } catch { /* Keep the page usable. */ }
  }

  function showAuth(text, kind = 'error') {
    state.verified = null;
    state.candidate = null;
    invalidateModelRequest();
    invalidateQuota();
    resetManagementData();
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

  function optionalAuthFailure(error) {
    if (!isManagementAuthError(error)) return false;
    clearRejectedManagementAuth();
    showAuth(error.status === 401
      ? '401：管理密钥无效或已过期，请重新验证后再试。'
      : '403：管理请求被拒绝，请确认管理密钥和来源 IP。');
    return true;
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
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    const serverModelsGeneration = state.serverModelsGeneration;
    try {
      const raw = await api().status();
      if (generation !== state.optionalGeneration || sessionGeneration !== state.sessionGeneration) return false;
      applyStatus(raw, { serverModelsGeneration });
      const source = options.source || (state.verified && state.verified.source) || '';
      if (state.verified) state.verified.source = source;
      state.candidate = null;
      markConnected(source);
      message('authState', '');
      return true;
    } catch (error) {
      if (generation !== state.optionalGeneration || sessionGeneration !== state.sessionGeneration) return false;
      // /status is a management-only route, so a 401/403 here always means the
      // management key, never an upstream account key.
      if (isAuthStatus(error.status)) {
        clearRejectedManagementAuth();
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
    return `${item.billing_configured === true ? '1' : '0'}|${item.workspace_id || ''}|${item.billing_revision || ''}`;
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

  function applyStatus(raw, options = {}) {
    const incoming = Core.normalizeStatus(raw);
    // A status answer may have been requested before a newer model-list write
    // completed. Its credentials are still fresh enough to apply, but the older
    // model snapshot must not replace the newer list (or the rows a save is
    // about to confirm), its reasoning revisions, or the draft pinned to them.
    const modelsAreCurrent = options.serverModelsGeneration === undefined ||
      options.serverModelsGeneration === state.serverModelsGeneration;
    const modelsWritable = modelsAreCurrent && !state.modelsSaving;
    const modelTestWasBusy = optionalBusy('modelTest');
    const reasoningWasSaving = state.reasoningSaving;
    state.modelTestGeneration += 1;
    state.modelTest = null;
    renderModelTest();
    // A status refresh makes an in-flight reasoning answer stale, but it must not
    // release the save lock: the POST is still in flight and only its own
    // completion may re-enable the rows.
    state.reasoningGeneration += 1;
    if (modelTestWasBusy) {
      message('modelTestState', '账户状态已刷新，正在发送的模型测试结果已失效。', 'error');
    }
    if (reasoningWasSaving) {
      message('reasoningState', '账户状态已刷新，正在发送的思考强度保存结果已失效；未保存草稿已保留。', 'error');
    }
    invalidateChangedQuotas(incoming.credentials);
    // Credentials are refreshed even when the model side is stale below, so the
    // edit form and picker still react to an account that disappeared.
    if (state.accountEdit && !incoming.credentials.some(item => item.id === state.accountEdit.id)) {
      resetAccountForm();
      message('accountState', '正在编辑的账户已被删除，已取消编辑。', '');
    }
    if (state.selectedCredential && !incoming.credentials.some(item => item.id === state.selectedCredential)) {
      state.selectedCredential = '';
      invalidateModelRequest();
    }
    if (!modelsWritable) {
      state.status = { ...incoming, models: cloneMappings(state.serverModels) };
      renderMeta();
      renderAccounts();
      renderModels();
      renderOverview();
      return;
    }
    const previousServerModels = state.serverModels;
    const serverModels = editorRowsFromServer(incoming.models);
    // A status refresh (account add/update, background reload) must not silently
    // discard mapping edits the operator has not saved yet. The saved baseline is
    // always refreshed; an active draft is kept even when its rows happen to look
    // unchanged again, because the conflict state must not be reset by content.
    const dirty = mappingsDirty();
    // state.status and incoming are the same object, so the server's model list
    // has to be captured before status.models is replaced with the editor-shaped
    // clone below: that write would otherwise rebind incoming.models too and the
    // stored thinking-strength policies would be lost.
    const fullModels = incoming.models;
    state.status = incoming;
    state.status.models = cloneMappings(serverModels);
    state.savedMappings = cloneMappings(serverModels);
    state.serverModels = Array.isArray(fullModels) ? fullModels : [];
    state.serverModelsGeneration += 1;
    state.modelsRevision = incoming.models_revision || '';
    if (state.modelsConflict && state.modelsRevision && state.modelsRevision !== state.modelsConflictRevision) {
      // A real status read supplied a token different from the one that lost the
      // CAS. Only now may the next save ask to confirm the overwrite.
      state.modelsConflictSeenRevision = state.modelsRevision;
    }
    // A draft whose rows are back on the saved baseline is not a pending edit:
    // release it so a later refresh can adopt the new server list. A conflict
    // stays armed even when the content happens to match.
    if (!state.modelsDraftActive || (!mappingsDirty() && !state.modelsConflict)) {
      state.mappings = cloneMappings(serverModels);
      resetMappingsDraft(state.modelsRevision);
    }
    reconcileReasoningDrafts();
    reportOrphanedReasoningDrafts(previousServerModels);
    renderMeta();
    renderAccounts();
    renderModels();
    renderOverview();
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
    // A quota pause carries its own deadline. Ticking through it is a purely
    // local re-render: no quota request and no network. Reaching the deadline
    // only switches the badge to "等待复查"; the plugin does not verify the
    // upstream quota there and does not reactivate the account.
    for (const item of state.status.credentials) {
      const pause = item.quota_pause;
      if (!pause) continue;
      const at = Date.parse(pause.resume_at || '');
      if (Number.isFinite(at) && at > now) {
        // Keep the displayed countdown current without querying the server.
        const tick = at - now > 60_000 ? 60_000 : 1_000;
        next = Math.min(next, at, now + tick);
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

  // A complete multi-model set can be rechecked too: the backend checks every
  // recorded model serially. Older servers lacking a completeness flag are
  // normalized as ambiguous for multiple models by Core.normalizeQuotaPause.
  function quotaPauseHasModels(pause) {
    return Boolean(pause && !pause.models_ambiguous && (pause.model_id || pause.multiple_models));
  }

  function canRecheckQuotaPause(item, pause) {
    return Boolean(item.billing_configured && item.workspace_id && quotaPauseHasModels(pause));
  }

  // The deadline starts an official recheck, not an unconditional recovery.
  // CPA's native console shares the auth file, so disable provenance there is
  // not reliable. The two separately configured keys cannot be linked by API.
  function quotaPauseNote(item, pause) {
    const parts = [
      '到点只表示应复查，不代表额度已恢复；开启自动暂停时，插件会逐账号串行复查，只有新鲜官方结果确认可用才解除，失败继续暂停。',
    ];
    if (item.billing_configured && item.workspace_id) {
      if (canRecheckQuotaPause(item, pause)) parts.push('可点「重查额度」提前发起复查。');
      parts.push('请确认 controlKey 和推理 key 属于同一账号；插件无法自动验证归属。');
    } else {
      parts.push('未配置官方 controlKey 或 Workspace ID，无法复查额度，仍须人工处理。');
    }
    parts.push('暂停期间请在本页启用/停用；CPA 原生入口和并发文件改写无法可靠归因，请勿在那里改动同一账户。');
    if (!quotaPauseHasModels(pause)) parts.push('耗尽模型集合不完整，无法自动确认恢复，只能人工解除。');
    else if (pause.multiple_models) parts.push('多个模型报告过额度耗尽，须逐一复查并确认属于同一 Workspace 成员。');
    const recheckMessages = {
      inflight: '仍有同一推理密钥的请求在途，结束后再复查。',
      sync_pending: '暂停状态尚未同步，成功写回前不会自动恢复。',
      unconfigured: '额度授权或 Workspace ID 缺失，无法复查。',
      ambiguous: '耗尽模型记录不完整，无法自动确认恢复。',
      unauthorized: '官方拒绝额度授权，请检查 controlKey。',
      forbidden: '官方拒绝访问 Workspace，请检查额度授权与 Workspace ID。',
      identity: '不同模型的官方复查身份不一致，继续暂停。',
      unproven: '上次复查未取得新鲜可用证据，继续暂停并稍后重试。',
      error: '上次复查失败，继续暂停并稍后重试。',
    };
    if (recheckMessages[pause.recheck_status]) parts.push(recheckMessages[pause.recheck_status]);
    if (pause.error) parts.push('暂停状态尚未写回凭据文件，插件会按固定间隔重试；期间本地仍会拦截该账户。');
    return parts.join('');
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
        const pause = item.quota_pause;
        const detail = el('div', { className: 'row-main' }, [
          el('span', { className: 'name', text: item.label }),
          el('span', {
            className: `badge ${item.disabled ? 'off' : 'on'}`,
            text: item.disabled ? '已停用' : '已启用',
          }),
          // A quota pause is separate from the operator's own switch: it never
          // changes `disabled`, so it gets its own badge and its own controls.
          pause ? el('span', { className: 'badge warn', text: Core.quotaPauseText(pause) }) : null,
          pause && pause.multiple_models ? el('span', { className: 'badge', text: '多个模型' }) : null,
          pause && pause.error ? el('span', { className: 'badge off', text: '同步待重试' }) : null,
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
          pause ? button('解除额度暂停', 'resume-quota-pause', {
            className: 'button compact',
            on: () => resumeQuotaPause(item),
          }) : null,
          canRecheckQuotaPause(item, pause) ? button('重查额度', 'recheck-quota-pause', {
            className: 'button compact',
            on: () => recheckQuotaPause(item),
          }) : null,
          button('删除', 'delete-account', {
            className: 'button compact danger',
            on: () => deleteAccount(item),
          }),
        ]);
        return el('div', { className: 'row', attrs: { 'data-credential-id': item.id } }, [
          detail,
          actions,
          pause ? el('p', { className: 'row-detail hint', attrs: { 'data-quota-pause-note': item.id }, text: quotaPauseNote(item, pause) }) : null,
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
    const exhaustsAt = Date.parse(window.forecast && window.forecast.exhausts_at);
    const resetsAt = Date.parse(window.resetsAt);
    const expired = Core.quotaForecastExpired(window);
    const forecast = Core.forecastView(expired ? { state: 'expired' } : window.forecast);
    if (forecast.text) {
      const description = el('p', { className: `quota-forecast${forecast.tone === 'warn' ? ' warn' : ''}`, text: forecast.text });
      if (!expired && window.forecast?.state === 'exhausted') {
        // An official zero remaining counter is a fact, so keep it visible.
        block.append(description);
      } else {
        block.append(el('details', { className: 'quota-trend' }, [
          el('summary', { text: '用量趋势（估算）' }),
          description,
          el('p', { className: 'quota-note', text: '仅为估算；更换模型或用量变化会影响估算，不能换算为某个模型的剩余 token 或请求次数。' }),
        ]));
      }
    }
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
    card.append(el('p', { className: 'quota-meta', text: `套餐：${plan.tier || '未知'} · 预检资金方式：${fundingName}` }));
    if (funding.data.fundingCheck) {
      const checkName = ({ available: '可用', insufficient: '不足', unknown: '未知' })[funding.data.fundingCheck] || '未知';
      card.append(el('p', { className: 'quota-meta', text: `资金检查：${checkName}` }));
    }
    card.append(el('p', { className: 'quota-note', text: '本次预检未指定模型。具体模型的套餐覆盖和实际扣费以官方为准；各窗口分别生效，额度不能相加。' }));
    for (const window of plan.windows) card.append(quotaWindowBlock(window));
    card.append(el('p', { className: 'quota-note', text: `窗口数据查询时间：${Core.formatTime(funding.fetched_at) || '未知'}` }));
    return card;
  }

  // Opening the credentials tab refreshes every configured account once. The walk
  // is strictly serial: one upstream query at a time, so a tab switch never fans
  // out into a burst against the official control API. A generation bump (logout,
  // credential edit, another tab switch) cancels the rest of the walk.
  function cancelQuotaAutoLoad() {
    state.quotaAutoGeneration += 1;
    state.quotaAutoRunning = false;
  }

  async function autoLoadQuotaForAccounts() {
    const generation = (state.quotaAutoGeneration += 1);
    state.quotaAutoRunning = true;
    try {
      for (const item of state.status.credentials.slice()) {
        if (generation !== state.quotaAutoGeneration) return;
        if (!item || !item.billing_configured || item.disabled) continue;
        if (state.quotaBusy[item.id]) continue;
        // A snapshot still inside the 60s window would only come back throttled,
        // so the walk skips it instead of spending a request to learn nothing.
        const known = state.quotas[item.id];
        const next = known && known.next_refresh_at ? Date.parse(known.next_refresh_at) : NaN;
        if (!Number.isNaN(next) && next > Date.now()) continue;
        await refreshQuota(item, { automatic: true });
      }
    } finally {
      if (generation === state.quotaAutoGeneration) state.quotaAutoRunning = false;
    }
  }

  function quotaRefreshHint(quota) {
    const next = quota && quota.next_refresh_at ? Date.parse(quota.next_refresh_at) : NaN;
    if (Number.isNaN(next)) return '打开本页会按账户顺序自动查询一次；也可以随时点「刷新额度」。';
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
      el('span', { className: 'hint', text: quota ? quotaRefreshHint(quota) : (item.billing_configured ? '打开本页会自动按顺序查询。' : '额度查询可选，未配置时不查询。') }),
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
        el('p', { className: 'quota-note', text: '点「编辑」填写同一 OpenDesign 账号的 controlKey 查询钱包；再填 Workspace ID 可查询套餐窗口。两项均可选，不影响模型推理。' }),
      ]));
    } else if (!quota) {
      cards.append(el('div', { className: 'quota-card' }, [
        el('h4', { text: '尚未查询' }),
        el('p', { className: 'quota-note', text: '打开本页会按账户顺序自动查询一次，也可以点「刷新额度」立即读取。' }),
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
    const options = [el('option', { value: '', text: '请选择账户', attrs: { disabled: '', selected: '' } })];
    for (const item of state.status.credentials) {
      options.push(el('option', {
        value: item.id,
        text: item.disabled ? `${item.label}（已停用）` : item.label,
        disabled: item.disabled,
      }));
    }
    select.replaceChildren(...options);
    select.value = state.selectedCredential;
    renderOptionalCredentialOptions();
  }

  // Credential pickers for the opt-in panels. They re-render locally only; no
  // request is issued by populating them.
  function renderOptionalCredentialOptions() {
    for (const [id, emptyLabel, includeDisabled] of [
      ['logsFilterCredential', '全部账户', true],
      ['modelTestCredential', '请选择账户', false],
    ]) {
      const select = $(id);
      if (!select) continue;
      const previous = select.value;
      const placeholderAttrs = includeDisabled ? {} : { disabled: '', selected: '' };
      const options = [el('option', { value: '', text: emptyLabel, attrs: placeholderAttrs })];
      for (const item of state.status.credentials) {
        if (!includeDisabled && item.disabled) continue;
        options.push(el('option', { value: item.id, text: item.disabled ? `${item.label}（已停用）` : item.label }));
      }
      select.replaceChildren(...options);
      const valid = state.status.credentials.some(item => item.id === previous && (includeDisabled || !item.disabled));
      select.value = valid ? previous : '';
    }
  }

  function renderModels() {
    renderCredentialOptions();
    renderUpstream();
    renderMappings();
    renderReasoning();
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

  function coverageResult(item, credentialID) {
    return state.modelCoverage[modelCoverageKey(credentialID, item.id)] || null;
  }

  function renderModelCoverage(item, credentialID) {
    const key = modelCoverageKey(credentialID, item.id);
    const result = coverageResult(item, credentialID);
    const busy = Boolean(state.modelCoverageBusy[key]);
    const status = Core.modelCoverageState(result && result.state);
    const parts = [el('span', { className: `badge ${status.tone}`, text: busy ? '查询中…' : status.text })];
    if (!result) {
      parts.push(el('span', { className: 'muted small', text: '尚未查询' }));
    } else if (result.state === 'not_configured') {
      parts.push(el('span', { className: 'muted small', text: '需同时配置 control key 和 Workspace ID' }));
    } else if (result.state === 'available' || result.state === 'stale') {
      parts.push(el('span', { className: 'small', text: Core.modelCoverageSignalText(result.model_covered) }));
      const check = Core.modelCoverageCheckText(result.funding_check);
      if (check) parts.push(el('span', { className: 'muted small', text: check }));
      if (result.fetched_at) parts.push(el('span', { className: 'muted small', text: `查询于 ${Core.formatTime(result.fetched_at)}` }));
    } else if (result.error) {
      parts.push(el('span', { className: 'muted small', text: Core.modelCoverageErrorText(result.error) }));
    }
    return el('div', { className: 'upstream-coverage' }, parts);
  }

  async function checkModelCoverage(item) {
    if (!item || state.busy || !state.upstreamFor) return;
    const credentialID = state.upstreamFor;
    const key = modelCoverageKey(credentialID, item.id);
    if (state.modelCoverageBusy[key]) return;
    const generation = state.modelCoverageGeneration;
    const revision = state.modelCoverageRevisions[credentialID] || 0;
    const stillCurrent = () => generation === state.modelCoverageGeneration &&
      revision === (state.modelCoverageRevisions[credentialID] || 0) &&
      state.upstreamFor === credentialID && state.selectedCredential === credentialID;
    state.modelCoverageBusy[key] = true;
    renderUpstream();
    try {
      const raw = await api().modelCoverage(credentialID, item.id, { refresh: true });
      if (!stillCurrent()) return;
      const coverage = Core.normalizeModelCoverage(raw);
      if (coverage.credential_id !== credentialID || coverage.model_id !== item.id) {
        throw new Error('模型覆盖响应与当前账户或模型不一致，请重新查询。');
      }
      state.modelCoverage[key] = coverage;
    } catch (error) {
      if (!stillCurrent()) return;
      if (isManagementAuthError(error)) {
        showAuth(error.status === 401
          ? '401：管理密钥无效或已过期，请重新输入当前 CPA 入口的管理密钥。'
          : `403：${formatError(error)}`);
        return;
      }
      state.modelCoverage[key] = Core.normalizeModelCoverage({
        credential_id: credentialID,
        model_id: item.id,
        state: 'error',
        error: { kind: error && error.code || '', message: formatError(error) },
      });
    } finally {
      if (stillCurrent()) {
        delete state.modelCoverageBusy[key];
        renderUpstream();
      }
    }
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
    const credentialID = state.upstreamFor;
    const rows = visible.map(item => {
      const box = el('input', { type: 'checkbox', checked: Core.hasUpstream(state.mappings, item.id), disabled: state.modelsSaving });
      box.addEventListener('change', () => {
        state.mappings = box.checked
          ? Core.addUpstream(state.mappings, item.id)
          : Core.removeUpstream(state.mappings, item.id);
        noteMappingsEdited();
        renderMappings();
      });
      const details = [el('span', { className: 'mono', text: item.id })];
      if (item.owned_by) details.push(el('span', { className: 'muted small', text: item.owned_by }));
      const action = button('检查覆盖', 'check-model-coverage', {
        className: 'button compact',
        disabled: state.busy || Boolean(state.modelCoverageBusy[modelCoverageKey(credentialID, item.id)]),
        on: event => {
          if (event && typeof event.preventDefault === 'function') event.preventDefault();
          if (event && typeof event.stopPropagation === 'function') event.stopPropagation();
          return checkModelCoverage(item);
        },
      });
      return el('div', { className: 'check upstream' }, [
        box,
        el('div', { className: 'upstream-detail' }, details),
        action,
        renderModelCoverage(item, credentialID),
      ]);
    });
    list.replaceChildren(...rows);
    if (state.modelsSaving) setRowControlsDisabled(list, true);
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
        noteMappingsEdited();
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
        noteMappingsEdited();
        updateDirtyBadge();
      });
      const remove = button('删除', 'remove-mapping', {
        className: 'button compact danger',
        on: () => {
          state.mappings.splice(index, 1);
          noteMappingsEdited();
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
    if (state.modelsSaving) setRowControlsDisabled(list, true);
    // Keep checkbox state in sync after edits/removals without re-rendering the
    // upstream list (which would drop the search field's focus).
    syncUpstreamChecks();
    if (dirty) updateDirtyBadge();
  }

  function syncUpstreamChecks() {
    // children is an HTMLCollection in browsers: no Array methods, so always
    // spread it before searching (the old code called children.find here).
    for (const node of Array.from($('upstreamList')?.children || [])) {
      if (node.tagName !== 'LABEL' && !String(node.className || '').split(/\s+/).includes('upstream')) continue;
      const kids = Array.from(node.children || []);
      const detail = kids.find(child => String(child.className || '').split(/\s+/).includes('upstream-detail'));
      const model = detail && Array.from(detail.children || []).find(child => child.tagName === 'SPAN' && child.className === 'mono');
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
    noteMappingsEdited();
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

  // Reflect an acknowledged mutation immediately. If the following status GET
  // fails, the page must still show what the server already confirmed.
  function applyAccountResult(result, fallback, removed = false) {
    const item = { ...fallback, ...(result && typeof result === 'object' ? result : {}) };
    const credentials = state.status.credentials.filter(account => account.id !== item.id);
    if (!removed && item.id) credentials.push(item);
    // state.status.models is the editor-shaped clone (no policy/revision), so
    // keep the full server list here instead of letting the acknowledged
    // account edit silently drop every model's reasoning revision.
    applyStatus({
      ...state.status,
      credentials,
      models: state.serverModels,
      // A synthetic status must not roll the whole-list token back to the last
      // status snapshot: a later save would silently lose its CAS lock.
      models_revision: state.modelsRevision,
    });
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
    const sessionGeneration = state.sessionGeneration;
    setBusy(true);
    // Values are captured for this request only; every send outcome clears the
    // DOM, including a lost response or a server error after persistence.
    if (keyInput) keyInput.value = '';
    if ($('accountControlKey')) $('accountControlKey').value = '';
    if (editing) invalidateQuota(editing.id);
    let saved = false;
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
        const result = await api().updateCredential(editing.id, body);
        if (sessionGeneration !== state.sessionGeneration) return;
        saved = true;
        applyAccountResult(result, { ...state.status.credentials.find(item => item.id === editing.id), id: editing.id, label });
        const wasEditing = editing.label;
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
        const result = await api().addCredential(label, key, extra);
        if (sessionGeneration !== state.sessionGeneration) return;
        saved = true;
        applyAccountResult(result, { label });
        if (labelInput) labelInput.value = '';
        resetAccountForm();
        message('accountState', billing.controlKey
          ? '账户已保存，API key 与 controlKey 都已从页面清空。点「刷新额度」才会查询额度。'
          : '账户已保存，API key 已从页面清空。额度查询配置可随后通过「编辑」添加。');
      }
      await refreshStatus();
      if (sessionGeneration !== state.sessionGeneration) return;
      renderAccounts();
    } catch (error) {
      if (sessionGeneration !== state.sessionGeneration) return;
      if (isManagementAuthError(error)) {
        clearRejectedManagementAuth();
        const prefix = saved ? '账户已保存，但读取最新状态时认证失效。' : '';
        showAuth(prefix + (error.status === 401 ? '401：管理密钥无效或已过期，请重新验证。' : `403：${formatError(error)}`));
      } else {
        message('accountState', saved
          ? `账户已保存，但读取最新状态失败：${formatError(error)}。请刷新状态确认，不要重复添加。`
          : `${editing ? '保存修改' : '保存'}请求失败，服务器结果尚未确认：${formatError(error)}。密钥输入已清空，请先刷新状态确认后再试。`, 'error');
      }
    } finally {
      // A stale response from a replaced session must not release a lock the new
      // session now owns.
      if (sessionGeneration === state.sessionGeneration) setBusy(false);
    }
  }

  async function deleteAccount(item) {
    if (state.busy) return;
    if (typeof confirm === 'function' && !confirm(`删除账户「${item.label}」？此操作会删除 CPA 中保存的 OpenDesign key。`)) return;
    const sessionGeneration = state.sessionGeneration;
    setBusy(true);
    message('accountState', '正在删除账户…');
    let deleted = false;
    try {
      await api().deleteCredential(item.id);
      if (sessionGeneration !== state.sessionGeneration) return;
      deleted = true;
      applyAccountResult(null, item, true);
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
      if (sessionGeneration !== state.sessionGeneration) return;
      if (isManagementAuthError(error)) {
        clearRejectedManagementAuth();
        showAuth(`${deleted ? '账户已删除，但读取最新状态时认证失效。' : ''}401/403：${formatError(error)}`);
      } else if (deleted) {
        message('accountState', `账户已删除，但读取最新状态失败：${formatError(error)}。请刷新状态确认。`, 'error');
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
      if (sessionGeneration === state.sessionGeneration) setBusy(false);
    }
  }

  async function toggleAccount(item) {
    if (state.busy) return;
    const sessionGeneration = state.sessionGeneration;
    setBusy(true);
    message('accountState', item.disabled ? '正在启用账户…' : '正在停用账户…');
    let updated = false;
    try {
      const result = await api().updateCredential(item.id, { disabled: !item.disabled });
      if (sessionGeneration !== state.sessionGeneration) return;
      updated = true;
      applyAccountResult(result, { ...item, disabled: !item.disabled });
      if (!item.disabled && state.selectedCredential === item.id) {
        state.selectedCredential = '';
        invalidateModelRequest();
      }
      message('accountState', item.disabled ? '账户已启用。' : '账户已停用。');
      await refreshStatus();
    } catch (error) {
      if (sessionGeneration !== state.sessionGeneration) return;
      if (isManagementAuthError(error)) {
        clearRejectedManagementAuth();
        showAuth(`${updated ? '账户状态已更新，但读取最新状态时认证失效。' : ''}401/403：${formatError(error)}`);
      } else message('accountState', updated
        ? `账户已${item.disabled ? '启用' : '停用'}，但读取最新状态失败：${formatError(error)}。请刷新状态确认。`
        : `更新失败：${formatError(error)}`, 'error');
    } finally {
      if (sessionGeneration === state.sessionGeneration) setBusy(false);
    }
  }

  // Manual resume clears only the plugin-owned quota block; the operator's own
  // disabled switch is untouched. It always asks first, because it puts an
  // exhausted account straight back into CPA scheduling.
  async function resumeQuotaPause(item) {
    if (state.busy) return;
    if (typeof confirm === 'function' && !confirm(`解除账户「${item.label}」的额度暂停？这不会证明额度已恢复，也不会改变通过本插件设置的人工启用状态。`)) return;
    const sessionGeneration = state.sessionGeneration;
    setBusy(true);
    message('accountState', '正在解除额度暂停…');
    try {
      await api().resumeQuotaPause(item.id);
      if (sessionGeneration !== state.sessionGeneration) return;
      await refreshStatus();
      if (sessionGeneration !== state.sessionGeneration) return;
      const current = state.status.credentials.find(credential => credential.id === item.id);
      if (!current) {
        message('accountState', '解除请求已完成，但账户状态已变化，请查看最新列表。');
      } else if (current.quota_paused) {
        message('accountState', '解除请求已完成，账户仍处于额度暂停；手动解除不会查询或证明额度已恢复，请查看最新暂停详情。');
      } else {
        message('accountState', '解除请求已完成，当前没有插件额度暂停；这不会查询或证明额度已恢复，账户是否调度仍由 CPA 与启用状态决定。');
      }
    } catch (error) {
      if (sessionGeneration !== state.sessionGeneration) return;
      message('accountState', `解除失败：${formatError(error)}`, 'error');
    } finally {
      if (sessionGeneration === state.sessionGeneration) setBusy(false);
    }
  }

  // Recheck all known exhausted models. Cached or failed results cannot clear
  // the pause; the background worker also tries when the deadline is due.
  async function recheckQuotaPause(item) {
    if (state.busy) return;
    if (typeof confirm === 'function' && !confirm(`用官方额度预检查重新确认账户「${item.label}」的额度？只有所有已知耗尽模型均有新鲜可用证据时才会解除暂停。`)) return;
    const sessionGeneration = state.sessionGeneration;
    setBusy(true);
    message('accountState', '正在重查额度…');
    try {
      await api().recheckQuotaPause(item.id);
      if (sessionGeneration !== state.sessionGeneration) return;
      await refreshStatus();
      if (sessionGeneration !== state.sessionGeneration) return;
      // A successful response can mean the pause was already gone before this
      // request; it does not necessarily prove that this call queried upstream.
      // The account may also have been paused again before the status refresh.
      const current = state.status.credentials.find(credential => credential.id === item.id);
      if (!current) {
        message('accountState', '复查请求已完成，但账户状态已变化，请查看最新列表。');
      } else if (current.quota_paused) {
        message('accountState', '复查请求已完成，账户仍处于额度暂停；请查看最新暂停详情。');
      } else {
        message('accountState', '复查请求已完成，当前没有插件额度暂停；账户是否调度仍由 CPA 与启用状态决定。');
      }
    } catch (error) {
      if (sessionGeneration !== state.sessionGeneration) return;
      message('accountState', `重查未通过：${formatError(error)}`, 'error');
    } finally {
      if (sessionGeneration === state.sessionGeneration) setBusy(false);
    }
  }

  async function refreshStatus() {
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    const serverModelsGeneration = state.serverModelsGeneration;
    const raw = await api().status();
    // A refresh started under an older management identity must not repaint the
    // session that replaced it (logout or another key).
    if (generation !== state.optionalGeneration || sessionGeneration !== state.sessionGeneration) return null;
    applyStatus(raw, { serverModelsGeneration });
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
    const sessionGeneration = state.sessionGeneration;
    const stillCurrent = () => requestToken === modelRequestSeq &&
      sessionGeneration === state.sessionGeneration &&
      state.selectedCredential === credentialID;
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
      if (activeModelRequest === requestToken && sessionGeneration === state.sessionGeneration) {
        activeModelRequest = 0;
        setBusy(false);
      }
    }
  }

  // Refreshes one account. The explicit button and the serial tab walk share it;
  // automatic runs stay silent on success so opening the tab does not spam the
  // status line with one message per account.
  async function refreshQuota(item, options = {}) {
    const automatic = options.automatic === true;
    if (!item || !item.billing_configured) {
      if (!automatic) message('accountState', '该账户还没有配置官方 controlKey，无法查询额度。点「编辑」添加后再试。', 'error');
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
        if (!automatic) message('accountState', `额度刷新已节流：${quotaRefreshHint(quota)}`, '');
      } else {
        state.quotas[item.id] = quota;
        const failed = quota.state === 'error';
        // The serial walk reports failures only: a toast per healthy account on
        // every tab switch would bury the one account that actually needs help.
        if (!automatic || failed) {
          message('accountState', failed
            ? `账户「${item.label}」额度查询失败：${quotaErrorText('未取得数据', quota.error)}；管理登录未受影响。`
            : `账户「${item.label}」额度已更新（${QUOTA_STATE_TEXT[quota.state] || quota.state}）。`, failed ? 'error' : '');
        }
      }
      renderAccounts();
    } catch (error) {
      // A logout/key switch bumps both generations; the old answer (including a
      // 401) must neither repaint nor log the new session out.
      if (!stillCurrent()) return;
      if (isManagementAuthError(error)) {
        clearRejectedManagementAuth();
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

  // --- optional routes (explicit operator action only) ---------------------
  //
  // None of these functions is called by rendering, status loading, tab
  // switching or a timer. Each one is reachable only from its own button.

  function optionalBusy(name) {
    return Boolean(state.optionalBusy[name]);
  }

  function syncOptionalBusyControls() {
    const logBusy = optionalLogBusy();
    for (const id of ['refreshLogs', 'refreshStats', 'clearLogs', 'logsFilterStatus', 'logsFilterCredential', 'logsFilterModel', 'logsFilterAttempted', 'logsFilterLimit']) {
      setDisabled(id, logBusy);
    }
    setDisabled('logsPrev', logBusy || state.logPage.offset <= 0);
    setDisabled('logsNext', logBusy || !state.logPage.has_more);
    for (const id of ['loadConfig']) setDisabled(id, optionalBusy('config'));
    const configLocked = optionalBusy('config') || !state.config;
    for (const id of ['configLogsEnabled', 'configRetentionDays', 'configMaxEntries', 'saveConfig',
      'configPauseEnabled', 'configPauseMaxSeconds', 'savePauseConfig']) {
      setDisabled(id, configLocked);
    }
    setDisabled('loadRuntime', optionalBusy('runtime'));
    setDisabled('openErrorRules', optionalBusy('errorRules'));
    syncErrorRulesControls();
    for (const id of ['runModelTest', 'modelTestCredential', 'modelTestModel', 'modelTestPrompt', 'modelTestReasoning']) {
      setDisabled(id, optionalBusy('modelTest'));
    }
  }

  function setOptionalBusy(name, value) {
    if (value) state.optionalBusy[name] = true;
    else delete state.optionalBusy[name];
    syncOptionalBusyControls();
  }

  function optionalLogBusy() {
    return optionalBusy('logs') || optionalBusy('stats') || optionalBusy('logsClear');
  }

  function optionalErrorText(error) {
    if (isManagementAuthError(error)) {
      return error.status === 401
        ? '401：管理密钥无效或已过期，请重新验证后再试。'
        : '403：管理请求被拒绝，请确认管理密钥和来源 IP。';
    }
    const status = error && Number.isInteger(error.status) ? error.status : 0;
    if (status === 404 || status === 405) return `接口未提供（HTTP ${status}）`;
    return status ? `请求失败（HTTP ${status}）：${formatError(error)}` : '请求失败，未连接到管理接口。';
  }

  function logStatusLabel(status, failed) {
    if (failed) return '失败';
    if (status === 'success') return '成功';
    if (status === 'error') return '失败';
    return '未知';
  }

  function credentialLabel(id) {
    const item = state.status.credentials.find(entry => entry.id === id);
    return item ? item.label : (id || '未知账户');
  }

  function logRow(record) {
    const failed = record.status === 'error' || (record.http_status !== null && record.http_status >= 400);
    const head = [el('span', {
      className: `badge ${failed ? 'off' : 'on'}`,
      text: logStatusLabel(record.status, failed),
    })];
    if (record.upstream_attempted === false) head.push(el('span', { className: 'badge', text: '本地拒绝' }));
    if (record.http_status !== null && record.upstream_attempted !== false) head.push(el('span', { className: 'mono', text: `上游 HTTP ${record.http_status}` }));
    if (record.stream) head.push(el('span', { className: 'badge', text: '流式' }));
    const details = [];
    if (record.model) details.push(`模型 ${record.model}`);
    if (record.started_at) details.push(`时间 ${Core.formatTime(record.started_at) || record.started_at}`);
    if (record.upstream_model && record.upstream_model !== record.model) details.push(`上游 ${record.upstream_model}`);
    // Only the conversion is shown: an unmapped request is indistinguishable from
    // one that never carried a strength, so the row stays quiet in both cases.
    if (record.reasoning_mapping) {
      details.push(`思考强度 ${Core.reasoningConversionText(record.reasoning_effort, record.reasoning_mapping)}`);
    } else if (record.reasoning_effort) {
      details.push(`思考强度 ${Core.reasoningLabel(record.reasoning_effort)}`);
    }
    details.push(`账户 ${credentialLabel(record.credential_id)}`);
    if (record.duration_ms !== null) details.push(`耗时 ${record.duration_ms} ms`);
    if (record.total_tokens !== null) details.push(`Token ${record.total_tokens}`);
    else if (record.input_tokens !== null || record.output_tokens !== null) {
      details.push(`Token ${record.input_tokens ?? '?'} / ${record.output_tokens ?? '?'}`);
    }
    if (record.error_kind) details.push(`错误 ${record.error_kind}`);
    if (record.request_id) details.push(`请求 ${record.request_id}`);
    return el('div', { className: 'log-row', attrs: { 'data-log-id': record.id } }, [
      el('div', { className: 'log-head' }, head),
      el('div', { className: 'log-detail', text: details.join(' · ') }),
    ]);
  }

  function renderRequestLogs() {
    const list = $('requestLogList');
    if (!list) return;
    if (!state.logPage.loaded) {
      list.replaceChildren(el('p', { className: 'empty', text: '尚未读取。点「刷新日志」后才会请求 GET /request-logs。' }));
    } else if (!state.requestLogs.length) {
      list.replaceChildren(el('p', { className: 'empty', text: '没有匹配的请求日志。' }));
    } else {
      list.replaceChildren(...state.requestLogs.map(logRow));
    }
    const info = $('logsPageInfo');
    if (info) {
      if (!state.logPage.loaded) info.textContent = '尚未读取日志。';
      else {
        const start = state.logPage.total ? state.logPage.offset + 1 : 0;
        const end = state.logPage.offset + state.requestLogs.length;
        info.textContent = `显示 ${start}-${end} / 共 ${state.logPage.total} 条`;
      }
    }
    setDisabled('logsPrev', optionalLogBusy() || state.logPage.offset <= 0);
    setDisabled('logsNext', optionalLogBusy() || !state.logPage.has_more);
  }

  function renderRequestStats() {
    const container = $('requestStats');
    const models = $('requestStatsModels');
    const stats = state.requestStats;
    if (!container) return;
    if (!stats) {
      container.replaceChildren(el('p', { className: 'empty', text: '点「读取统计」后才会请求 GET /request-stats。' }));
      if (models) models.replaceChildren();
      return;
    }
    const facts = [
      { label: '总请求', value: String(stats.total) },
      { label: '成功', value: String(stats.success) },
      { label: '失败', value: String(stats.failed) },
      { label: '流式', value: String(stats.streams) },
    ];
    const rate = Core.formatRatio(stats.success_rate);
    if (rate) facts.push({ label: '成功率', value: rate });
    if (stats.tokens.total !== null) facts.push({ label: 'Token 总量', value: String(stats.tokens.total) });
    if (stats.tokens.input !== null) facts.push({ label: '输入 Token', value: String(stats.tokens.input) });
    if (stats.tokens.output !== null) facts.push({ label: '输出 Token', value: String(stats.tokens.output) });
    facts.push({ label: '有 usage 的请求', value: String(stats.tokens.requests_with_usage) });
    facts.push({ label: '上游已尝试', value: String(stats.upstream_attempted) });
    facts.push({ label: '本地拒绝', value: String(stats.local_rejected) });
    facts.push({ label: '无 usage 的请求', value: String(stats.tokens.requests_without_usage) });
    const scope = retentionScopeText(stats.retention);
    if (scope) facts.push({ label: '统计范围', value: scope });
    renderFacts(container, facts);
    if (!models) return;
    if (!stats.models.length) {
      models.replaceChildren(el('p', { className: 'empty', text: '统计范围内没有按模型聚合的数据。' }));
      return;
    }
    models.replaceChildren(...stats.models.map(item => {
      const parts = [`共 ${item.total} 次`, `成功 ${item.success}`, `失败 ${item.failed}`,
        `上游已尝试 ${item.upstream_attempted}`, `本地拒绝 ${item.local_rejected}`];
      if (item.total_tokens !== null) parts.push(`Token ${item.total_tokens}`);
      return el('div', { className: 'row', attrs: { 'data-stats-model': item.model } }, [
        el('div', { className: 'row-main' }, [
          el('span', { className: 'name mono', text: item.model }),
          el('span', { className: `badge ${item.failed ? 'warn' : 'good'}`, text: item.failed ? '有失败' : '全部成功' }),
        ]),
        el('div', { className: 'row-detail hint', text: parts.join(' · ') }),
      ]);
    }));
  }

  function readLogQuery() {
    const status = $('logsFilterStatus');
    const model = $('logsFilterModel');
    const credential = $('logsFilterCredential');
    const attempted = $('logsFilterAttempted');
    const limit = $('logsFilterLimit');
    const parsedLimit = limit ? Number(limit.value) : 50;
    return {
      offset: state.logQuery.offset,
      limit: [25, 50, 100].includes(parsedLimit) ? parsedLimit : 50,
      status: status ? status.value : '',
      model: model ? model.value.trim() : '',
      credentialID: credential ? credential.value : '',
      attempted: attempted ? attempted.value : '',
    };
  }

  async function loadRequestLogs(options = {}) {
    if (optionalLogBusy()) return;
    const query = { ...readLogQuery() };
    if (options.reset) query.offset = 0;
    if (options.offset !== undefined) query.offset = options.offset;
    if (options.keepFilters !== true) {
      // Normalise the visible filter controls so the next page keeps them.
      const limit = $('logsFilterLimit');
      if (limit) limit.value = String(query.limit);
    }
    state.logQuery = { ...query };
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('logs', true);
    message('logsState', '正在读取请求日志…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    try {
      const raw = await api().requestLogs(query);
      if (!stillCurrent()) return;
      const page = Core.normalizeRequestLogsPage(raw);
      state.requestLogs = page.logs;
      state.logPage = {
        loaded: true,
        total: page.total,
        offset: page.offset,
        limit: page.limit,
        has_more: page.has_more,
        next_offset: page.next_offset,
        retention: page.retention,
      };
      renderRequestLogs();
      message('logsState', page.logs.length
        ? `已读取 ${page.logs.length} 条日志（共 ${page.total} 条）。`
        : '接口已响应，但没有匹配的日志。');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      state.requestLogs = [];
      state.logQuery = { ...state.logQuery, offset: 0 };
      state.logPage = {
        ...state.logPage,
        loaded: true,
        total: 0,
        offset: 0,
        has_more: false,
        next_offset: null,
      };
      renderRequestLogs();
      message('logsState', optionalErrorText(error), 'error');
    } finally {
      if (stillCurrent()) {
        setOptionalBusy('logs', false);
        renderRequestLogs();
      }
    }
  }

  function nextLogsPage() {
    if (optionalLogBusy() || !state.logPage.has_more) return;
    const fallback = Math.min(1_000_000, state.logPage.offset + state.logPage.limit);
    const next = Number.isSafeInteger(state.logPage.next_offset) && state.logPage.next_offset >= 0 && state.logPage.next_offset <= 1_000_000
      ? state.logPage.next_offset : null;
    const target = next !== null && next > state.logPage.offset ? next : fallback;
    if (target <= state.logPage.offset) return;
    loadRequestLogs({ offset: target });
  }

  function previousLogsPage() {
    if (optionalLogBusy() || state.logPage.offset <= 0) return;
    loadRequestLogs({ offset: Math.max(0, state.logPage.offset - state.logPage.limit) });
  }

  async function loadRequestStats() {
    if (optionalLogBusy()) return;
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('stats', true);
    message('logsState', '正在读取请求统计…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    try {
      const raw = await api().requestStats();
      if (!stillCurrent()) return;
      state.requestStats = Core.normalizeRequestStats(raw);
      renderRequestStats();
      message('logsState', '统计已更新。');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      state.requestStats = null;
      renderRequestStats();
      message('logsState', optionalErrorText(error), 'error');
    } finally {
      if (stillCurrent()) {
        setOptionalBusy('stats', false);
        renderRequestLogs();
      }
    }
  }

  async function clearRequestLogs() {
    if (optionalLogBusy()) return;
    if (typeof confirm === 'function' && !confirm('清空全部请求日志？此操作不可撤销，但不会影响账户、模型映射或推理。')) return;
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('logsClear', true);
    message('logsState', '正在清空请求日志…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    try {
      await api().clearRequestLogs();
      if (!stillCurrent()) return;
      state.requestLogs = [];
      state.requestStats = null;
      state.logPage = { ...state.logPage, loaded: true, total: 0, offset: 0, has_more: false, next_offset: null };
      state.logQuery = { ...state.logQuery, offset: 0 };
      renderRequestLogs();
      renderRequestStats();
      message('logsState', '请求日志已清空。', 'ok');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      message('logsState', `清空失败：${optionalErrorText(error)}`, 'error');
    } finally {
      if (stillCurrent()) {
        setOptionalBusy('logsClear', false);
        renderRequestLogs();
      }
    }
  }

  function renderConfig() {
    const config = state.config;
    const enabled = $('configLogsEnabled');
    const days = $('configRetentionDays');
    const max = $('configMaxEntries');
    const pauseEnabled = $('configPauseEnabled');
    const pauseMax = $('configPauseMaxSeconds');
    // `state.config` always comes from Core.normalizeConfig, but renderConfig is
    // also reached from tests and partial states, so tolerate a missing section.
    const pausePolicy = config && config.pause ? config.pause : null;
    if (pauseEnabled) pauseEnabled.checked = Boolean(pausePolicy && pausePolicy.enabled);
    if (pauseMax) pauseMax.value = pausePolicy ? String(pausePolicy.max_seconds) : '';
    if (config) {
      if (enabled) enabled.checked = config.logs.enabled !== false;
      if (days) days.value = config.logs.retention_days === null ? '' : String(config.logs.retention_days);
      if (max) max.value = config.logs.max_entries === null ? '' : String(config.logs.max_entries);
    } else {
      if (enabled) enabled.checked = false;
      if (days) days.value = '';
      if (max) max.value = '';
    }
    renderHostOwnedFacts();
    syncOptionalBusyControls();
  }

  function renderHostOwnedFacts() {
    const container = $('hostOwnedFacts');
    if (!container) return;
    const config = state.config;
    if (!config) {
      container.replaceChildren(el('p', { className: 'empty', text: '点「读取配置」后才会请求 GET /config。' }));
      return;
    }
    const facts = [
      { label: '上游地址', value: config.upstream.origin
        ? `${config.upstream.origin}${config.upstream.fixed ? '（固定）' : ''}`
        : (config.upstream.fixed ? '未知（固定）' : '未知') },
      { label: '代理池', value: config.proxy.owner ? `宿主 ${config.proxy.owner}${config.proxy.read_only ? '（只读）' : ''}` : '宿主拥有（只读）' },
      { label: '路由', value: config.routing.owner ? `宿主 ${config.routing.owner}${config.routing.read_only ? '（只读）' : ''}` : '宿主拥有（只读）' },
      { label: '本插件私有代理', value: '不提供' },
    ];
    if (config.log_level) facts.push({ label: '日志级别', value: config.log_level });
    renderFacts(container, facts);
  }

  // Saves only the plugin-owned quota-pause policy. It never changes how CPA
  // schedules credentials and never touches an operator's disabled choice.
  async function savePauseConfig(event) {
    if (event) event.preventDefault();
    if (optionalBusy('config')) return;
    const enabled = $('configPauseEnabled');
    const max = $('configPauseMaxSeconds');
    const seconds = max ? Number(max.value) : NaN;
    if (!Number.isSafeInteger(seconds) || seconds < 60 || seconds > 604800) {
      message('configState', '额度暂停上限必须是 60 到 604800 之间的整数秒。', 'error');
      return;
    }
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('config', true);
    message('configState', '正在保存额度暂停设置…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    try {
      const raw = await api().updatePauseConfig({ enabled: Boolean(enabled && enabled.checked), max_seconds: seconds });
      if (!stillCurrent()) return;
      state.config = Core.normalizeConfig(raw);
      renderConfig();
      message('configState', '额度暂停设置已保存。', 'ok');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      message('configState', optionalErrorText(error), 'error');
    } finally {
      if (stillCurrent()) setOptionalBusy('config', false);
    }
  }

  async function loadConfig() {
    if (optionalBusy('config')) return;
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('config', true);
    message('configState', '正在读取配置…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    try {
      const raw = await api().config();
      if (!stillCurrent()) return;
      state.config = Core.normalizeConfig(raw);
      renderConfig();
      message('configState', '配置已更新（只有请求日志设置可写）。', 'ok');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      state.config = null;
      renderConfig();
      message('configState', optionalErrorText(error), 'error');
    } finally {
      if (stillCurrent()) setOptionalBusy('config', false);
    }
  }

  async function saveConfig(event) {
    if (event) event.preventDefault();
    if (optionalBusy('config')) return;
    const enabled = $('configLogsEnabled');
    const days = $('configRetentionDays');
    const max = $('configMaxEntries');
    const logs = { enabled: Boolean(enabled && enabled.checked) };
    const retention = days ? Number(days.value) : NaN;
    const entries = max ? Number(max.value) : NaN;
    if (!Number.isSafeInteger(retention) || retention < 1 || retention > 90) {
      message('configState', '保留天数必须是 1 到 90 之间的整数。', 'error');
      return;
    }
    if (!Number.isSafeInteger(entries) || entries < 1 || entries > 500) {
      message('configState', '最多条数必须是 1 到 500 之间的整数。', 'error');
      return;
    }
    logs.retention_days = retention;
    logs.max_entries = entries;
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('config', true);
    message('configState', '正在保存日志设置…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    try {
      const raw = await api().updateConfig(logs);
      if (!stillCurrent()) return;
      state.config = Core.normalizeConfig(raw);
      renderConfig();
      message('configState', '日志设置已保存。', 'ok');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      message('configState', `保存失败：${optionalErrorText(error)}`, 'error');
    } finally {
      if (stillCurrent()) setOptionalBusy('config', false);
    }
  }

  function hostBoundaryText(host) {
    if (!host) return '宿主状态未知';
    const flags = [];
    if (host.disabled === true) flags.push('已停用');
    else if (host.disabled === false) flags.push('已启用');
    if (host.unavailable === true) flags.push('不可用');
    else if (host.unavailable === false) flags.push('可用');
    if (host.status) flags.push(`状态 ${host.status}`);
    if (host.status_message) flags.push(host.status_message);
    if (host.next_retry_after) flags.push(`下次重试 ${Core.formatTime(host.next_retry_after) || host.next_retry_after}`);
    return flags.length ? flags.join(' / ') : '宿主状态未知';
  }

  function renderRuntime() {
    const facts = $('runtimeFacts');
    const list = $('runtimeCredentials');
    const runtime = state.runtime;
    if (!runtime) {
      renderFacts(facts, [], '点「读取运行时」后才会请求 GET /runtime。');
      if (list) list.replaceChildren(el('p', { className: 'empty', text: '尚未读取 runtime。' }));
      return;
    }
    renderFacts(facts, [
      { label: '插件状态', value: runtime.stopped ? '已停止' : (runtime.loaded ? '已加载' : '未加载') },
      { label: '在飞请求', value: String(runtime.active_requests) },
      { label: '待处理操作', value: String(runtime.pending_operations) },
      { label: '映射模型数', value: String(runtime.model_count) },
      { label: '宿主账户状态', value: runtime.host.available ? '可读取' : '不可读取（仅插件侧状态）' },
      { label: '观测时间', value: Core.formatTime(runtime.observed_at) || '未知' },
    ]);
    if (list) {
      if (!runtime.credentials.length) {
        list.replaceChildren(el('p', { className: 'empty', text: 'runtime 未返回账户。' }));
      } else {
        list.replaceChildren(...runtime.credentials.map(item => {
          const head = [
            el('span', { className: 'name', text: item.label }),
            el('span', { className: `badge ${item.disabled ? 'off' : 'on'}`, text: item.disabled ? '已停用' : '已启用' }),
          ];
          if (item.testing) head.push(el('span', { className: 'badge', text: '测试中' }));
          const details = [`映射模型 ${item.model_count} 个（${item.models_scope === 'shared' ? '全局共享' : '未知范围'}）`];
          details.push(`宿主账户：${hostBoundaryText(item.host)}`);
          if (item.last_test) {
            const when = Core.formatTime(item.last_test.tested_at);
            const outcome = item.last_test.status === 'success' ? '成功' : '失败';
            const identity = [
              item.last_test.model_id ? `模型 ${item.last_test.model_id}` : '',
              item.last_test.upstream_model ? `上游 ${item.last_test.upstream_model}` : '',
              item.last_test.error_kind ? `原因 ${item.last_test.error_kind}` : '',
            ].filter(Boolean).join('，');
            details.push(`最近测试 ${outcome}${when ? `（${when}）` : ''}${identity ? `：${identity}` : ''}`);
          }
          return el('div', { className: 'row', attrs: { 'data-runtime-credential': item.id } }, [
            el('div', { className: 'row-main' }, head),
            el('div', { className: 'row-detail hint', text: details.join(' · ') }),
          ]);
        }));
      }
    }
  }

  async function loadRuntime() {
    if (optionalBusy('runtime')) return;
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('runtime', true);
    message('configState', '正在读取运行时状态…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    try {
      const raw = await api().runtime();
      if (!stillCurrent()) return;
      state.runtime = Core.normalizeRuntime(raw);
      renderRuntime();
      message('configState', '运行时状态已更新（只读）。', 'ok');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      state.runtime = null;
      renderRuntime();
      message('configState', optionalErrorText(error), 'error');
    } finally {
      if (stillCurrent()) setOptionalBusy('runtime', false);
    }
  }

  // --- retry scheduling / request-scoped error rules ------------------------
  //
  // The policy is edited in a dialog and pushed into every credential's auth
  // file. Opening the dialog is the only read; nothing here runs on page load,
  // tab switch or render. A generation counter discards a late response from a
  // dialog the operator has already reopened.

  const errorRulesView = { loaded: false, busy: false, dirty: false, generation: 0 };

  function errorRulesDraft() {
    return state.errorRules ? state.errorRules.rules : [];
  }

  function errorRulesBaseline() {
    return state.errorRules ? state.errorRules.baseline : [];
  }

  function errorRulesPayload() {
    return Core.buildErrorRulesPayload(errorRulesDraft());
  }

  function errorRulesDirty() {
    return JSON.stringify(errorRulesPayload()) !== JSON.stringify(Core.buildErrorRulesPayload(errorRulesBaseline()));
  }

  function markErrorRulesDirty(options = {}) {
    errorRulesView.dirty = errorRulesDirty();
    syncErrorRulesControls();
    // Editing a value refreshes only the card's own explanation, so the input
    // keeps focus and the caret position; structural edits rebuild the list.
    if (options.rerender) renderErrorRules();
    else refreshErrorRuleExplanations();
  }

  function refreshErrorRuleExplanations() {
    const list = $('errorRulesList');
    if (!list) return;
    const rules = errorRulesDraft();
    for (const card of [...(list.children || [])]) {
      const index = Number(card.getAttribute && card.getAttribute('data-rule'));
      const rule = Number.isInteger(index) ? rules[index] : null;
      if (!rule) continue;
      const explanation = Core.explainErrorRule(rule, index);
      for (const node of [...(card.children || [])]) {
        if (node.className && node.className.indexOf('error-rule-name') >= 0) node.textContent = explanation.name;
        if (node.className && node.className.indexOf('error-rule-description') >= 0) node.textContent = explanation.detail;
      }
    }
  }

  function syncErrorRulesControls() {
    const locked = errorRulesView.busy || !errorRulesView.loaded;
    setDisabled('addErrorRule', locked);
    setDisabled('saveErrorRules', locked || !errorRulesView.dirty);
    setDisabled('resetErrorRules', locked);
    // Re-syncing while the draft has unsaved edits would push the saved policy
    // and then look like the edits were applied, so it stays blocked.
    setDisabled('retryErrorRules', locked || errorRulesView.dirty);
    setDisabled('closeErrorRules', errorRulesView.busy);
    const retry = $('retryErrorRules');
    if (retry) retry.setAttribute('title', errorRulesView.dirty ? '请先保存编辑中的规则' : '将已保存规则重新同步到所有账号');
    const list = $('errorRulesList');
    if (!list) return;
    const stack = [...(list.children || [])];
    while (stack.length) {
      const node = stack.pop();
      if (node.tagName === 'INPUT' || node.tagName === 'SELECT' || node.tagName === 'TEXTAREA' || node.tagName === 'BUTTON') {
        node.disabled = locked;
      }
      if (node.children) stack.push(...node.children);
    }
  }

  function errorRuleConditionRow(ruleIndex, condition, position) {
    const kind = el('select', { className: 'control error-rule-kind', attrs: { 'aria-label': `规则 ${ruleIndex + 1} 条件 ${position + 1} 匹配类型` } });
    for (const [value, label] of [['contains', '包含文本'], ['regex', '正则表达式']]) {
      const option = el('option', { text: label, attrs: { value } });
      kind.append(option);
    }
    kind.value = condition.type === 'regex' ? 'regex' : 'contains';
    kind.addEventListener('change', () => {
      condition.type = kind.value === 'regex' ? 'regex' : 'contains';
      markErrorRulesDirty();
    });
    const textarea = el('textarea', { className: 'control error-rule-condition-text', attrs: { rows: '2', spellcheck: 'false', 'aria-label': `规则 ${ruleIndex + 1} 条件 ${position + 1} 匹配内容` } });
    textarea.value = condition.text;
    textarea.addEventListener('input', () => {
      condition.text = textarea.value;
      markErrorRulesDirty();
    });
    const remove = el('button', { className: 'button compact danger', type: 'button', text: '删除条件' });
    remove.addEventListener('click', () => {
      const rule = errorRulesDraft()[ruleIndex];
      if (!rule) return;
      rule.conditions.splice(position, 1);
      markErrorRulesDirty({ rerender: true });
    });
    return el('div', { className: 'error-rule-condition', attrs: { 'data-condition': String(position) } }, [
      el('label', { className: 'field condition-kind-field' }, [el('span', { text: '匹配类型' }), kind]),
      el('label', { className: 'field' }, [el('span', { text: '匹配内容' }), textarea]),
      el('div', { className: 'row-actions' }, [remove]),
    ]);
  }

  function errorRuleCard(rule, index) {
    const explanation = Core.explainErrorRule(rule, index);
    const status = el('input', { className: 'control error-rule-status', type: 'number', attrs: { min: '100', max: '599', step: '1', required: 'required', 'aria-label': `规则 ${index + 1} 状态码` } });
    status.value = String(rule.status || '');
    status.addEventListener('input', () => {
      rule.status = Number(status.value);
      markErrorRulesDirty();
    });
    const action = el('select', { className: 'control error-rule-action', attrs: { 'aria-label': `规则 ${index + 1} 动作` } });
    for (const [value, label] of [['stop', '停止'], ['stop-and-cooldown', '停止并冷却'], ['continue', '继续'], ['continue-and-cooldown', '继续并冷却']]) {
      action.append(el('option', { text: label, attrs: { value } }));
    }
    action.value = rule.action || 'stop';
    action.addEventListener('change', () => {
      rule.action = action.value;
      markErrorRulesDirty();
    });
    const up = el('button', { className: 'button compact', type: 'button', text: '上移', attrs: { 'aria-label': `规则 ${index + 1} 上移` } });
    up.addEventListener('click', () => moveErrorRule(index, -1));
    const down = el('button', { className: 'button compact', type: 'button', text: '下移', attrs: { 'aria-label': `规则 ${index + 1} 下移` } });
    down.addEventListener('click', () => moveErrorRule(index, 1));
    const remove = el('button', { className: 'button compact danger', type: 'button', text: '删除' });
    remove.addEventListener('click', () => {
      errorRulesDraft().splice(index, 1);
      markErrorRulesDirty({ rerender: true });
    });
    const addCondition = el('button', { className: 'button compact', type: 'button', text: '新增条件' });
    addCondition.addEventListener('click', () => {
      rule.conditions.push({ type: 'contains', text: '' });
      markErrorRulesDirty({ rerender: true });
    });
    return el('section', { className: 'error-rule-card', attrs: { 'data-rule': String(index), 'aria-label': `规则 ${index + 1}` } }, [
      el('div', { className: 'error-rule-head' }, [
        el('strong', { className: 'error-rule-name', text: explanation.name }),
        el('div', { className: 'row-actions' }, [up, down, remove]),
      ]),
      el('p', { className: 'error-rule-description hint', text: explanation.detail }),
      el('div', { className: 'error-rule-fields' }, [
        el('label', { className: 'field narrow' }, [el('span', { text: '状态码' }), status]),
        el('label', { className: 'field' }, [el('span', { text: '动作' }), action]),
      ]),
      ...rule.conditions.map((condition, position) => errorRuleConditionRow(index, condition, position)),
      el('div', { className: 'row-actions' }, [addCondition]),
    ]);
  }

  function moveErrorRule(index, delta) {
    const rules = errorRulesDraft();
    const target = index + delta;
    if (target < 0 || target >= rules.length) return;
    const [rule] = rules.splice(index, 1);
    rules.splice(target, 0, rule);
    markErrorRulesDirty({ rerender: true });
  }

  function renderErrorRules() {
    const list = $('errorRulesList');
    if (!list) return;
    if (!errorRulesView.loaded) {
      list.replaceChildren(el('p', { className: 'empty', text: '正在读取错误规则…' }));
      return;
    }
    const rules = errorRulesDraft();
    if (!rules.length) {
      list.replaceChildren(el('p', { className: 'empty', text: '没有规则。此时不写入插件自有规则，由 CPA 自身的配置决定分类与调度。' }));
      syncErrorRulesControls();
      return;
    }
    list.replaceChildren(...rules.map((rule, index) => errorRuleCard(rule, index)));
    syncErrorRulesControls();
  }

  function errorRulesSyncText(view) {
    const head = Core.errorRulesSyncText(view.sync);
    const lines = [head];
    if (view.inherits_cpa) lines.push('当前没有插件自有规则，由 CPA 配置决定。');
    if (view.load_error) lines.push(view.load_error);
    return lines.join('\n');
  }

  function applyErrorRulesResponse(raw) {
    const view = Core.normalizeErrorRules(raw);
    state.errorRules = {
      rules: view.rules.map(rule => ({ status: rule.status, action: rule.action, conditions: rule.conditions.map(condition => ({ ...condition })) })),
      baseline: view.rules.map(rule => ({ status: rule.status, action: rule.action, conditions: rule.conditions.map(condition => ({ ...condition })) })),
      defaults: view.defaults,
      inherits_cpa: view.inherits_cpa,
      load_error: view.load_error,
      sync: view.sync,
    };
    errorRulesView.loaded = true;
    errorRulesView.dirty = false;
    renderErrorRules();
    return view;
  }

  async function openErrorRules() {
    const dialog = $('errorRulesDialog');
    if (dialog && typeof dialog.showModal === 'function' && !dialog.open) dialog.showModal();
    errorRulesView.generation += 1;
    const generation = errorRulesView.generation;
    const sessionGeneration = state.sessionGeneration;
    errorRulesView.loaded = false;
    renderErrorRules();
    setOptionalBusy('errorRules', true);
    message('errorRulesState', '正在读取错误规则…');
    const stillCurrent = () => generation === errorRulesView.generation && sessionGeneration === state.sessionGeneration;
    try {
      const raw = await api().errorRules();
      if (!stillCurrent()) return;
      const view = applyErrorRulesResponse(raw);
      message('errorRulesState', errorRulesSyncText(view), view.sync.failed.length || view.load_error ? 'error' : 'ok');
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      errorRulesView.loaded = false;
      renderErrorRules();
      message('errorRulesState', optionalErrorText(error), 'error');
    } finally {
      if (stillCurrent()) setOptionalBusy('errorRules', false);
    }
  }

  function closeErrorRules() {
    if (errorRulesView.busy) return;
    const dialog = $('errorRulesDialog');
    if (dialog && typeof dialog.close === 'function' && dialog.open) dialog.close();
  }

  // The action reference is a separate dialog so an operator can read what each
  // action does without leaving the editor. It is pure static text: no request,
  // no state, and it never touches the saved policy.
  function openErrorRuleHelp() {
    const dialog = $('errorRuleHelpDialog');
    if (dialog && typeof dialog.showModal === 'function' && !dialog.open) dialog.showModal();
  }

  function closeErrorRuleHelp() {
    const dialog = $('errorRuleHelpDialog');
    if (dialog && typeof dialog.close === 'function' && dialog.open) dialog.close();
  }

  function addErrorRule() {
    if (!errorRulesView.loaded) return;
    errorRulesDraft().push({ status: 429, action: 'continue-and-cooldown', conditions: [{ type: 'contains', text: '' }] });
    markErrorRulesDirty({ rerender: true });
  }

  async function submitErrorRules() {
    if (errorRulesView.busy || !errorRulesView.loaded) return;
    const problem = Core.validateErrorRules(errorRulesDraft());
    if (problem) {
      message('errorRulesState', problem, 'error');
      return;
    }
    const generation = errorRulesView.generation;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('errorRules', true);
    message('errorRulesState', '正在保存并同步到所有账号…');
    try {
      const raw = await api().saveErrorRules(errorRulesPayload());
      if (generation !== errorRulesView.generation || sessionGeneration !== state.sessionGeneration) return;
      const view = applyErrorRulesResponse(raw);
      message('errorRulesState', errorRulesSyncText(view), view.sync.failed.length ? 'error' : 'ok');
    } catch (error) {
      if (generation !== errorRulesView.generation || sessionGeneration !== state.sessionGeneration) return;
      if (optionalAuthFailure(error)) return;
      message('errorRulesState', optionalErrorText(error), 'error');
    } finally {
      if (generation === errorRulesView.generation && sessionGeneration === state.sessionGeneration) setOptionalBusy('errorRules', false);
    }
  }

  async function retryErrorRules() {
    if (errorRulesView.busy || !errorRulesView.loaded || errorRulesView.dirty) return;
    const generation = errorRulesView.generation;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('errorRules', true);
    message('errorRulesState', '正在重新同步到所有账号…');
    try {
      const raw = await api().syncErrorRules();
      if (generation !== errorRulesView.generation || sessionGeneration !== state.sessionGeneration) return;
      const view = applyErrorRulesResponse(raw);
      message('errorRulesState', errorRulesSyncText(view), view.sync.failed.length ? 'error' : 'ok');
    } catch (error) {
      if (generation !== errorRulesView.generation || sessionGeneration !== state.sessionGeneration) return;
      if (optionalAuthFailure(error)) return;
      message('errorRulesState', optionalErrorText(error), 'error');
    } finally {
      if (generation === errorRulesView.generation && sessionGeneration === state.sessionGeneration) setOptionalBusy('errorRules', false);
    }
  }

  async function resetErrorRules() {
    if (errorRulesView.busy || !errorRulesView.loaded) return;
    const generation = errorRulesView.generation;
    const sessionGeneration = state.sessionGeneration;
    setOptionalBusy('errorRules', true);
    message('errorRulesState', '正在恢复默认规则…');
    try {
      const raw = await api().resetErrorRules();
      if (generation !== errorRulesView.generation || sessionGeneration !== state.sessionGeneration) return;
      const view = applyErrorRulesResponse(raw);
      message('errorRulesState', errorRulesSyncText(view), view.sync.failed.length ? 'error' : 'ok');
    } catch (error) {
      if (generation !== errorRulesView.generation || sessionGeneration !== state.sessionGeneration) return;
      if (optionalAuthFailure(error)) return;
      message('errorRulesState', optionalErrorText(error), 'error');
    } finally {
      if (generation === errorRulesView.generation && sessionGeneration === state.sessionGeneration) setOptionalBusy('errorRules', false);
    }
  }

  function renderModelTest() {
    const facts = $('modelTestFacts');
    if (!facts) return;
    const result = state.modelTest;
    if (!result) {
      renderFacts(facts, [], '尚未测试。结果只会显示状态、耗时和 token 计数。');
      return;
    }
    const ok = result.status === 'success';
    const items = [
      { label: '结果', value: ok ? '成功' : '失败' },
      { label: '模型', value: result.model_id || '未知' },
    ];
    if (result.upstream_model) items.push({ label: '上游模型', value: result.upstream_model });
    if (result.upstream_attempted === true) items.push({ label: '上游已尝试', value: '是' });
    else if (result.upstream_attempted === false) items.push({ label: '上游已尝试', value: '否（本地拒绝）' });
    if (result.upstream_attempted === true && result.http_status !== null) items.push({ label: '上游 HTTP', value: String(result.http_status) });
    // The plugin sends the strength after the model's mapping runs, so the row
    // shows the conversion an operator configured rather than the raw input.
    if (result.reasoning_effort || result.reasoning_mapping) {
      items.push({
        label: '思考强度',
        value: Core.reasoningConversionText(result.reasoning_effort, result.reasoning_mapping),
      });
    }
    if (result.duration_ms !== null) items.push({ label: '耗时', value: `${result.duration_ms} ms` });
    if (result.usage.total_tokens !== null) items.push({ label: 'Token 总量', value: String(result.usage.total_tokens) });
    if (result.usage.prompt_tokens !== null) items.push({ label: '输入 Token', value: String(result.usage.prompt_tokens) });
    if (result.usage.completion_tokens !== null) items.push({ label: '输出 Token', value: String(result.usage.completion_tokens) });
    if (result.usage.total_tokens === null && result.usage.prompt_tokens === null && result.usage.completion_tokens === null) {
      items.push({ label: 'Usage', value: '上游未返回' });
    }
    if (result.request_kind) items.push({ label: '请求类型', value: result.request_kind });
    if (result.error_kind) items.push({ label: '错误类型', value: result.error_kind });
    renderFacts(facts, items);
    message('modelTestState', ok
      ? '测试完成：只发送了一次真实推理请求，响应正文未显示。'
      : '测试失败：未显示上游响应正文。', ok ? 'ok' : 'error');
  }

  function invalidateModelTestResult(text = '') {
    state.modelTestGeneration += 1;
    state.modelTest = null;
    renderModelTest();
    if (text) message('modelTestState', text, 'error');
  }

  async function runModelTest() {
    if (optionalBusy('modelTest')) return;
    const credential = $('modelTestCredential');
    const model = $('modelTestModel');
    const prompt = $('modelTestPrompt');
    const reasoning = $('modelTestReasoning');
    const credentialID = credential ? credential.value : '';
    const modelID = model ? model.value.trim() : '';
    const reasoningEffort = reasoning ? reasoning.value : '';
    if (!credentialID) {
      message('modelTestState', '请先选择要测试的账户。', 'error');
      return;
    }
    if (!modelID) {
      message('modelTestState', '请填写客户端模型名后再发送测试。', 'error');
      return;
    }
    const reasoningNote = reasoningEffort
      ? `，思考强度 ${Core.reasoningLabel(reasoningEffort)}（按该模型已保存的映射转换）`
      : '';
    if (typeof confirm === 'function' && !confirm(`向账户「${credentialLabel(credentialID)}」发送一次真实模型测试请求（模型 ${modelID}${reasoningNote}）？这会产生一次真实推理用量。`)) return;
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    const modelTestGeneration = state.modelTestGeneration;
    setOptionalBusy('modelTest', true);
    state.modelTest = null;
    renderModelTest();
    message('modelTestState', '正在发送模型测试（只发送一次，不自动重试）…');
    const stillCurrent = () => generation === state.optionalGeneration && sessionGeneration === state.sessionGeneration;
    // A mapping save, a status refresh or a selection change advances the model
    // test generation; a late answer for that older test is dropped silently so
    // it can neither repaint the panel nor overwrite a newer explanation.
    const resultStillCurrent = () => stillCurrent() && modelTestGeneration === state.modelTestGeneration;
    const selectionCurrent = () => (!credential || credential.value === credentialID)
      && (!model || model.value.trim() === modelID);
    try {
      const raw = await api().modelTest({
        credentialID,
        modelID,
        prompt: prompt ? prompt.value.trim() : '',
        reasoningEffort,
      });
      if (!stillCurrent()) return;
      if (!resultStillCurrent()) return;
      if (!selectionCurrent()) {
        state.modelTest = null;
        renderModelTest();
        message('modelTestState', '账户或模型选择已变化，已丢弃这次模型测试结果，请重新发送。', 'error');
        return;
      }
      const normalized = Core.normalizeModelTest(raw);
      if (normalized.credential_id !== credentialID || normalized.model_id !== modelID) {
        state.modelTest = null;
        renderModelTest();
        message('modelTestState', '模型测试响应与当前账户或模型不一致，已丢弃，请重新发送。', 'error');
        return;
      }
      state.modelTest = normalized;
      renderModelTest();
    } catch (error) {
      if (!stillCurrent()) return;
      if (optionalAuthFailure(error)) return;
      if (!resultStillCurrent()) return;
      if (!selectionCurrent()) {
        state.modelTest = null;
        renderModelTest();
        message('modelTestState', '账户或模型选择已变化，已丢弃这次模型测试结果，请重新发送。', 'error');
        return;
      }
      state.modelTest = null;
      renderModelTest();
      message('modelTestState', optionalErrorText(error), 'error');
    } finally {
      if (stillCurrent()) setOptionalBusy('modelTest', false);
    }
  }

  async function saveModels() {
    if (state.busy) return;
    // A reasoning POST in flight can return a full model list; letting a model
    // list save run meanwhile would let the later response replace the newer
    // list. Wait for the pending save instead of racing it.
    if (state.reasoningSaving) {
      message('modelState', '思考强度映射正在保存，请等保存完成后再保存模型映射。', 'error');
      return;
    }
    let models;
    try {
      models = Core.buildModelsPayload(state.mappings);
    } catch (error) {
      message('modelState', `无法保存：${error.message}`, 'error');
      return;
    }
    if (!models.length && typeof confirm === 'function' && !confirm('清空所有模型映射？保存后将从 CPA 撤回本插件的模型。')) return;
    const sessionGeneration = state.sessionGeneration;
    const sentDraftRevision = state.modelsDraftActive ? state.modelsDraftRevision : state.modelsRevision;
    const serverRevision = state.modelsRevision;
    // The backend started advertising a whole-list token after this draft was
    // begun. Sending the tokenless body would be last-writer-wins, so refuse
    // once and require an explicit overwrite confirmation.
    if (state.modelsDraftActive && !sentDraftRevision && serverRevision && state.modelsConflict !== true) {
      state.modelsConflict = true;
      state.modelsConflictRevision = serverRevision;
      state.modelsConflictSeenRevision = serverRevision;
      renderMappings();
      message('modelState', '保存已取消：服务器现在提供整表版本校验，但当前草稿是在此之前开始编辑的。草稿已保留；再次点击「保存模型映射」可按当前服务器版本确认覆盖。', 'error');
      return;
    }
    // Changing the upstream clears the stored policy by default on the backend.
    // Never carry it silently and never claim it was kept: the operator has to
    // see which rows drop their thinking-strength rules first. Rows that only
    // had their alias renamed keep them and are therefore not listed. This runs
    // before the overwrite confirmation re-pins the rows, so a row whose original
    // identity is about to fall away is still reported.
    const losingReasoning = mappingsLosingReasoning();
    if (losingReasoning.length && typeof confirm === 'function'
      && !confirm(`保存后以下模型不会沿用原来的思考强度映射，需要重新配置：${losingReasoning.join('；')}。确定继续保存？`)) {
      message('modelState', '已取消保存，草稿仍然保留。保存后这些模型不会沿用原来的思考强度映射。', '');
      return;
    }
    // A 409 (or a server revision that moved under the draft) must be confirmed
    // again by an explicit click. This is tied to the conflict flag, never to the
    // rows' content: typing the old values back does not skip the confirmation.
    if (state.modelsConflict === true) {
      if (!serverRevision || state.modelsConflictSeenRevision !== serverRevision) {
        // After a 409 the same stale token would just be rejected again. Require
        // a fresh status read first, then an explicit confirmation.
        renderMappings();
        message('modelState', '保存已取消：模型列表已在其他页面更新。请先刷新状态读取最新整表版本，核对后再点击保存确认覆盖；草稿已保留。', 'error');
        return;
      }
      if (typeof confirm === 'function'
        && !confirm('模型列表已在其他页面更新。继续保存会用当前草稿覆盖服务器上的版本。确定继续？')) {
        message('modelState', '已取消保存，草稿仍然保留。', '');
        return;
      }
      state.modelsDraftActive = true;
      state.modelsDraftRevision = serverRevision;
      state.modelsConflict = false;
      // The confirmation re-bases the draft onto the token just read, so every
      // original identity must describe that snapshot before it is sent.
      state.mappings = rebaseMappingOriginals(state.mappings);
    }
    // A whole-table save carries the rows' original identities, and the backend
    // rejects a list where two rows claim the same original row or an alias
    // reuses another row's original alias (an alias swap). Detect that here so
    // the operator gets a clear instruction instead of an unresolvable 409.
    if (modelsSaveRevision() && mappingIdentityConflict()) {
      renderMappings();
      message('modelState', '保存已取消：多行改别名后互相占用了对方的原始模型身份，服务器会拒绝整表保存。草稿已保留；请分两次保存，或先还原后重排。', 'error');
      return;
    }
    const submittedKey = mappingKey(state.mappings);
    let body;
    try {
      body = Core.buildModelsSaveBody(state.mappings, modelsSaveRevision());
    } catch (error) {
      message('modelState', `无法保存：${error.message}`, 'error');
      return;
    }
    const sentRevision = body.expected_models_revision || '';
    const submittedRows = state.mappings.map(row => ({ ...row }));
    const serverModelsGeneration = state.serverModelsGeneration;
    state.modelsSaving = true;
    setBusy(true);
    message('modelState', '正在保存模型映射…');
    try {
      const raw = await api().request('/models', { method: 'POST', body });
      if (sessionGeneration !== state.sessionGeneration) return;
      // Saving changes the host model catalog, so any displayed/in-flight model
      // test no longer describes the current mapping. The generation increments
      // exactly once, so a late response can never repaint the cleared result.
      invalidateModelTestResult();
      const saved = raw && Array.isArray(raw.models) ? raw.models : models;
      const normalized = saved.map(item => ({ id: String(item.id || '').trim(), upstream_id: String(item.upstream_id || '').trim() }))
        .filter(item => item.id && item.upstream_id);
      const responseModels = raw && Array.isArray(raw.models)
        ? Core.normalizeStatus({ models: raw.models }).models
        : null;
      // Only an echoed list describes a real snapshot to pin against. An older
      // backend that returns no list leaves the submitted rows' own pins intact.
      const savedRows = responseModels ? editorRowsFromServer(normalized) : cloneMappings(submittedRows);
      state.status.models = cloneMappings(savedRows);
      state.savedMappings = cloneMappings(savedRows);
      const responseRevision = raw && typeof raw.models_revision === 'string' ? raw.models_revision.trim() : '';
      // The server echoes each model's full stored state: keep reasoning_mapping
      // *and* reasoning_revision, so a following reasoning save still carries a
      // CAS token instead of silently falling back to last-writer-wins.
      if (responseModels) {
        // If another response replaced the list while this request was in
        // flight, its policy/revision is fresher: keep it for models that still
        // exist instead of rolling it back to this older snapshot.
        applyServerModels(responseModels,
          state.serverModelsGeneration === serverModelsGeneration ? {} : { merged: true });
      } else {
        // An older backend without the echoed list: keep any policy/revision we
        // already have for matching aliases.
        applyServerModels(normalized, { merged: true });
      }
      // A reasoning answer still in flight (in practice blocked above) belongs
      // to the old list and must not repaint the panel.
      state.reasoningGeneration += 1;
      state.reasoningSaving = false;
      const rowsStillMatchSubmission = mappingKey(state.mappings) === submittedKey;
      if (rowsStillMatchSubmission) {
        state.mappings = cloneMappings(savedRows);
        state.modelsRevision = responseRevision || state.modelsRevision;
        resetMappingsDraft(state.modelsRevision);
      } else {
        // The controls are locked during a save; this branch is a safety net for
        // programmatic edits and keeps them instead of throwing them away.
        state.modelsDraftActive = true;
        state.modelsDraftRevision = sentRevision || state.modelsRevision;
        state.modelsConflict = false;
        if (responseRevision) state.modelsRevision = responseRevision;
      }
      renderModels();
      const keptReasoning = rowsStillMatchSubmission && responseModels
        ? renamedRowsKeepingReasoning(submittedRows, responseModels)
        : [];
      message('modelState', rowsStillMatchSubmission
        ? `已保存 ${normalized.length} 个模型映射。${keptReasoning.length ? `其中 ${keptReasoning.join('、')} 只改了别名，思考强度映射已按原模型保留。` : ''}`
        : '模型映射已保存；保存期间的新编辑已保留为草稿。');
    } catch (error) {
      if (sessionGeneration !== state.sessionGeneration) return;
      if (isAuthStatus(error.status) && !isUpstreamSource(error)) {
        clearRejectedManagementAuth();
        showAuth(error.status === 401 ? '401：管理密钥无效或已过期，模型映射未保存。' : `403：${formatError(error)}`);
      } else if (isUpstreamSource(error)) {
        message('modelState', `保存失败（上游）：${formatError(error)}`, 'error');
      } else if (partialCompletion(formatError(error)) === 'mapping-persisted') {
        // The mapping is already on disk; only the host catalog refresh failed.
        // The saved baseline moves forward so the draft badge tells the truth.
        invalidateModelTestResult();
        state.savedMappings = cloneMappings(models);
        // A token we sent is now stale: the next save must re-confirm instead of
        // silently overwriting whatever revision the server has now.
        if (sentRevision) {
          state.modelsDraftActive = true;
          state.modelsDraftRevision = sentRevision;
          state.modelsConflict = true;
          state.modelsConflictRevision = state.modelsRevision;
          state.modelsConflictSeenRevision = '';
        }
        renderMappings();
        message('modelState', `模型映射已保存，但宿主刷新模型目录失败：${formatError(error)}。可先刷新状态确认，再重试保存或启用账户触发刷新。`, 'error');
      } else if (error.status === 409 && !sentRevision && !state.modelsRevision) {
        // An older backend has no whole-list token, so a 409 is not a CAS
        // conflict here. Keep the previous wording instead of arming a retry
        // gate that no status read could ever satisfy.
        message('modelState', '保存失败：模型或规则已变更，请检查提交内容后重试。', 'error');
      } else if (error.status === 409) {
        // Keep the submitted rows exactly as they are. The conflict must be
        // resolved by an explicit second confirmation, never by reverting.
        state.modelsDraftActive = true;
        state.modelsDraftRevision = sentRevision || state.modelsRevision;
        state.modelsConflict = true;
        state.modelsConflictRevision = sentRevision || state.modelsRevision;
        // A status refresh may already have supplied a token different from the
        // one just rejected; that counts as reading the new revision. Otherwise
        // the next click waits for a fresh status read instead of spinning on
        // the same stale token.
        state.modelsConflictSeenRevision = (serverRevision && serverRevision !== sentRevision)
          ? serverRevision
          : '';
        renderMappings();
        message('modelState', '保存失败：模型列表已在其他页面更新。提交内容已保留；请先刷新状态读取最新整表版本，再次点击「保存模型映射」并确认后才会覆盖服务器版本。', 'error');
      } else {
        message('modelState', `保存失败：${formatError(error)}`, 'error');
      }
    } finally {
      // A response from a replaced session must not release the busy slot the
      // new session may already own.
      if (sessionGeneration === state.sessionGeneration) {
        state.modelsSaving = false;
        setBusy(false);
      }
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
    noteMappingsEdited();
    renderMappings();
    message('modelState', '已手动添加一行映射，填写对外别名和上游模型 ID 后保存。', '');
  }

  // Explicitly discard the draft; status refreshes never do this on their own.
  function revertMappings() {
    if (state.busy || !mappingsDirty()) return;
    state.mappings = cloneMappings(state.savedMappings);
    resetMappingsDraft();
    renderModels();
    message('modelState', '已还原为最近一次保存的模型映射。', '');
  }

  // --- thinking-strength mapping -------------------------------------------

  // A model is identified by both halves of its mapping, so editing an alias to
  // point at a different upstream is treated as a different model (and the
  // policy has to be saved again for the new upstream).
  function reasoningModelKey(model) {
    return `${String((model && model.id) || '').trim()}\u0000${String((model && model.upstream_id) || '').trim()}`;
  }

  function selectedReasoningModel() {
    const key = state.reasoningModelKey;
    return state.serverModels.find(model => reasoningModelKey(model) === key) || null;
  }

  // A draft wins while one exists; otherwise the saved policy, and for a model
  // that has never been saved the pre-fill derived from its upstream ID. Draft
  // rules keep the operator's row order so editing never reshuffles the table.
  function currentReasoningMapping(model) {
    if (!model) return { enabled: false, rules: [] };
    const draft = state.reasoningDrafts.get(reasoningModelKey(model));
    if (draft) return { enabled: draft.enabled, rules: draft.rules.map(rule => ({ from: rule.from, to: rule.to })) };
    if (model.reasoning_mapping) return Core.normalizeReasoningMapping(model.reasoning_mapping);
    return Core.defaultReasoningMapping(model.upstream_id);
  }

  function reasoningBaseline(model) {
    if (!model) return { enabled: false, rules: [] };
    return model.reasoning_mapping
      ? Core.normalizeReasoningMapping(model.reasoning_mapping)
      : Core.defaultReasoningMapping(model.upstream_id);
  }

  function reasoningDirty() {
    const model = selectedReasoningModel();
    if (!model) return false;
    return Core.reasoningKey(currentReasoningMapping(model)) !== Core.reasoningKey(reasoningBaseline(model));
  }

  // A draft is never silently deleted just because its model left the current
  // list: an upstream change or a temporary removal must not destroy unsaved
  // rules. Orphans stay in the map (so a re-added model gets its draft back) and
  // are surfaced as copyable text by reportOrphanedReasoningDrafts.
  function reconcileReasoningDrafts() {
    for (const [key, draft] of [...state.reasoningDrafts]) {
      const model = state.serverModels.find(item => reasoningModelKey(item) === key);
      if (!model) continue;
      if (Core.reasoningKey(draft) === Core.reasoningKey(reasoningBaseline(model))) {
        state.reasoningDrafts.delete(key);
      }
    }
  }

  function reasoningDraftText(draft) {
    return draft.rules.length
      ? draft.rules.map(rule => `${Core.reasoningLabel(rule.from)}→${Core.reasoningLabel(rule.to)}`).join('、')
      : '（空表）';
  }

  // Models that were present before a refresh but are gone (or now point at a
  // different upstream) keep any draft they had. Show the rules as plain text so
  // the operator can copy them instead of losing the edit silently.
  function reportOrphanedReasoningDrafts(previousModels) {
    const previous = new Set((Array.isArray(previousModels) ? previousModels : []).map(reasoningModelKey));
    const live = new Set(state.serverModels.map(reasoningModelKey));
    const lines = [];
    for (const [key, draft] of state.reasoningDrafts) {
      if (!previous.has(key) || live.has(key)) continue;
      const [id, upstream] = String(key).split('\u0000');
      lines.push(`${id || '（无别名）'} → ${upstream || '（无上游）'}：${reasoningDraftText(draft)}`);
    }
    if (!lines.length) return;
    message('reasoningState', `模型映射已变化，以下未保存的思考强度草稿已保留，可复制后重新添加模型：${lines.join('；')}。`, 'error');
  }

  // The revision a draft is pinned to. It is captured on the first edit and
  // survives later edits and status refreshes; only saving or an explicit
  // overwrite/discard moves it.
  function reasoningDraftRevision(model) {
    const draft = state.reasoningDrafts.get(reasoningModelKey(model));
    if (draft) return draft.baseRevision || '';
    return String((model && model.reasoning_revision) || '');
  }

  function storeReasoningDraft(model, mapping) {
    const key = reasoningModelKey(model);
    const existing = state.reasoningDrafts.get(key);
    state.reasoningDrafts.set(key, {
      enabled: mapping.enabled,
      rules: mapping.rules,
      baseRevision: existing ? existing.baseRevision : String(model.reasoning_revision || ''),
      conflict: existing ? existing.conflict === true : false,
    });
  }

  function updateReasoningRows(mutate) {
    const model = selectedReasoningModel();
    if (!model) return;
    const draft = currentReasoningMapping(model);
    mutate(draft);
    storeReasoningDraft(model, draft);
    renderReasoning();
  }

  function setReasoningEnabled(enabled) {
    updateReasoningRows(draft => { draft.enabled = Boolean(enabled); });
    message('reasoningState', '');
  }

  function addReasoningRule() {
    const model = selectedReasoningModel();
    if (!model) return;
    const next = Core.nextReasoningSource(currentReasoningMapping(model).rules);
    if (!next) {
      message('reasoningState', '所有请求强度都有规则了，无需再添加。', '');
      return;
    }
    // A new rule is an identity mapping: it changes nothing until the operator
    // picks a target, so adding a row can never silently rewrite traffic.
    updateReasoningRows(draft => { draft.rules.push({ from: next, to: Core.defaultReasoningTarget(next) }); });
    message('reasoningState', '');
  }

  function removeReasoningRule(index) {
    updateReasoningRows(draft => { draft.rules.splice(index, 1); });
    message('reasoningState', '');
  }

  function changeReasoningRule(index, field, value) {
    const model = selectedReasoningModel();
    if (!model) return;
    const draft = currentReasoningMapping(model);
    if (!draft.rules[index]) return;
    if (field === 'from' && draft.rules.some((rule, other) => other !== index && rule.from === value)) {
      // Two rules for one strength would be rejected by the plugin, so the
      // duplicate is refused here instead of failing at save time.
      renderReasoning();
      message('reasoningState', `请求强度「${Core.reasoningLabel(value)}」已有规则，请先修改或删除原有规则。`, 'error');
      return;
    }
    draft.rules[index][field] = value;
    storeReasoningDraft(model, draft);
    renderReasoning();
    message('reasoningState', '');
  }

  // Restores the vendor table for the selected model, enabled or not, without
  // writing anything: the operator still has to save.
  function resetReasoningTable() {
    const model = selectedReasoningModel();
    if (!model) return;
    updateReasoningRows(draft => {
      // A non-DeepSeek model has no vendor table to restore, so this yields the
      // empty table instead of silently writing DeepSeek rules onto it.
      draft.rules = Core.defaultReasoningMapping(model.upstream_id).rules.map(rule => ({ from: rule.from, to: rule.to }));
    });
    message('reasoningState', '已恢复默认表，点击「保存映射」后生效。', '');
  }

  function reasoningSelect(value, options, onChange, disabled = false) {
    const node = el('select', { className: 'control', disabled });
    for (const option of options) node.append(el('option', { value: option.value, text: option.label }));
    node.value = value;
    node.addEventListener('change', event => onChange(event.target.value));
    return node;
  }

  function renderReasoningRows(rules, model, busy = false) {
    const body = $('reasoningRows');
    if (!body) return;
    const rows = [];
    if (!model) {
      rows.push(el('tr', {}, [el('td', { className: 'effort-empty', attrs: { colspan: '3' }, text: '先保存模型映射，再配置思考强度。' })]));
    } else {
      if (!rules.length) {
        rows.push(el('tr', {}, [el('td', { className: 'effort-empty', attrs: { colspan: '3' }, text: '暂无规则，所有强度直接透传' })]));
      }
      rules.forEach((rule, index) => {
        rows.push(el('tr', {}, [
          el('td', {}, [reasoningSelect(rule.from, Core.reasoningSourceOptions(), value => changeReasoningRule(index, 'from', value), busy)]),
          el('td', {}, [reasoningSelect(rule.to, Core.reasoningTargetOptions(), value => changeReasoningRule(index, 'to', value), busy)]),
          el('td', {}, [el('button', { className: 'button compact danger', type: 'button', text: '删除', disabled: busy, on: () => removeReasoningRule(index) })]),
        ]));
      });
    }
    body.replaceChildren(...rows);
  }

  function syncReasoningControls() {
    const model = selectedReasoningModel();
    const mapping = currentReasoningMapping(model);
    const busy = state.busy || state.reasoningSaving;
    const enabled = $('reasoningEnabled');
    if (enabled) {
      enabled.checked = mapping.enabled;
      enabled.disabled = busy || !model;
    }
    const modelSelect = $('reasoningModel');
    if (modelSelect) modelSelect.disabled = busy || !state.serverModels.length;
    const add = $('addReasoningRule');
    if (add) add.disabled = busy || !model || !Core.nextReasoningSource(mapping.rules);
    const reset = $('resetReasoningTable');
    if (reset) reset.disabled = busy || !model;
    const save = $('saveReasoning');
    if (save) save.disabled = busy || !model;
    // Saving the model list must not race a pending reasoning POST whose answer
    // could put the old model list back.
    const saveModelsButton = $('saveModels');
    if (saveModelsButton) saveModelsButton.disabled = busy;
    const dirty = $('reasoningDirty');
    if (dirty) dirty.hidden = !model || !reasoningDirty();
  }

  function renderReasoning() {
    const select = $('reasoningModel');
    if (select) {
      const options = state.serverModels.map(model => el('option', {
        value: reasoningModelKey(model),
        text: `${model.id} → ${model.upstream_id}`,
      }));
      if (!options.length) options.push(el('option', { value: '', text: '暂无模型', disabled: true }));
      select.replaceChildren(...options);
      const previous = state.reasoningModelKey;
      const keep = state.serverModels.some(model => reasoningModelKey(model) === previous);
      state.reasoningModelKey = keep ? previous : (state.serverModels.length ? reasoningModelKey(state.serverModels[0]) : '');
      select.value = state.reasoningModelKey;
    }
    const model = selectedReasoningModel();
    renderReasoningRows(currentReasoningMapping(model).rules, model, state.busy || state.reasoningSaving);
    syncReasoningControls();
    if (!model) message('reasoningState', '');
  }

  function selectReasoningModel(key) {
    state.reasoningModelKey = String(key || '');
    renderReasoning();
    message('reasoningState', '');
  }

  // The model test's strength picker. "未指定" stays the default so a test with
  // no strength behaves exactly as it did before this control existed.
  function populateReasoningInputs() {
    const select = $('modelTestReasoning');
    if (!select) return;
    const options = Core.reasoningSourceOptions().filter(option => option.value !== 'unset');
    select.replaceChildren(
      el('option', { value: '', text: '未指定' }),
      ...options.map(option => el('option', { value: option.value, text: option.label })),
    );
  }

  // The reasoning panel reads the server's model list only, so a reasoning save
  // refreshes that list without touching the model editor's saved baseline.
  // Keeping policy and revision is essential: dropping either would make the
  // next save unable to detect a competing tab.
  function mergeServerModels(current, incoming) {
    const existing = new Map((Array.isArray(current) ? current : []).map(model => [reasoningModelKey(model), model]));
    return (Array.isArray(incoming) ? incoming : []).map(model => existing.get(reasoningModelKey(model)) || model);
  }

  function applyServerModels(models, options = {}) {
    const previousServerModels = state.serverModels;
    state.serverModels = options.merged === true
      ? mergeServerModels(state.serverModels, models)
      : (Array.isArray(models) ? models : []);
    state.serverModelsGeneration += 1;
    reconcileReasoningDrafts();
    reportOrphanedReasoningDrafts(previousServerModels);
  }

  async function saveReasoning() {
    const model = selectedReasoningModel();
    if (!model || state.reasoningSaving || state.busy) return;
    const key = reasoningModelKey(model);
    const draft = state.reasoningDrafts.get(key) || null;
    const serverRevision = String(model.reasoning_revision || '');
    // The first attempt always sends the revision the draft was edited against,
    // so the backend can reject a competing tab. Only after such a rejection
    // (conflict) does a second explicit click move the draft onto the revision
    // just observed; that is the operator's overwrite decision.
    if (draft && draft.conflict === true && draft.baseRevision && serverRevision
      && draft.baseRevision !== serverRevision) {
      if (typeof confirm === 'function'
        && !confirm(`该模型的思考强度已在其他页面更新。继续保存会用当前草稿覆盖服务器上的版本。确定继续？`)) {
        message('reasoningState', '已取消保存，草稿仍然保留。', '');
        return;
      }
      draft.baseRevision = serverRevision;
      state.reasoningDrafts.set(key, draft);
    }
    const generation = state.reasoningGeneration;
    const sessionGeneration = state.sessionGeneration;
    let payload;
    try {
      payload = Core.buildReasoningPayload(model, currentReasoningMapping(model), reasoningDraftRevision(model));
    } catch (error) {
      message('reasoningState', `无法保存：${error.message}`, 'error');
      return;
    }
    state.reasoningSaving = true;
    // Every row is rebuilt disabled so a click mid-save cannot be silently lost.
    renderReasoning();
    message('reasoningState', '正在保存思考强度映射…');
    try {
      const raw = await api().saveReasoningMapping(payload);
      if (generation !== state.reasoningGeneration || sessionGeneration !== state.sessionGeneration) return;
      applyServerModels(raw && Array.isArray(raw.models) ? raw.models : null);
      if (raw && typeof raw.models_revision === 'string' && raw.models_revision.trim()) {
        // A reasoning save can move the whole-list token too. Keeping it makes
        // the next model-list save CAS-checked instead of tokenless.
        state.modelsRevision = raw.models_revision.trim();
      }
      state.reasoningDrafts.delete(key);
      // The policy is request-shaping state: a result produced under the old
      // rules no longer describes what the plugin will now send.
      invalidateModelTestResult();
      renderReasoning();
      message('reasoningState', `已保存 ${model.id} 的思考强度映射${payload.reasoning_mapping.enabled ? '（已启用）' : '（当前关闭，不生效）'}。`);
    } catch (error) {
      if (generation !== state.reasoningGeneration || sessionGeneration !== state.sessionGeneration) return;
      if (isAuthStatus(error.status) && !isUpstreamSource(error)) {
        clearRejectedManagementAuth();
        showAuth(error.status === 401 ? '401：管理密钥无效或已过期，映射未保存。' : `403：${formatError(error)}`);
      } else if (error.status === 409) {
        // A 409 means the server refused the CAS; never claim success and never
        // drop the draft. A refresh surfaces the newer revision, and the next
        // save asks before overwriting it.
        if (draft) {
          draft.conflict = true;
          state.reasoningDrafts.set(key, draft);
          renderReasoning();
        }
        message('reasoningState', '保存失败：模型或规则已变更，草稿已保留。请刷新状态核对后再保存。', 'error');
      } else if (error.status === 404) {
        message('reasoningState', '保存失败：模型已不存在，请刷新状态。', 'error');
      } else {
        message('reasoningState', `保存失败：${formatError(error)}`, 'error');
      }
    } finally {
      // The saver owns the lock even when a status refresh made the answer stale,
      // but a session switch already cleared it and owns the new page state.
      if (sessionGeneration === state.sessionGeneration) {
        state.reasoningSaving = false;
        renderReasoning();
      }
    }
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
    populateReasoningInputs();
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
    const reasoningModel = $('reasoningModel');
    if (reasoningModel) reasoningModel.addEventListener('change', () => selectReasoningModel(reasoningModel.value));
    const reasoningEnabled = $('reasoningEnabled');
    if (reasoningEnabled) reasoningEnabled.addEventListener('change', () => setReasoningEnabled(reasoningEnabled.checked));
    const addReasoning = $('addReasoningRule');
    if (addReasoning) addReasoning.addEventListener('click', addReasoningRule);
    const resetReasoning = $('resetReasoningTable');
    if (resetReasoning) resetReasoning.addEventListener('click', resetReasoningTable);
    const saveReasoningButton = $('saveReasoning');
    if (saveReasoningButton) saveReasoningButton.addEventListener('click', saveReasoning);
    const select = $('modelCredential');
    if (select) select.addEventListener('change', () => selectCredential(select.value));
    const search = $('upstreamSearch');
    if (search) search.addEventListener('input', () => filterUpstream(search.value));
    const selectVisible = $('selectVisible');
    if (selectVisible) selectVisible.addEventListener('click', () => selectVisibleUpstream(true));
    const clearVisible = $('clearVisible');
    if (clearVisible) clearVisible.addEventListener('click', () => selectVisibleUpstream(false));
    for (const item of TABS) {
      const tab = $(item.button);
      if (!tab) continue;
      tab.addEventListener('click', event => activateTab(tabFromEvent(event)));
      tab.addEventListener('keydown', handleTabKey);
    }
    const refreshLogs = $('refreshLogs');
    if (refreshLogs) refreshLogs.addEventListener('click', () => loadRequestLogs({ reset: true }));
    const refreshStats = $('refreshStats');
    if (refreshStats) refreshStats.addEventListener('click', loadRequestStats);
    const clearLogs = $('clearLogs');
    if (clearLogs) clearLogs.addEventListener('click', clearRequestLogs);
    const themeToggle = $('themeToggle');
    if (themeToggle) themeToggle.addEventListener('click', cycleThemePreference);
    const logsPrev = $('logsPrev');
    if (logsPrev) logsPrev.addEventListener('click', previousLogsPage);
    const logsNext = $('logsNext');
    if (logsNext) logsNext.addEventListener('click', nextLogsPage);
    const logsFilterStatus = $('logsFilterStatus');
    if (logsFilterStatus) logsFilterStatus.addEventListener('change', () => loadRequestLogs({ reset: true }));
    const logsFilterCredential = $('logsFilterCredential');
    if (logsFilterCredential) logsFilterCredential.addEventListener('change', () => loadRequestLogs({ reset: true }));
    const logsFilterModel = $('logsFilterModel');
    if (logsFilterModel) {
      logsFilterModel.addEventListener('change', () => loadRequestLogs({ reset: true }));
      logsFilterModel.addEventListener('keydown', event => {
        if (event.key === 'Enter') loadRequestLogs({ reset: true });
      });
    }
    const logsFilterAttempted = $('logsFilterAttempted');
    if (logsFilterAttempted) logsFilterAttempted.addEventListener('change', () => loadRequestLogs({ reset: true }));
    const logsFilterLimit = $('logsFilterLimit');
    if (logsFilterLimit) logsFilterLimit.addEventListener('change', () => loadRequestLogs({ reset: true }));
    const loadConfigButton = $('loadConfig');
    if (loadConfigButton) loadConfigButton.addEventListener('click', loadConfig);
    const logSettingsForm = $('logSettingsForm');
    if (logSettingsForm) logSettingsForm.addEventListener('submit', saveConfig);
    const pauseSettingsForm = $('pauseSettingsForm');
    if (pauseSettingsForm) pauseSettingsForm.addEventListener('submit', savePauseConfig);
    const loadRuntimeButton = $('loadRuntime');
    if (loadRuntimeButton) loadRuntimeButton.addEventListener('click', loadRuntime);
    const runTestButton = $('runModelTest');
    if (runTestButton) runTestButton.addEventListener('click', runModelTest);
    const openRules = $('openErrorRules');
    if (openRules) openRules.addEventListener('click', openErrorRules);
    const closeRules = $('closeErrorRules');
    if (closeRules) closeRules.addEventListener('click', closeErrorRules);
    const addRule = $('addErrorRule');
    if (addRule) addRule.addEventListener('click', addErrorRule);
    const saveRules = $('saveErrorRules');
    if (saveRules) saveRules.addEventListener('click', submitErrorRules);
    const retryRules = $('retryErrorRules');
    if (retryRules) retryRules.addEventListener('click', retryErrorRules);
    const resetRules = $('resetErrorRules');
    if (resetRules) resetRules.addEventListener('click', resetErrorRules);
    const ruleHelp = $('openErrorRuleHelp');
    if (ruleHelp) ruleHelp.addEventListener('click', openErrorRuleHelp);
    const closeRuleHelp = $('closeErrorRuleHelp');
    if (closeRuleHelp) closeRuleHelp.addEventListener('click', closeErrorRuleHelp);
    const modelTestCredential = $('modelTestCredential');
    if (modelTestCredential) modelTestCredential.addEventListener('change', () => {
      invalidateModelTestResult('账户选择已变化，之前的模型测试结果已清除。');
    });
    const modelTestModel = $('modelTestModel');
    if (modelTestModel) modelTestModel.addEventListener('input', () => {
      invalidateModelTestResult('模型选择已变化，之前的模型测试结果已清除。');
    });
  }

  async function submitAuth(event) {
    if (event) event.preventDefault();
    // A stale busy flag from the previous session (for example a model POST that
    // returned 401) must never lock the operator out of re-authenticating.
    if (state.authSubmitting) return;
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
    const generation = state.optionalGeneration;
    const sessionGeneration = state.sessionGeneration;
    const candidate = { headers: Auth.candidateHeaders(key, type) };
    // Re-validating the same key keeps the current session: its drafts and CAS
    // tokens are still valid. A different key starts a fresh session.
    const sameIdentity = sameAuthIdentity(state.verified && state.verified.headers, candidate.headers);
    state.candidate = candidate;
    state.authSubmitting = true;
    // Authentication owns its own flag instead of the shared busy lock, so a
    // stale model write can never keep the operator from re-verifying.
    setDisabled('authSubmit', true);
    message('authState', '正在验证管理密钥…');
    try {
      const status = await api().status();
      if (generation !== state.optionalGeneration || sessionGeneration !== state.sessionGeneration) return;
      const hadDrafts = mappingsDirty() || state.modelsDraftActive || state.reasoningDrafts.size > 0;
      // Persist only after CPA accepted the key.
      Auth.saveAuth(key, type, remember);
      state.verified = { headers: candidate.headers, source: 'manual' };
      state.candidate = null;
      if (keyInput) keyInput.value = '';
      if (!sameIdentity) {
        // A management-key switch owns a fresh session: old drafts, CAS tokens
        // and any unsubmitted account secret in the form must go with it.
        resetManagementData();
        resetAccountForm();
      }
      applyStatus(status);
      setAuthCollapsed(true);
      markConnected('manual');
      message('modelState', !sameIdentity && hadDrafts
        ? '管理密钥已切换，未保存的模型映射/思考强度草稿已清除。'
        : '');
      message('accountState', '');
      message('authState', '管理密钥验证成功，已开始读取账户和模型。', 'ok');
    } catch (error) {
      if (generation !== state.optionalGeneration || sessionGeneration !== state.sessionGeneration) return;
      showAuth(isManagementAuthError(error)
        ? (error.status === 401
          ? '401：管理密钥未通过验证，请检查后重试。'
          : '403：管理请求被拒绝，请确认管理密钥和来源 IP。')
        : `验证失败：${formatError(error)}`);
    } finally {
      state.authSubmitting = false;
      setDisabled('authSubmit', false);
    }
  }

  function clearAuth() {
    try { Auth.clearAuth(); } catch { /* Ignore blocked storage. */ }
    state.verified = null;
    state.candidate = null;
    state.authSubmitting = false;
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
    initTheme();
    renderMeta();
    activateTab('overview');
    setAuthCollapsed(false);
    resetAccountForm();
    renderAccounts();
    renderModels();
    renderOverview();
    renderRequestLogs();
    renderRequestStats();
    renderConfig();
    renderRuntime();
    renderErrorRules();
    renderModelTest();
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
      resumeQuotaPause,
      recheckQuotaPause,
      selectAccount,
      selectCredential,
      fetchModels,
      saveModels,
      addMapping,
      revertMappings,
      selectReasoningModel,
      setReasoningEnabled,
      addReasoningRule,
      removeReasoningRule,
      changeReasoningRule,
      resetReasoningTable,
      saveReasoning,
      populateReasoningInputs,
      refreshQuota,
      checkModelCoverage,
      filterUpstream,
      selectVisibleUpstream,
      toggleAuthPanel,
      activateTab,
      handleTabKey,
      loadRequestLogs,
      loadRequestStats,
      clearRequestLogs,
      nextLogsPage,
      previousLogsPage,
      loadConfig,
      saveConfig,
      savePauseConfig,
      loadRuntime,
      openErrorRules,
      closeErrorRules,
      openErrorRuleHelp,
      closeErrorRuleHelp,
      submitErrorRules,
      retryErrorRules,
      resetErrorRules,
      addErrorRule,
      runModelTest,
      cycleThemePreference,
      applyThemePreference,
    },
    render: {
      applyStatus,
      renderMeta,
      renderAccounts,
      renderCredentialOptions,
      renderUpstream,
      renderModelCoverage,
      renderMappings,
      renderModels,
      renderReasoning,
      reasoningDirty,
      currentReasoningMapping,
      showAuth,
      loadStatus,
      resetAccountForm,
      setAuthCollapsed,
      syncFetchButton,
      syncModelTools,
      updateDirtyBadge,
      renderOverview,
      renderRequestLogs,
      renderRequestStats,
      renderConfig,
      renderRuntime,
      renderErrorRules,
      applyErrorRulesResponse,
      errorRulesSyncText,
      renderModelTest,
      applyThemePreference,
      cycleThemePreference,
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
