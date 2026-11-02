/* Minimal management API client. No credentials are ever written to URLs. */
(() => {
  'use strict';

  class APIError extends Error {
    constructor(status, message, body) {
      super(message);
      this.name = 'APIError';
      this.status = status;
      this.body = body;
    }
  }

  function buildPath(base, path, query) {
    const root = String(base || '').replace(/\/+$/, '');
    const suffix = String(path || '').startsWith('/') ? path : `/${path || ''}`;
    let result = root + suffix;
    if (query && typeof query === 'object') {
      const params = new URLSearchParams();
      for (const [name, value] of Object.entries(query)) {
        if (value === undefined || value === null || value === '') continue;
        params.set(name, String(value));
      }
      const encoded = params.toString();
      if (encoded) result += `?${encoded}`;
    }
    return result;
  }

  function errorMessage(status, payload, raw) {
    if (globalThis.OpenDesignCore && globalThis.OpenDesignCore.parseAPIError) {
      return globalThis.OpenDesignCore.parseAPIError(status, payload, raw);
    }
    return `HTTP ${status}`;
  }

  function createClient(options = {}) {
    const fetchImpl = options.fetchImpl || globalThis.fetch?.bind(globalThis);
    const resolveAuth = typeof options.resolveAuth === 'function' ? options.resolveAuth : () => ({ headers: {} });
    if (typeof fetchImpl !== 'function') throw new Error('fetch 不可用');

    async function request(path, settings = {}) {
      const method = String(settings.method || 'GET').toUpperCase();
      const auth = await resolveAuth();
      const headers = { Accept: 'application/json', ...(auth && auth.headers ? auth.headers : {}), ...(settings.headers || {}) };
      const init = { method, headers, credentials: 'same-origin', cache: 'no-store' };
      if (settings.body !== undefined) {
        headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(settings.body);
      }
      if (settings.signal) init.signal = settings.signal;
      const response = await fetchImpl(buildPath(options.base, path, settings.query), init);
      const raw = await response.text();
      let payload = null;
      if (raw) {
        try { payload = JSON.parse(raw); } catch { payload = raw; }
      }
      if (!response.ok) throw new APIError(response.status, errorMessage(response.status, payload, raw), payload);
      return payload;
    }

    return {
      request,
      status: () => request('/status'),
      addCredential: (label, apiKey) => request('/credentials', { method: 'POST', body: { label, api_key: apiKey } }),
      // CPA management routes match exact METHOD + path, so credential ids travel
      // as query/body fields rather than path segments.
      deleteCredential: id => request('/credentials', { method: 'DELETE', query: { id } }),
      updateCredential: (id, body = {}) => request('/credentials', {
        method: 'POST',
        body: { ...body, id: String(id) },
      }),
      upstreamModels: credentialID => request('/upstream-models', { query: { credential_id: credentialID } }),
      saveModels: models => request('/models', { method: 'POST', body: { models } }),
    };
  }

  const api = { createClient, buildPath, APIError };
  Object.defineProperty(api, '__test', { value: { errorMessage }, enumerable: false });
  globalThis.OpenDesignAPI = api;
})();
