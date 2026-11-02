/* Management-key compatibility for the OpenDesign console.
 *
 * Reuses the public CPA / CPAMP "remember password" login (cli-proxy-auth) when it
 * belongs to this exact CPA endpoint. A key typed into the console is validated
 * first and then kept in this tab, or in local storage only when the user asks.
 * Management keys are never placed in URLs, query strings or logs.
 */
(() => {
  'use strict';

  const API_SUFFIX = /\/v(?:0|8)\/management\/open-design\/?$/;
  const RESOURCE_CONSOLE = /^(.*)\/v(?:0|8)\/resource\/plugins\/open-design\/console\/?$/;
  const MANAGEMENT_CONSOLE = /^(.*)\/v(?:0|8)\/management\/open-design\/console\.html\/?$/;

  function pageRoot() {
    if (typeof location === 'undefined') return null;
    const match = location.pathname.match(RESOURCE_CONSOLE) || location.pathname.match(MANAGEMENT_CONSOLE);
    return match ? match[1].replace(/\/+$/, '') : null;
  }

  function endpoint() {
    if (typeof location === 'undefined') return null;
    let path = '';
    const root = pageRoot();
    if (root !== null) path = `${root}/v0/management/open-design`;
    else {
      const meta = typeof document !== 'undefined' && document.querySelector
        ? document.querySelector('meta[name="cpa-api-base"]')
        : null;
      if (!meta || typeof meta.content !== 'string' || !meta.content.trim()) return null;
      let configured;
      try { configured = new URL(meta.content, location.origin); } catch { return null; }
      if (configured.origin !== location.origin || configured.username || configured.password || configured.search || configured.hash) return null;
      path = configured.pathname;
    }
    path = path.replace(/\/+$/, '');
    if (!API_SUFFIX.test(path)) return null;
    return { origin: location.origin, root: path.replace(API_SUFFIX, ''), apiPath: path };
  }

  function sessionKey() {
    const own = endpoint();
    return own ? `opendesign:management-auth:${own.origin}${own.root}` : null;
  }

  function authHeaders(login) {
    if (!login || typeof login.key !== 'string' || !login.key.trim()) return {};
    return login.type === 'x-management-key'
      ? { 'X-Management-Key': login.key.trim() }
      : { Authorization: `Bearer ${login.key.trim()}` };
  }

  function readManual(persistent = false) {
    const name = sessionKey();
    if (!name) return null;
    try {
      const value = (persistent ? localStorage : sessionStorage).getItem(name);
      if (!value) return null;
      const parsed = JSON.parse(value);
      if (!parsed || typeof parsed.key !== 'string' || !parsed.key.trim()) return null;
      return { key: parsed.key.trim(), type: parsed.type === 'x-management-key' ? 'x-management-key' : 'bearer' };
    } catch { return null; }
  }

  function decodeSaved(value) {
    if (typeof value !== 'string' || !value) return null;
    const v2 = value.startsWith('enc::v2::');
    if (!v2 && !value.startsWith('enc::v1::')) {
      try { return JSON.parse(value)?.state || null; } catch { return null; }
    }
    try {
      const bytes = Uint8Array.from(atob(value.slice(9)), char => char.charCodeAt(0));
      const masks = v2
        ? [`cli-proxy-api-webui::secure-storage|${location.host}`, `cli-proxy-api-webui::secure-storage|v2|${location.host}`]
        : [`cli-proxy-api-webui::secure-storage|${location.host}|${navigator.userAgent}`];
      for (const candidate of masks) {
        try {
          const mask = new TextEncoder().encode(candidate);
          const plain = new TextDecoder().decode(bytes.map((byte, index) => byte ^ mask[index % mask.length]));
          const parsed = JSON.parse(plain);
          if (parsed && typeof parsed === 'object') return parsed.state || parsed;
        } catch { /* Try the next compatible local format. */ }
      }
    } catch { /* Malformed saved login must not break the console. */ }
    return null;
  }

  function readSaved() {
    const own = endpoint();
    if (!own) return null;
    try {
      if (localStorage.getItem('isLoggedIn') !== 'true') return null;
      const saved = decodeSaved(localStorage.getItem('cli-proxy-auth'));
      if (!saved || saved.rememberPassword !== true || typeof saved.managementKey !== 'string' || !saved.managementKey.trim()) return null;
      const server = new URL(saved.apiBase);
      const cleanPath = value => value.replace(/\/+$/, '').replace(/\/v(?:0|8)\/management$/, '');
      if (server.username || server.password || server.search || server.hash || server.origin !== own.origin) return null;
      if (cleanPath(server.pathname) !== own.root) return null;
      return { key: saved.managementKey.trim(), type: 'bearer' };
    } catch { return null; }
  }

  function resolveHeaders() {
    for (const [login, source] of [[readManual(), 'session'], [readManual(true), 'remembered'], [readSaved(), 'cpa-login']]) {
      const headers = authHeaders(login);
      if (Object.keys(headers).length) return { headers, source };
    }
    return { headers: {}, source: '' };
  }

  function candidateHeaders(key, type) {
    return authHeaders({ key: typeof key === 'string' ? key : '', type });
  }

  function saveAuth(key, type, remember = false) {
    const name = sessionKey();
    if (!name || typeof key !== 'string' || !key.trim()) return false;
    const value = JSON.stringify({ key: key.trim(), type: type === 'x-management-key' ? type : 'bearer' });
    try { sessionStorage.setItem(name, value); } catch { /* The tab still holds the key in memory. */ }
    try {
      if (remember) localStorage.setItem(name, value);
      else localStorage.removeItem(name);
    } catch { /* A blocked store must not prevent using the other. */ }
    return true;
  }

  function clearAuth(rejectedHeaders) {
    const name = sessionKey();
    if (!name) return;
    for (const persistent of [false, true]) {
      try {
        const storage = persistent ? localStorage : sessionStorage;
        const stored = authHeaders(readManual(persistent));
        const rejected = rejectedHeaders && typeof rejectedHeaders === 'object' ? rejectedHeaders : null;
        const matches = !rejected || Object.keys(stored).some(header => stored[header] === rejected[header]);
        if (matches) storage.removeItem(name);
      } catch { /* Clearing a blocked store must not break the page. */ }
    }
  }

  function rememberEnabled() {
    return Object.keys(authHeaders(readManual(true))).length > 0;
  }

  const api = {
    endpoint,
    authHeaders,
    candidateHeaders,
    resolveHeaders,
    saveAuth,
    clearAuth,
    rememberEnabled,
  };
  Object.defineProperty(api, '__test', {
    value: { readManual, readSaved, decodeSaved, sessionKey, pageRoot },
    enumerable: false,
  });
  globalThis.OpenDesignAuth = api;
})();
