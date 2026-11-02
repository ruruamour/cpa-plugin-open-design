/* Pure helpers shared by the OpenDesign console and the Node tests. */
(() => {
  'use strict';

  const DEFAULT_BASE_URL = 'https://amr-link.open-design.ai/v1';

  function text(value) {
    return value === undefined || value === null ? '' : String(value).trim();
  }

  function parseAPIError(status, payload, raw) {
    let detail = '';
    if (payload && typeof payload === 'object') {
      const error = payload.error;
      if (typeof error === 'string') detail = error;
      else if (error && typeof error === 'object') detail = text(error.message || error.error || error.code);
      if (!detail) detail = text(payload.message);
    } else if (typeof payload === 'string') {
      detail = payload;
    }
    if (!detail) detail = text(raw);
    detail = detail.replace(/\s+/g, ' ');
    if (detail.length > 300) detail = detail.slice(0, 300) + '…';
    return `HTTP ${status}${detail ? `：${detail}` : ''}`;
  }

  function normalizeStatus(value) {
    const data = value && typeof value === 'object' ? value : {};
    const credentials = [];
    if (Array.isArray(data.credentials)) {
      for (const item of data.credentials) {
        if (!item || typeof item !== 'object') continue;
        const id = text(item.id);
        if (!id) continue;
        credentials.push({ id, label: text(item.label) || id, disabled: item.disabled === true });
      }
    }
    const models = [];
    if (Array.isArray(data.models)) {
      for (const item of data.models) {
        if (!item || typeof item !== 'object') continue;
        const id = text(item.id);
        const upstreamID = text(item.upstream_id);
        if (!id || !upstreamID) continue;
        models.push({ id, upstream_id: upstreamID });
      }
    }
    return {
      version: text(data.version),
      base_url: text(data.base_url) || DEFAULT_BASE_URL,
      credentials,
      models,
    };
  }

  function normalizeUpstreamModels(value) {
    const source = Array.isArray(value) ? value : (value && Array.isArray(value.data) ? value.data : []);
    const result = [];
    const seen = new Set();
    for (const item of source) {
      if (!item || typeof item !== 'object') continue;
      const id = text(item.id);
      if (!id || seen.has(id)) continue;
      seen.add(id);
      result.push({ id, owned_by: text(item.owned_by) });
    }
    return result;
  }

  function buildModelsPayload(rows) {
    const models = [];
    const seen = new Set();
    for (const row of Array.isArray(rows) ? rows : []) {
      const id = text(row && row.id);
      const upstreamID = text(row && row.upstream_id);
      if (!id && !upstreamID) continue;
      if (!id || !upstreamID) throw new Error('模型别名和上游模型 ID 都不能为空');
      if (seen.has(id)) throw new Error(`模型别名重复：${id}`);
      seen.add(id);
      models.push({ id, upstream_id: upstreamID });
    }
    return models;
  }

  function hasUpstream(models, upstreamID) {
    const wanted = text(upstreamID);
    return (Array.isArray(models) ? models : []).some(model => text(model.upstream_id) === wanted);
  }

  function addUpstream(models, upstreamID) {
    const value = text(upstreamID);
    const rows = (Array.isArray(models) ? models : []).map(model => ({ ...model }));
    if (!value || hasUpstream(rows, value)) return rows;
    rows.push({ id: value, upstream_id: value });
    return rows;
  }

  function removeUpstream(models, upstreamID) {
    const wanted = text(upstreamID);
    return (Array.isArray(models) ? models : [])
      .filter(model => text(model.upstream_id) !== wanted)
      .map(model => ({ ...model }));
  }

  const api = {
    DEFAULT_BASE_URL,
    parseAPIError,
    normalizeStatus,
    normalizeUpstreamModels,
    buildModelsPayload,
    hasUpstream,
    addUpstream,
    removeUpstream,
  };
  Object.defineProperty(api, '__test', { value: { text }, enumerable: false });
  globalThis.OpenDesignCore = api;
})();
