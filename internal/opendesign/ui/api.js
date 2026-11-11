/* Minimal management API client. No credentials are ever written to URLs. */
(() => {
  'use strict';

  class APIError extends Error {
    constructor(status, message, body) {
      super(message);
      this.name = 'APIError';
      this.status = status;
      this.body = body;
      const meta = errorMetadata(body);
      this.source = meta.source;
      this.code = meta.code;
    }
  }

  // Newer backends tag an error with where it came from (for example
  // source="upstream" when the OpenDesign key itself was rejected) and with the
  // upstream error code. Older backends send neither, so both fields stay empty
  // and callers keep their previous behaviour.
  function stringField(value) {
    return typeof value === 'string' && value.trim() ? value.trim() : '';
  }

  function errorMetadata(payload) {
    const meta = { source: '', code: '' };
    if (!payload || typeof payload !== 'object') return meta;
    const error = payload.error;
    if (error && typeof error === 'object') {
      meta.source = stringField(error.source) || stringField(payload.source);
      meta.code = stringField(error.code) || stringField(payload.code);
      return meta;
    }
    meta.source = stringField(payload.source);
    meta.code = stringField(payload.code);
    return meta;
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
      // A caller may override the auth for exactly one request. The console uses
      // that to validate a newly typed management key without ever letting the
      // unverified candidate become the identity of ordinary requests.
      const auth = typeof settings.resolveAuth === 'function'
        ? await settings.resolveAuth()
        : await resolveAuth();
      // Snapshot at the same time as the headers, not when the reply arrives.
      const authGeneration = Number.isInteger(auth && auth.authGeneration) ? auth.authGeneration : null;
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
      if (!response.ok) {
        const error = new APIError(response.status, errorMessage(response.status, payload, raw), payload);
        // Local correlation only: never send this token (or a key) over the wire.
        // It travels with this particular request, including follow-up requests.
        if (authGeneration !== null) error.authGeneration = authGeneration;
        throw error;
      }
      return payload;
    }

    return {
      request,
      status: (options = {}) => request('/status', options),
      // extra carries the optional billing fields (control_key / workspace_id) on
      // create; older callers pass only a label and key.
      addCredential: (label, apiKey, extra = {}) => request('/credentials', {
        method: 'POST',
        body: { label, api_key: apiKey, ...extra },
      }),
      // CPA management routes match exact METHOD + path, so credential ids travel
      // as query/body fields rather than path segments.
      deleteCredential: id => request('/credentials', { method: 'DELETE', query: { id } }),
      updateCredential: (id, body = {}) => request('/credentials', {
        method: 'POST',
        body: { ...body, id: String(id) },
      }),
      upstreamModels: credentialID => request('/upstream-models', { query: { credential_id: credentialID } }),
      // Cached read unless refresh is explicitly requested: loading the page must
      // never hit the upstream billing API on its own.
      quota: (credentialID, options = {}) => request('/quota', {
        query: { credential_id: credentialID, refresh: options.refresh ? 1 : '' },
      }),
      modelCoverage: (credentialID, modelID, options = {}) => request('/model-coverage', {
        query: { credential_id: credentialID, model_id: modelID, refresh: options.refresh ? 1 : '' },
      }),
      // Whole-list save. `expectedRevision` is optional and purely additive:
      // callers that pass only the list keep sending exactly `{ models }` with
      // no CAS, so nothing here can pretend an old caller is concurrency-safe.
      // An empty/absent revision is omitted rather than sent as an empty string,
      // because an empty token is not a valid comparison value.
      // The console itself builds the full body directly (it additionally needs
      // per-row original identities and local validation), so this helper stays
      // the simple entry point for other callers.
      saveModels: (models, expectedRevision) => {
        const body = { models };
        const revision = typeof expectedRevision === 'string' ? expectedRevision.trim() : '';
        if (revision) body.expected_models_revision = revision;
        return request('/models', { method: 'POST', body });
      },
      // Quota pause is plugin-owned: it never changes the operator's `disabled`
      // choice. Resume and recheck are explicit single-credential actions.
      quotaPauses: () => request('/quota-pause'),
      resumeQuotaPause: credentialID => request('/quota-pause/resume', { method: 'POST', body: { credential_id: credentialID } }),
      recheckQuotaPause: credentialID => request('/quota-pause/recheck', { method: 'POST', body: { credential_id: credentialID } }),
      // The thinking-strength policy for one model. The body carries the model
      // identity plus the full rule table, so a stale tab can be rejected by the
      // backend instead of silently overwriting a newer edit.
      saveReasoningMapping: payload => request('/models/reasoning', { method: 'POST', body: payload }),
      // Optional observability/runtime routes. Every one of them is called from
      // an explicit operator action only: never while loading the page, switching
      // tabs or rendering. Query values are constrained by the backend.
      requestLogs: (options = {}) => request('/request-logs', {
        query: {
          limit: options.limit,
          offset: options.offset,
          status: options.status,
          model: options.model,
          credential_id: options.credentialID,
          attempted: options.attempted,
          q: options.search,
        },
      }),
      requestStats: (options = {}) => request('/request-stats', { query: { since: options.since, until: options.until } }),
      clearRequestLogs: () => request('/request-logs', { method: 'DELETE' }),
      config: () => request('/config'),
      // Request-scoped retry-scheduling policy. Reading it happens only when the
      // operator opens the error-rules dialog; nothing here runs on page load.
      errorRules: () => request('/error-rules'),
      saveErrorRules: rules => request('/error-rules', { method: 'PUT', body: { rules } }),
      syncErrorRules: () => request('/error-rules/sync', { method: 'POST', body: {} }),
      resetErrorRules: () => request('/error-rules/reset', { method: 'POST', body: {} }),
      // Only plugin-owned log settings are accepted by the backend; upstream,
      // proxy and routing stay host-owned and read-only.
      // The plugin only accepts its own sections: request-log policy and the
      // quota-pause policy. Host-owned scheduling is never sent from here.
      updateConfig: (logs, pause) => request('/config', { method: 'POST', body: pause ? { logs, pause } : { logs } }),
      updatePauseConfig: pause => request('/config', { method: 'POST', body: { pause } }),
      runtime: () => request('/runtime'),
      // Read-only host scheduling projection for the account cards. The page
      // asks for it only while the credentials tab is open, alongside status.
      scheduling: () => request('/scheduling'),
      modelTest: input => request('/model-test', {
        method: 'POST',
        body: {
          credential_id: String(input && input.credentialID || ''),
          model_id: String(input && input.modelID || ''),
          prompt: input && input.prompt ? String(input.prompt) : undefined,
          // Omitted when the operator did not pick a strength, so the request
          // body stays byte-identical to what it was before this field existed.
          reasoning_effort: input && input.reasoningEffort ? String(input.reasoningEffort) : undefined,
        },
      }),
    };
  }

  const api = { createClient, buildPath, APIError };
  Object.defineProperty(api, '__test', { value: { errorMessage, errorMetadata }, enumerable: false });
  globalThis.OpenDesignAPI = api;
})();
