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
        credentials.push({
          id,
          label: text(item.label) || id,
          disabled: item.disabled === true,
          billing_configured: item.billing_configured === true,
          workspace_id: text(item.workspace_id),
        });
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

  function aliasForUpstream(upstreamID) {
    const value = text(upstreamID);
    return value ? `open-design-${value}` : '';
  }

  function addUpstream(models, upstreamID) {
    const value = text(upstreamID);
    const rows = (Array.isArray(models) ? models : []).map(model => ({ ...model }));
    if (!value || hasUpstream(rows, value)) return rows;
    rows.push({ id: aliasForUpstream(value), upstream_id: value });
    return rows;
  }

  function removeUpstream(models, upstreamID) {
    const wanted = text(upstreamID);
    return (Array.isArray(models) ? models : [])
      .filter(model => text(model.upstream_id) !== wanted)
      .map(model => ({ ...model }));
  }

  function filterUpstream(models, query) {
    const rows = (Array.isArray(models) ? models : []).map(model => ({ ...model }));
    const needle = text(query).toLowerCase();
    if (!needle) return rows;
    return rows.filter(model =>
      text(model.id).toLowerCase().includes(needle) ||
      text(model.owned_by).toLowerCase().includes(needle)
    );
  }

  function addUpstreams(models, upstreamIDs) {
    let rows = Array.isArray(models) ? models : [];
    for (const upstreamID of Array.isArray(upstreamIDs) ? upstreamIDs : []) {
      rows = addUpstream(rows, upstreamID);
    }
    return rows;
  }

  function removeUpstreams(models, upstreamIDs) {
    let rows = Array.isArray(models) ? models : [];
    for (const upstreamID of Array.isArray(upstreamIDs) ? upstreamIDs : []) {
      rows = removeUpstream(rows, upstreamID);
    }
    return rows;
  }

  // --- quota helpers -------------------------------------------------------

  const QUOTA_STATES = ['not_configured', 'needs_refresh', 'not_requested', 'available', 'partial', 'stale', 'error'];

  function quotaState(value) {
    const state = text(value);
    return QUOTA_STATES.includes(state) ? state : '';
  }

  // Money and credits stay strings end to end: the backend may send values that
  // do not fit a JS Number, and a rounded balance must never be shown as if it
  // were exact. null/absent means unknown, which the UI renders as such.
  function decimalText(value) {
    if (typeof value === 'string') return value.trim();
    if (typeof value === 'number' && Number.isFinite(value)) return String(value);
    return '';
  }

  function creditText(value) {
    const raw = decimalText(value);
    return /^[0-9]+$/.test(raw) ? raw : '';
  }

  function quotaError(value) {
    if (!value || typeof value !== 'object') return null;
    const kind = text(value.kind);
    const message = text(value.message);
    if (!kind && !message) return null;
    return { kind, message };
  }

  function quotaTimestamp(value) {
    const stamp = text(value);
    return stamp && !Number.isNaN(Date.parse(stamp)) ? stamp : '';
  }

  function normalizeForecast(value) {
    const source = value && typeof value === 'object' ? value : {};
    const state = text(source.state);
    const seconds = source.seconds_to_exhaust;
    return {
      state,
      seconds_to_exhaust: typeof seconds === 'number' && Number.isSafeInteger(seconds) && seconds >= 0 ? seconds : null,
      exhausts_at: quotaTimestamp(source.exhausts_at),
      basis: text(source.basis),
    };
  }

  function normalizeWindow(value) {
    if (!value || typeof value !== 'object') return null;
    return {
      policyId: text(value.policyId),
      durationSeconds: Number.isSafeInteger(value.durationSeconds) && value.durationSeconds > 0 ? value.durationSeconds : null,
      resetMode: text(value.resetMode),
      usedCredits: creditText(value.usedCredits),
      limitCredits: creditText(value.limitCredits),
      remainingCredits: creditText(value.remainingCredits),
      windowStart: quotaTimestamp(value.windowStart),
      resetsAt: quotaTimestamp(value.resetsAt),
      forecast: normalizeForecast(value.forecast),
    };
  }

  function normalizeComponent(value) {
    const source = value && typeof value === 'object' ? value : {};
    return {
      state: quotaState(source.state),
      fetched_at: quotaTimestamp(source.fetched_at),
      data: source.data && typeof source.data === 'object' ? source.data : null,
      error: quotaError(source.error),
    };
  }

  function normalizeQuota(value) {
    const data = value && typeof value === 'object' ? value : {};
    const wallet = normalizeComponent(data.wallet);
    if (wallet.data) {
      wallet.data = {
        balanceUsd: decimalText(wallet.data.balanceUsd),
        updatedAt: quotaTimestamp(wallet.data.updatedAt),
      };
    }
    const funding = normalizeComponent(data.funding);
    if (funding.data) {
      const plan = funding.data.codingPlan && typeof funding.data.codingPlan === 'object' ? funding.data.codingPlan : null;
      funding.data = {
        balanceUsd: decimalText(funding.data.balanceUsd),
        funding: text(funding.data.funding),
        modelCovered: typeof funding.data.modelCovered === 'boolean' ? funding.data.modelCovered : null,
        fundingCheck: text(funding.data.fundingCheck),
        codingPlan: plan && {
          tier: text(plan.tier),
          windows: (Array.isArray(plan.windows) ? plan.windows : [])
            .map(normalizeWindow)
            .filter(Boolean),
        },
      };
    }
    return {
      credential_id: text(data.credential_id),
      billing_configured: data.billing_configured === true,
      workspace_id: text(data.workspace_id),
      state: quotaState(data.state),
      fetched_at: quotaTimestamp(data.fetched_at),
      next_refresh_at: quotaTimestamp(data.next_refresh_at),
      wallet,
      funding,
    };
  }

  // used/limit as basis points, computed with BigInt so a credits string larger
  // than Number.MAX_SAFE_INTEGER still yields an exact meter percentage.
  function creditsPercent(used, limit) {
    const usedRaw = creditText(used);
    const limitRaw = creditText(limit);
    if (!usedRaw || !limitRaw) return null;
    try {
      const limitValue = BigInt(limitRaw);
      if (limitValue <= 0n) return null;
      let bps = (BigInt(usedRaw) * 10000n) / limitValue;
      if (bps < 0n) bps = 0n;
      if (bps > 10000n) bps = 10000n;
      const whole = bps / 100n;
      const fraction = (bps % 100n).toString().padStart(2, '0');
      return { basisPoints: Number(bps), text: `${whole}.${fraction}%` };
    } catch { return null; }
  }

  function formatCredits(value) {
    const raw = creditText(value);
    if (!raw) return '';
    return raw.replace(/^0+(?=\d)/, '').replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  }

  function formatUSD(value) {
    const raw = decimalText(value);
    return raw ? `$${raw}` : '';
  }

  function formatDuration(seconds) {
    if (!Number.isSafeInteger(seconds) || seconds < 0) return '';
    if (seconds < 60) return `${seconds} 秒`;
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return `${minutes} 分钟`;
    const hours = Math.round(minutes / 60);
    if (hours < 48) return `${hours} 小时`;
    return `${Math.round(hours / 24)} 天`;
  }

  function formatTime(value) {
    const stamp = quotaTimestamp(value);
    if (!stamp) return '';
    const date = new Date(stamp);
    const pad = number => String(number).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
  }

  // Forecast copy follows the backend states. It is deliberately an estimate
  // based on recent consumption: no "N days left" promise and no pricing math.
  function forecastView(forecast) {
    const value = forecast && typeof forecast === 'object' ? forecast : {};
    switch (text(value.state)) {
      case 'insufficient_samples':
        return { text: '样本不足，暂不预测（需至少两次间隔 60 秒以上的查询）', tone: 'muted' };
      case 'rolling_window':
        return { text: '滚动窗口，不预测耗尽时间', tone: 'muted' };
      case 'estimated': {
        const span = formatDuration(value.seconds_to_exhaust);
        const at = formatTime(value.exhausts_at);
        const parts = ['按最近消费速度估算'];
        if (span && at) parts.push(`约 ${span}后达到当前窗口上限（${at}）`);
        else if (span) parts.push(`约 ${span}后达到当前窗口上限`);
        else if (at) parts.push(`预计在 ${at} 达到当前窗口上限`);
        return { text: `${parts.join('，')}；仅为估算，不改变模型调度`, tone: 'warn' };
      }
      case 'resets_before_exhaustion': {
        const span = formatDuration(value.seconds_to_exhaust);
        return { text: span
          ? `按最近消费速度估算，预计 ${span}后耗尽，但会先到窗口重置时间；仅为估算`
          : '预计先到窗口重置时间，暂不给出耗尽时间', tone: 'muted' };
      }
      case 'beyond_horizon':
        return { text: '按最近消费速度估算，预计远超一年，不显示具体耗尽时间', tone: 'muted' };
      default:
        return { text: '', tone: 'muted' };
    }
  }

  const api = {
    DEFAULT_BASE_URL,
    parseAPIError,
    normalizeStatus,
    normalizeUpstreamModels,
    normalizeQuota,
    creditsPercent,
    formatCredits,
    formatUSD,
    formatDuration,
    formatTime,
    forecastView,
    buildModelsPayload,
    hasUpstream,
    aliasForUpstream,
    addUpstream,
    removeUpstream,
    filterUpstream,
    addUpstreams,
    removeUpstreams,
  };
  Object.defineProperty(api, '__test', { value: { text }, enumerable: false });
  globalThis.OpenDesignCore = api;
})();
