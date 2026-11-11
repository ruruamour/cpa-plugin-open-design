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

  function normalizeQuotaPause(value) {
    const data = value && typeof value === 'object' ? value : null;
    if (!data) return null;
    const pause = {
      reason: safeCode(data.reason, 40),
      source: safeCode(data.source, 40),
      resume_at: safeTimestamp(data.resume_at),
      // A historical reset/retry hint from the upstream error, not a live
      // prediction: quota can become available before or after this time.
      estimate_at: safeTimestamp(data.estimate_at),
      blocked_at: safeTimestamp(data.blocked_at),
      model_id: safeIdentity(data.model_id, 512),
      multiple_models: data.multiple_models === true,
      // Older servers did not expose the complete model set; be conservative.
      models_ambiguous: data.models_ambiguous === true || (data.models_ambiguous !== false && data.multiple_models === true),
      recheck_status: safeCode(data.recheck_status, 40),
      manual_disabled: data.manual_disabled === true,
      remaining_seconds: safeInteger(data.remaining_seconds, 0, 31536000),
      // The plugin reports this once the recorded deadline has passed. It means
      // "a recheck is due", never "the account is enabled again".
      awaiting_recheck: data.awaiting_recheck === true,
      error: shortText(data.error, 200),
    };
    return pause;
  }

  function quotaPauseRemaining(pause, now = Date.now()) {
    if (!pause) return null;
    const at = Date.parse(pause.resume_at || '');
    if (!Number.isFinite(at)) return null;
    const remaining = Math.floor((at - now) / 1000);
    return remaining > 0 ? remaining : 0;
  }

  function quotaPauseWaitingText(pause, now) {
    const estimate = quotaPauseEstimateRemaining(pause, now);
    return estimate === null ? '等待复查' : `等待复查 · 上游原提示 ${formatDuration(estimate)}后重置或重试`;
  }

  function quotaPauseText(pause, now = Date.now()) {
    if (!pause) return '';
    // Reaching the recorded deadline never means "resumed". The plugin marks the
    // pause as due for an official recheck instead (and it may still need a
    // manual resume), so that state reads as waiting for the recheck even if the
    // recorded deadline is missing or the local clock disagrees. The old upstream
    // reset hint stays visible: it is not a recovery promise.
    if (pause.awaiting_recheck === true) return `额度暂停（${quotaPauseWaitingText(pause, now)}）`;
    const remaining = quotaPauseRemaining(pause, now);
    if (remaining === null) return '额度暂停';
    if (remaining <= 0) return `额度暂停（${quotaPauseWaitingText(pause, now)}）`;
    const estimate = quotaPauseEstimateRemaining(pause, now);
    if (estimate !== null && estimate > remaining) {
      return `额度暂停（${formatDuration(remaining)}后复查，上游原提示 ${formatDuration(estimate)}后重置或重试）`;
    }
    return `额度暂停（约 ${formatDuration(remaining)}后复查）`;
  }

  // The backend accepts a plan rate as a positive decimal string of at most 32
  // characters. Mirror that exact contract so a legal fractional rate survives
  // normalization, and a numeric, signed, exponent or zero value can never
  // become a divisor.
  function rateText(value) {
    if (typeof value !== 'string') return '';
    const raw = value.trim();
    if (!raw || raw.length > 32 || !/^[0-9]+(?:\.[0-9]+)?$/.test(raw)) return '';
    return /[1-9]/.test(raw) ? raw : '';
  }

  // Exact decimal -> rational, so money can be added and rounded with BigInt.
  function decimalRatio(value) {
    const raw = rateText(value);
    if (!raw) return null;
    const fraction = raw.split('.')[1] || '';
    return { numerator: BigInt(raw.replace('.', '')), denominator: 10n ** BigInt(fraction.length) };
  }

  function gcd(left, right) {
    let a = left < 0n ? -left : left;
    let b = right < 0n ? -right : right;
    while (b) { const next = a % b; a = b; b = next; }
    return a;
  }

  function addRatios(left, right) {
    if (!left) return right;
    if (!right) return left;
    // Use the least common denominator before multiplying, especially when
    // many accounts share the same rate; reduction keeps intermediate BigInts
    // small without rounding any account's fraction.
    const common = gcd(left.denominator, right.denominator);
    const leftScale = right.denominator / common;
    const rightScale = left.denominator / common;
    const numerator = left.numerator * leftScale + right.numerator * rightScale;
    const denominator = left.denominator * leftScale;
    const divisor = gcd(numerator, denominator);
    return divisor > 1n
      ? { numerator: numerator / divisor, denominator: denominator / divisor }
      : { numerator, denominator };
  }

  // credits / (rate credits per $1) * 100, kept as an exact cents fraction.
  function usdCentsRatio(credits, creditsPerUSD) {
    const amount = creditText(credits);
    const rate = decimalRatio(creditsPerUSD);
    if (!amount || !rate) return null;
    return { numerator: BigInt(amount) * 100n * rate.denominator, denominator: rate.numerator };
  }

  function formatUSDCents(cents) {
    return `${cents / 100n}.${String(cents % 100n).padStart(2, '0')}`;
  }

  function roundUSDCents(ratio) {
    // Positive half-up rounding: floor(numerator / denominator + 1/2).
    return (ratio.numerator * 2n + ratio.denominator) / (ratio.denominator * 2n);
  }

  function creditsToUSD(credits, creditsPerUSD) {
    const ratio = usdCentsRatio(credits, creditsPerUSD);
    return ratio ? formatUSDCents(roundUSDCents(ratio)) : '';
  }

  function formatCreditsUSD(credits, creditsPerUSD) {
    const usd = creditsToUSD(credits, creditsPerUSD);
    return usd ? `≈ $${usd}` : '';
  }

  // Sum each account's exact cents value, then round the total once. A missing
  // or malformed rate contributes credits but no dollars.
  function sumCreditsUSD(items) {
    let total = null;
    let converted = 0;
    let accounts = 0;
    for (const item of Array.isArray(items) ? items : []) {
      const amount = creditText(item && item.credits);
      if (!amount) continue;
      accounts += 1;
      const ratio = usdCentsRatio(amount, item && item.creditsPerUSD);
      if (!ratio) continue;
      converted += 1;
      total = addRatios(total, ratio);
    }
    return { usd: total ? formatUSDCents(roundUSDCents(total)) : '', converted, accounts };
  }

  // Compact copy for the account card. Paused accounts are moved to the bottom
  // of the list, so the badge must explain both why and for how long without
  // opening the quota details or querying the upstream again.
  function quotaPauseTitle(pause) {
    if (!pause) return '';
    return pause.reason === 'quota_exhausted' ? '额度耗尽 · 自动暂停' : '额度暂停';
  }

  // Seconds until the historical upstream reset/retry hint. It is not evidence
  // that quota remains exhausted until then; only a fresh recheck can tell.
  function quotaPauseEstimateRemaining(pause, now = Date.now()) {
    if (!pause || !pause.estimate_at) return null;
    const at = Date.parse(pause.estimate_at || '');
    if (!Number.isFinite(at)) return null;
    const remaining = Math.floor((at - now) / 1000);
    return remaining > 0 ? remaining : null;
  }

  function quotaPauseCountdown(pause, now = Date.now()) {
    if (!pause) return '';
    if (pause.awaiting_recheck === true) return quotaPauseWaitingText(pause, now);
    const remaining = quotaPauseRemaining(pause, now);
    if (remaining === null) return '';
    if (remaining <= 0) return quotaPauseWaitingText(pause, now);
    // The upstream hint is from the old error, not a verified recovery time.
    const estimate = quotaPauseEstimateRemaining(pause, now);
    if (estimate !== null && estimate > remaining) {
      return `${formatDuration(remaining)}后复查 · 上游原提示 ${formatDuration(estimate)}后重置或重试`;
    }
    return `约 ${formatDuration(remaining)}后复查`;
  }

  // Short card labels retain the distinction between an official reset time,
  // a plugin recheck deadline and an old upstream hint. Full timestamps remain
  // available in the expanded details and title attributes.
  function formatQuotaTime(value, now = Date.now()) {
    const full = formatTime(value);
    if (!full) return '';
    return full.slice(0, 4) === String(new Date(now).getFullYear())
      ? full.slice(5).replaceAll('-', '/') : full.replaceAll('-', '/');
  }

  function quotaPauseCompact(pause, now = Date.now()) {
    if (!pause) return '';
    const remaining = quotaPauseRemaining(pause, now);
    if (pause.awaiting_recheck === true || remaining === 0) return '等待复查';
    if (remaining === null) return '复查时间待确认';
    // Minute precision keeps a badge compact and avoids second-by-second jumps.
    return `约 ${formatDuration(Math.max(60, Math.ceil(remaining / 60) * 60))}后复查`;
  }

  function quotaPauseSummary(pause, now = Date.now()) {
    const title = quotaPauseTitle(pause);
    if (!title) return '';
    const countdown = quotaPauseCountdown(pause, now);
    return countdown ? `${title} · ${countdown}` : title;
  }

  const SCHEDULING_STATES = {
    available: true,
    credential_cooldown: true,
    restricted: true,
    host_disabled: true,
    manual_disabled: true,
    quota_paused: true,
    unknown: true,
  };

  function normalizeScheduling(value) {
    const data = value && typeof value === 'object' ? value : {};
    const host = data.host && typeof data.host === 'object' ? data.host : {};
    const accounts = [];
    if (Array.isArray(data.accounts)) {
      for (const item of data.accounts) {
        if (!item || typeof item !== 'object') continue;
        const id = text(item.id);
        if (!id) continue;
        const state = SCHEDULING_STATES[item.state] ? item.state : 'unknown';
        accounts.push({
          id,
          state,
          quota_paused: item.quota_paused === true,
          host: item.host && typeof item.host === 'object' ? {
            disabled: item.host.disabled === true,
            unavailable: item.host.unavailable === true,
            status: safeCode(item.host.status, 64),
            message: shortText(item.host.message, 200),
            next_retry_after: safeTimestamp(item.host.next_retry_after),
          } : null,
        });
      }
    }
    return {
      observed_at: safeTimestamp(data.observed_at),
      host_available: host.available === true,
      host_note: shortText(host.note, 200),
      accounts,
    };
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
          billing_revision: text(item.billing_revision),
          workspace_id: text(item.workspace_id),
          // The quota pause is a separate, plugin-owned flag. `disabled` stays
          // the operator's own choice, so the console never conflates the two.
          quota_paused: item.quota_paused === true,
          effective_disabled: item.effective_disabled === true,
          quota_pause: normalizeQuotaPause(item.quota_pause),
          credits_per_usd: rateText(item.credits_per_usd),
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
        // An absent policy stays absent so the console can tell "never configured"
        // (pre-fill the vendor table) from "saved as empty" (show the stored table).
        const stored = item.reasoning_mapping === undefined || item.reasoning_mapping === null
          ? null
          : normalizeReasoningMapping(item.reasoning_mapping);
        // An optional opaque revision lets the console ask the backend to reject
        // a save that was written against an older model/rule table. A backend
        // that does not send it leaves the field empty and the save omits it.
        models.push({
          id,
          upstream_id: upstreamID,
          reasoning_mapping: stored,
          reasoning_revision: text(item.reasoning_revision),
        });
      }
    }
    return {
      version: text(data.version),
      base_url: text(data.base_url) || DEFAULT_BASE_URL,
      // Optional whole-list optimistic-lock token. A backend that does not send
      // it leaves the field empty; a backend that starts sending it lets the
      // console reject a save written against an older model list.
      models_revision: text(data.models_revision),
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
    return buildModelsSaveBody(rows, '').models;
  }

  // The POST /models body. expected_models_revision is only attached when the
  // server advertised a token; an older backend keeps receiving exactly the
  // same body as before, so it is never asked to validate an unknown field.
  //
  // original_id/original_upstream_id describe the row this editor started from.
  // They are sent only together with a whole-list token: the backend resolves
  // them against that exact snapshot, so an alias-only rename can keep its
  // reasoning mapping. A row without a captured original identity (a new row)
  // never gets one here, and a half-filled pair is dropped rather than sent,
  // because the backend rejects a partial identity outright.
  function buildModelsSaveBody(rows, expectedRevision) {
    const revision = text(expectedRevision);
    const models = [];
    const seen = new Set();
    for (const row of Array.isArray(rows) ? rows : []) {
      const id = text(row && row.id);
      const upstreamID = text(row && row.upstream_id);
      if (!id && !upstreamID) continue;
      if (!id || !upstreamID) throw new Error('模型别名和上游模型 ID 都不能为空');
      if (seen.has(id)) throw new Error(`模型别名重复：${id}`);
      seen.add(id);
      const model = { id, upstream_id: upstreamID };
      const originalID = text(row && row.original_id);
      const originalUpstreamID = text(row && row.original_upstream_id);
      if (revision && originalID && originalUpstreamID) {
        model.original_id = originalID;
        model.original_upstream_id = originalUpstreamID;
      }
      models.push(model);
    }
    const body = { models };
    if (revision) body.expected_models_revision = revision;
    return body;
  }

  // --- thinking-strength mapping -------------------------------------------

  // The closed sets the plugin accepts. The source list doubles as the render
  // order, so the table never reshuffles between renders.
  const REASONING_SOURCES = ['unset', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'];
  const REASONING_TARGETS = ['unset', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'];

  function reasoningLabel(value) {
    const normalized = text(value);
    return !normalized || normalized === 'unset' ? '未设置' : normalized;
  }

  function defaultReasoningRules() {
    return [
      { from: 'unset', to: 'unset' },
      { from: 'none', to: 'none' },
      { from: 'minimal', to: 'low' },
      { from: 'low', to: 'low' },
      { from: 'medium', to: 'high' },
      { from: 'high', to: 'high' },
      { from: 'xhigh', to: 'high' },
      { from: 'max', to: 'max' },
      { from: 'ultra', to: 'max' },
    ];
  }

  // A DeepSeek upstream is pre-filled from the vendor table; every other model
  // starts empty. Either way the mapping starts disabled: it is opt-in per model.
  function defaultReasoningMapping(upstreamID) {
    const upstream = text(upstreamID).toLowerCase();
    return { enabled: false, rules: upstream.includes('deepseek') ? defaultReasoningRules() : [] };
  }

  // Reads whatever the server sent into the editor shape. Anything outside the
  // closed sets, or a repeated source, is dropped rather than rendered: the
  // table can then never offer a value the plugin would refuse to save.
  function normalizeReasoningMapping(raw) {
    const source = raw && typeof raw === 'object' ? raw : {};
    const rules = [];
    const seen = new Set();
    for (const rule of Array.isArray(source.rules) ? source.rules : []) {
      const from = text(rule && rule.from).toLowerCase();
      const to = text(rule && rule.to).toLowerCase();
      if (!REASONING_SOURCES.includes(from) || !REASONING_TARGETS.includes(to) || seen.has(from)) continue;
      seen.add(from);
      rules.push({ from, to });
    }
    rules.sort((left, right) => REASONING_SOURCES.indexOf(left.from) - REASONING_SOURCES.indexOf(right.from));
    return { enabled: source.enabled === true, rules };
  }

  // A stable identity for the dirty check and for draft keys.
  function reasoningKey(mapping) {
    const view = normalizeReasoningMapping(mapping);
    return JSON.stringify([view.enabled, view.rules.map(rule => [rule.from, rule.to])]);
  }

  function reasoningSourceOptions() {
    return REASONING_SOURCES.map(value => ({ value, label: reasoningLabel(value) }));
  }

  function reasoningTargetOptions() {
    return REASONING_TARGETS.map(value => ({ value, label: reasoningLabel(value) }));
  }

  // A newly added rule starts as an identity mapping, but a source that is not
  // itself a legal target (ultra) has to fall back to a value the target picker
  // actually offers, otherwise the row could never be saved.
  function defaultReasoningTarget(source) {
    const value = text(source).toLowerCase();
    return REASONING_TARGETS.includes(value) ? value : 'max';
  }

  // The next source that has no rule yet, so "添加规则" never creates a
  // duplicate the save route would reject. Returns an empty string when every
  // source is already mapped.
  function nextReasoningSource(rules) {
    const used = new Set((Array.isArray(rules) ? rules : []).map(rule => text(rule && rule.from)));
    return REASONING_SOURCES.find(value => !used.has(value)) || '';
  }

  // The save payload. Anything the plugin would reject is reported instead of
  // dropped: silently discarding a rule would save a policy the operator did not
  // write. Rule order is preserved so the request is exactly the edited table.
  function buildReasoningPayload(model, mapping, expectedRevision) {
    const raw = mapping && typeof mapping === 'object' ? mapping : {};
    const sources = new Set(REASONING_SOURCES);
    const targets = new Set(REASONING_TARGETS);
    const rules = [];
    const seen = new Set();
    for (const rule of Array.isArray(raw.rules) ? raw.rules : []) {
      const from = text(rule && rule.from).toLowerCase();
      const to = text(rule && rule.to).toLowerCase();
      if (!sources.has(from) || !targets.has(to)) {
        throw new Error(`未知的思考强度：${from || '（空）'} → ${to || '（空）'}`);
      }
      if (seen.has(from)) throw new Error(`请求强度「${reasoningLabel(from)}」重复，请修改或删除重复规则`);
      seen.add(from);
      rules.push({ from, to });
    }
    const payload = {
      model: text(model && model.id),
      upstream_id: text(model && model.upstream_id),
      reasoning_mapping: { enabled: raw.enabled === true, rules },
    };
    // Only sent when the server advertised a revision; an older backend that
    // never sends one keeps receiving the exact same body as before.
    const revision = text(expectedRevision);
    if (revision) payload.expected_revision = revision;
    return payload;
  }

  function reasoningConversionText(effort, mapping) {
    const applied = mapping && typeof mapping === 'object' ? mapping : null;
    if (applied && text(applied.to)) {
      return `${reasoningLabel(applied.from)} → ${reasoningLabel(applied.to)}`;
    }
    const sent = text(effort);
    return sent ? reasoningLabel(sent) : '—';
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
    // Official amounts are decimal strings. A numeric fallback could already
    // have lost cents/credits before the UI sees it, so keep it unknown.
    if (typeof value !== 'string') return '';
    const raw = value.trim();
    return /^[0-9]+(?:\.[0-9]+)?$/.test(raw) ? raw : '';
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
    // An optional fixed code naming the check that rejected the answer.
    return { kind, message, reason: safeCode(value.reason, 40) || '' };
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

  function normalizeModelCoverage(value) {
    const data = value && typeof value === 'object' ? value : {};
    const modelCovered = typeof data.model_covered === 'boolean' ? data.model_covered : null;
    const rawFundingCheck = text(data.funding_check);
    const fundingCheck = ['available', 'insufficient', 'unknown'].includes(rawFundingCheck) ? rawFundingCheck : '';
    const rawFunding = text(data.funding);
    const funding = ['coding_plan', 'wallet', 'gateway'].includes(rawFunding) ? rawFunding : '';
    const rawState = text(data.state);
    const state = ['not_configured', 'needs_refresh', 'available', 'stale', 'error'].includes(rawState) ? rawState : '';
    return {
      credential_id: text(data.credential_id),
      model_id: text(data.model_id),
      billing_configured: data.billing_configured === true,
      workspace_id: text(data.workspace_id),
      state,
      fetched_at: quotaTimestamp(data.fetched_at),
      next_refresh_at: quotaTimestamp(data.next_refresh_at),
      model_covered: modelCovered,
      funding_check: fundingCheck,
      funding,
      error: quotaError(data.error),
    };
  }

  function modelCoverageState(value) {
    switch (text(value)) {
      case 'not_configured': return { text: '未配置', tone: 'muted' };
      case 'needs_refresh': return { text: '未检查', tone: 'muted' };
      case 'available': return { text: '已查询', tone: 'good' };
      case 'stale': return { text: '结果可能过时', tone: 'warn' };
      case 'error': return { text: '查询失败', tone: 'off' };
      default: return { text: '未检查', tone: 'muted' };
    }
  }

  function modelCoverageCheckText(value) {
    switch (text(value)) {
      case 'available': return '官方预检：可用';
      case 'insufficient': return '官方预检：不足';
      case 'unknown': return '官方预检：未知';
      default: return '';
    }
  }

  function modelCoverageSignalText(value) {
    if (value === true) return '套餐覆盖';
    if (value === false) return '套餐未覆盖';
    return '覆盖未知';
  }

  function modelCoverageErrorText(value) {
    const error = value && typeof value === 'object' ? value : {};
    const messages = {
      unauthorized: 'control key 未获官方接受',
      forbidden: '账户或工作区无权查询',
      rate_limited: '请求过于频繁，请稍后再试',
      timeout: '官方查询超时',
      malformed: '官方响应或配置无效',
      network: '无法连接官方查询接口',
      upstream: '官方查询暂时不可用',
      stopped: '插件正在停止',
    };
    return messages[text(error.kind)] || text(error.message) || '官方查询失败';
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
    const units = [[86400, '天'], [3600, '小时'], [60, '分钟'], [1, '秒']];
    const parts = [];
    let remaining = seconds;
    for (const [size, label] of units) {
      const count = Math.floor(remaining / size);
      if (count) parts.push(`${count} ${label}`);
      remaining %= size;
      if (parts.length === 2) break;
    }
    return parts.join(' ') || '0 秒';
  }

  function formatTime(value) {
    const stamp = quotaTimestamp(value);
    if (!stamp) return '';
    const date = new Date(stamp);
    const pad = number => String(number).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
  }

  // Dense log rows keep seconds but drop the date for today's entries, so the
  // time column stays comparable while an older page still identifies its day.
  function formatLogTime(value, now = Date.now()) {
    const stamp = quotaTimestamp(value);
    if (!stamp) return '';
    const date = new Date(stamp);
    const current = new Date(Number.isFinite(now) ? now : Date.now());
    const pad = number => String(number).padStart(2, '0');
    const clock = `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
    if (date.toDateString() === current.toDateString()) return clock;
    const day = `${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
    return date.getFullYear() === current.getFullYear() ? `${day} ${clock}` : `${date.getFullYear()}-${day} ${clock}`;
  }

  function formatLogDuration(ms) {
    if (!Number.isSafeInteger(ms) || ms < 0) return '';
    if (ms < 1000) return `${ms} ms`;
    if (ms < 60_000) return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`;
    const seconds = Math.floor(ms / 1000);
    return `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`;
  }

  function formatCount(value) {
    const n = safeInteger(value);
    if (n === null) return '';
    return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  }

  // Forecast copy follows the backend states. It is deliberately an estimate
  // based on recent consumption: no "N days left" promise and no pricing math.
  function forecastView(forecast) {
    const value = forecast && typeof forecast === 'object' ? forecast : {};
    switch (text(value.state)) {
      case 'exhausted':
        return { text: '本次采样显示窗口额度已用尽，请以官方后续查询为准', tone: 'warn' };
      case 'expired':
        return { text: '上次估算时间或窗口已过，请刷新额度后重新预测', tone: 'muted' };
      case 'insufficient_samples':
        return { text: '样本不足，暂不预测（需两次间隔至少 60 秒且有消费变化的成功查询）', tone: 'muted' };
      case 'rolling_window':
        return { text: '滚动窗口，不预测耗尽时间', tone: 'muted' };
      case 'estimated': {
        const span = formatDuration(value.seconds_to_exhaust);
        const at = formatTime(value.exhausts_at);
        const parts = ['按该窗口最近整体 credits 消耗估算'];
        if (at) parts.push(`预计在 ${at} 达到当前窗口上限${span ? `（采样时预计剩余 ${span}）` : ''}`);
        else if (span) parts.push(`采样时预计剩余 ${span}`);
        return { text: `${parts.join('，')}；仅为估算，不改变模型调度`, tone: 'warn' };
      }
      case 'resets_before_exhaustion': {
        const span = formatDuration(value.seconds_to_exhaust);
        return { text: span
          ? `按该窗口最近整体 credits 消耗估算，采样时预计 ${span}后耗尽，但会先到窗口重置时间；仅为估算`
          : '预计先到窗口重置时间，暂不给出耗尽时间', tone: 'muted' };
      }
      case 'beyond_horizon':
        return { text: '按该窗口最近整体 credits 消耗估算，预计远超一年，不显示具体耗尽时间', tone: 'muted' };
      default:
        return { text: '', tone: 'muted' };
    }
  }

  // A prediction must never cross a known reset boundary. If the estimated
  // exhaustion is at/after resetsAt, or either timestamp has already passed,
  // the cached estimate is withdrawn until another explicit refresh.
  function quotaForecastExpired(window, now = Date.now()) {
    const source = window && typeof window === 'object' ? window : {};
    const forecast = source.forecast && typeof source.forecast === 'object' ? source.forecast : {};
    const current = Number.isFinite(now) ? now : Date.now();
    const exhaustsAt = Date.parse(text(forecast.exhausts_at));
    const resetsAt = Date.parse(text(source.resetsAt));
    if (Number.isFinite(exhaustsAt) && exhaustsAt <= current) return true;
    if (Number.isFinite(resetsAt) && resetsAt <= current) return true;
    if (Number.isFinite(exhaustsAt) && Number.isFinite(resetsAt) && exhaustsAt >= resetsAt) return true;
    return false;
  }

  // --- theme ---------------------------------------------------------------

  const THEME_PREFERENCES = ['system', 'light', 'dark'];

  function normalizeThemePreference(value) {
    const raw = text(value).toLowerCase();
    return THEME_PREFERENCES.includes(raw) ? raw : 'system';
  }

  function nextThemePreference(value) {
    const current = normalizeThemePreference(value);
    return THEME_PREFERENCES[(THEME_PREFERENCES.indexOf(current) + 1) % THEME_PREFERENCES.length];
  }

  function resolvedTheme(preference, systemDark) {
    const wanted = normalizeThemePreference(preference);
    if (wanted === 'system') return systemDark === true ? 'dark' : 'light';
    return wanted;
  }

  function themePreferenceLabel(preference, resolved) {
    const wanted = normalizeThemePreference(preference);
    if (wanted === 'system') return `跟随系统（${resolved === 'dark' ? '深色' : '浅色'}）`;
    return wanted === 'dark' ? '深色' : '浅色';
  }

  // --- optional observability / runtime views ------------------------------
  //
  // These helpers mirror the real backend shapes. They copy only an explicit
  // allowlist of metadata: request/response bodies, completions, headers, keys,
  // control keys and workspace ids never enter the page.

  function shortText(value, max = 200) {
    const raw = text(value).replace(/[\u0000-\u001f\u007f]+/g, ' ');
    return raw.length > max ? `${raw.slice(0, max)}…` : raw;
  }

  function safeInteger(value, min = 0, max = Number.MAX_SAFE_INTEGER) {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= min && value <= max ? value : null;
  }

  function safeTimestamp(value) {
    return quotaTimestamp(value);
  }

  function safeCode(value, max = 80) {
    const raw = text(value);
    return /^[A-Za-z0-9_.:-]+$/.test(raw) ? raw.slice(0, max) : '';
  }

  function safeIdentifier(value, max = 128) {
    const raw = text(value);
    if (!raw || raw.length > max) return '';
    return /[\u0000-\u001f\u007f]/.test(raw) ? '' : raw;
  }

  // The backend limits model-test aliases to 512 UTF-8 bytes, not UTF-16 code
  // units. Identity fields must survive in full: truncating an alias would make
  // a response look like it belongs to a different model.
  function utf8ByteLength(value) {
    const raw = String(value ?? '');
    let bytes = 0;
    for (let index = 0; index < raw.length; index += 1) {
      const code = raw.charCodeAt(index);
      if (code < 0x80) bytes += 1;
      else if (code < 0x800) bytes += 2;
      else if (code >= 0xd800 && code <= 0xdbff) {
        bytes += 4;
        index += 1;
      } else bytes += 3;
    }
    return bytes;
  }

  function safeIdentity(value, maxBytes = 512) {
    const raw = text(value);
    if (!raw || utf8ByteLength(raw) > maxBytes) return '';
    return /[\u0000-\u001f\u007f]/.test(raw) ? '' : raw;
  }

  function safeURLText(value) {
    const raw = text(value);
    if (!raw) return '';
    try {
      const parsed = new URL(raw);
      if (parsed.username || parsed.password || parsed.search || parsed.hash) return '';
      return shortText(parsed.toString().replace(/\/$/, ''), 200);
    } catch { return ''; }
  }

  function safeCounter(value) {
    return safeInteger(value);
  }

  function normalizeRetention(value) {
    const data = value && typeof value === 'object' ? value : {};
    return {
      enabled: data.enabled !== false,
      retention_days: safeInteger(data.retention_days, 1, 90),
      max_entries: safeInteger(data.max_entries, 1, 500),
      entries: safeInteger(data.entries) ?? 0,
      oldest: safeTimestamp(data.oldest),
      newest: safeTimestamp(data.newest),
    };
  }

  function normalizeLogStatus(value) {
    const raw = text(value);
    return raw === 'success' || raw === 'error' ? raw : '';
  }

  // RequestLogEntry as served by GET /request-logs. Only safe metadata survives.
  function normalizeRequestLog(value) {
    if (!value || typeof value !== 'object') return null;
    const httpStatus = safeInteger(value.http_status, 100, 599);
    const record = {
      id: safeIdentifier(value.id, 64),
      started_at: safeTimestamp(value.started_at),
      finished_at: safeTimestamp(value.finished_at),
      duration_ms: safeCounter(value.duration_ms),
      model: shortText(value.model, 200),
      upstream_model: shortText(value.upstream_model, 200),
      credential_id: safeIdentifier(value.credential_id, 128),
      stream: value.stream === true,
      status: normalizeLogStatus(value.status),
      http_status: httpStatus,
      error_kind: safeCode(value.error_kind),
      input_tokens: safeCounter(value.input_tokens),
      output_tokens: safeCounter(value.output_tokens),
      total_tokens: safeCounter(value.total_tokens),
      request_id: safeIdentifier(value.request_id, 128),
      upstream_attempted: value.upstream_attempted === undefined ? null : value.upstream_attempted === true,
      reasoning_effort: safeCode(value.reasoning_effort, 32),
      reasoning_mapping: reasoningConversion(value.reasoning_mapping),
      first_token_ms: safeCounter(value.first_token_ms),
      cached_tokens: safeCounter(value.cached_tokens),
      reasoning_tokens: safeCounter(value.reasoning_tokens),
      output_tokens_per_second: safeRate(value.output_tokens_per_second),
    };
    // A detail is part of its total; one that contradicts it is unknown, not
    // something to display. The backend applies the same rule before storing.
    if (record.cached_tokens !== null && (record.input_tokens === null || record.cached_tokens > record.input_tokens)) record.cached_tokens = null;
    if (record.reasoning_tokens !== null && (record.output_tokens === null || record.reasoning_tokens > record.output_tokens)) record.reasoning_tokens = null;
    if (record.first_token_ms !== null && (record.duration_ms === null || record.first_token_ms > record.duration_ms)) record.first_token_ms = null;
    if (!record.id && !record.started_at && !record.model && !record.status) return null;
    return record;
  }

  // A non-negative finite number, for derived rates.
  function safeRate(value) {
    return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;
  }

  function formatTokenRate(value) {
    const rate = safeRate(value);
    return rate === null ? '' : `${Math.round(rate).toLocaleString('en-US')} t/s`;
  }

  // Tone of a latency figure. The first output is judged on its own; a total
  // is judged by throughput once the reply is long enough for that to mean
  // something, otherwise by wall time.
  function latencyTone(ms, kind, outputTokens) {
    if (!Number.isSafeInteger(ms) || ms <= 0) return '';
    const seconds = ms / 1000;
    if (kind === 'first') return seconds < 5 ? 'good' : seconds < 10 ? 'warn' : 'bad';
    if (Number.isSafeInteger(outputTokens) && outputTokens >= 100) {
      const throughput = outputTokens / seconds;
      return throughput >= 30 ? 'good' : throughput >= 15 ? 'warn' : 'bad';
    }
    return seconds < 10 ? 'good' : seconds < 30 ? 'warn' : 'bad';
  }

  const STATS_RANGES = ['all', 'today', '24h', '7d', '30d'];
  const STATS_RANGE_LABELS = { all: '全部', today: '今天', '24h': '近 24 小时', '7d': '近 7 天', '30d': '近 30 天' };

  function normalizeStatsRange(value) {
    return STATS_RANGES.includes(value) ? value : 'all';
  }

  function statsRangeLabel(value) {
    return STATS_RANGE_LABELS[normalizeStatsRange(value)];
  }

  // The lower bound a statistics range asks the backend for, or '' for all
  // retained rows. "Today" starts at local midnight.
  function statsRangeSince(value, now = Date.now()) {
    const range = normalizeStatsRange(value);
    if (range === 'all') return '';
    if (range === 'today') {
      const start = new Date(now);
      start.setHours(0, 0, 0, 0);
      return start.toISOString();
    }
    const hours = { '24h': 24, '7d': 24 * 7, '30d': 24 * 30 }[range];
    return new Date(now - hours * 3600 * 1000).toISOString();
  }

  function normalizeRequestLogsPage(value) {
    const data = value && typeof value === 'object' ? value : {};
    const source = Array.isArray(data.logs) ? data.logs : [];
    const logs = [];
    for (const item of source) {
      const record = normalizeRequestLog(item);
      if (record) logs.push(record);
      if (logs.length >= 200) break;
    }
    const limit = safeInteger(data.limit, 1, 200) ?? 50;
    const offset = safeInteger(data.offset, 0, 1_000_000) ?? 0;
    return {
      logs,
      total: safeInteger(data.total) ?? logs.length,
      limit,
      offset,
      has_more: data.has_more === true,
      next_offset: safeInteger(data.next_offset),
      retention: normalizeRetention(data.retention),
    };
  }

  function normalizeRequestStats(value) {
    const data = value && typeof value === 'object' ? value : {};
    const tokens = data.tokens && typeof data.tokens === 'object' ? data.tokens : {};
    const ratio = value => (typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1 ? value : null);
    const rate = ratio(data.success_rate);
    const range = data.range && typeof data.range === 'object' ? data.range : {};
    const models = (Array.isArray(data.models) ? data.models : []).map(item => {
      const row = item && typeof item === 'object' ? item : {};
      return {
        model: shortText(row.model, 200) || 'unknown',
        total: safeInteger(row.total) ?? 0,
        success: safeInteger(row.success) ?? 0,
        failed: safeInteger(row.failed) ?? 0,
        upstream_attempted: safeInteger(row.upstream_attempted) ?? 0,
        local_rejected: safeInteger(row.local_rejected) ?? 0,
        input_tokens: safeInteger(row.input_tokens),
        output_tokens: safeInteger(row.output_tokens),
        total_tokens: safeInteger(row.total_tokens),
      };
    }).slice(0, 100);
    return {
      total: safeInteger(data.total) ?? 0,
      success: safeInteger(data.success) ?? 0,
      failed: safeInteger(data.failed) ?? 0,
      streams: safeInteger(data.streams) ?? 0,
      upstream_attempted: safeInteger(data.upstream_attempted) ?? 0,
      local_rejected: safeInteger(data.local_rejected) ?? 0,
      success_rate: rate,
      tokens: {
        input: safeInteger(tokens.input),
        output: safeInteger(tokens.output),
        total: safeInteger(tokens.total),
        cached: safeInteger(tokens.cached),
        reasoning: safeInteger(tokens.reasoning),
        cache_rate: ratio(tokens.cache_rate),
        cache_samples: safeInteger(tokens.cache_samples) ?? 0,
        requests_with_usage: safeInteger(tokens.requests_with_usage) ?? 0,
        requests_without_usage: safeInteger(tokens.requests_without_usage) ?? 0,
      },
      output_tokens_per_second: safeRate(data.output_tokens_per_second),
      output_rate_samples: safeInteger(data.output_rate_samples) ?? 0,
      first_token_ms: safeInteger(data.first_token_ms),
      first_token_samples: safeInteger(data.first_token_samples) ?? 0,
      range: { since: safeTimestamp(range.since), until: safeTimestamp(range.until) },
      models,
      retention: normalizeRetention(data.retention),
    };
  }

  function normalizeConfig(value) {
    const data = value && typeof value === 'object' ? value : {};
    const hostPart = part => {
      const source = part && typeof part === 'object' ? part : {};
      return {
        owner: safeCode(source.owner, 40),
        read_only: source.read_only === true,
        mode: safeCode(source.mode, 40),
      };
    };
    const upstream = data.upstream && typeof data.upstream === 'object' ? data.upstream : {};
    const pause = data.pause && typeof data.pause === 'object' ? data.pause : {};
    const pauseMax = safeInteger(pause.max_seconds, 60, 604800);
    return {
      logs: normalizeRetention(data.logs),
      // Older servers may omit this section. The reviewed default is enabled,
      // matching the plugin's own pauseSettings; only an explicit false turns
      // automatic pauses off.
      pause: {
        enabled: pause.enabled !== false,
        max_seconds: pauseMax === null ? 86400 : pauseMax,
        owner: safeCode(pause.owner, 20),
      },
      upstream: { origin: safeURLText(upstream.origin), fixed: upstream.fixed === true },
      proxy: hostPart(data.proxy),
      routing: hostPart(data.routing),
      log_level: safeCode(data.log_level, 40),
    };
  }

  function normalizeRuntimeTest(value) {
    if (!value || typeof value !== 'object') return null;
    return {
      model_id: safeIdentity(value.model_id, 512),
      upstream_model: shortText(value.upstream_model, 512),
      status: normalizeLogStatus(value.status),
      http_status: safeInteger(value.http_status, 100, 599),
      duration_ms: safeCounter(value.duration_ms),
      tested_at: safeTimestamp(value.tested_at),
      error_kind: safeCode(value.error_kind),
    };
  }

  function normalizeRuntimeHost(value) {
    if (!value || typeof value !== 'object') return null;
    const optionalBoolean = field => value[field] === true ? true : value[field] === false ? false : null;
    return {
      disabled: optionalBoolean('disabled'),
      unavailable: optionalBoolean('unavailable'),
      status: safeCode(value.status, 64),
      status_message: shortText(value.status_message, 200),
      next_retry_after: safeTimestamp(value.next_retry_after),
    };
  }

  function normalizeRuntime(value) {
    const data = value && typeof value === 'object' ? value : {};
    const credentials = (Array.isArray(data.credentials) ? data.credentials : []).map(item => {
      const row = item && typeof item === 'object' ? item : {};
      const id = safeIdentifier(row.id, 128);
      return {
        id,
        label: shortText(row.label, 100) || id,
        disabled: row.disabled === true,
        model_count: safeInteger(row.model_count) ?? 0,
        models_scope: safeCode(row.models_scope, 40),
        testing: row.testing === true,
        last_test: normalizeRuntimeTest(row.last_test),
        host: normalizeRuntimeHost(row.host),
      };
    });
    const host = data.host && typeof data.host === 'object' ? data.host : {};
    const capabilities = [];
    if (data.capabilities && typeof data.capabilities === 'object') {
      for (const [name, part] of Object.entries(data.capabilities)) {
        const source = part && typeof part === 'object' ? part : {};
        capabilities.push({
          name: safeCode(name, 40),
          owner: safeCode(source.owner, 40),
          read_only: source.read_only === true,
        });
      }
    }
    return {
      observed_at: safeTimestamp(data.observed_at),
      loaded: data.loaded === true,
      stopped: data.stopped === true,
      active_requests: safeInteger(data.active_requests) ?? 0,
      pending_operations: safeInteger(data.pending_operations) ?? 0,
      model_count: safeInteger(data.model_count) ?? 0,
      credentials,
      host: { available: host.available === true, note: shortText(host.note, 300) },
      capabilities,
    };
  }

  // POST /model-test answer. The response body/completion is never part of the
  // normalized result, only status, timing, token counters and a safe error kind.
  function normalizeModelTest(value) {
    const data = value && typeof value === 'object' ? value : {};
    const usage = data.usage && typeof data.usage === 'object' ? data.usage : {};
    return {
      credential_id: safeIdentifier(data.credential_id, 128),
      model_id: safeIdentity(data.model_id, 512),
      upstream_model: shortText(data.upstream_model, 512),
      status: normalizeLogStatus(data.status),
      http_status: safeInteger(data.http_status, 100, 599),
      duration_ms: safeCounter(data.duration_ms),
      usage: {
        prompt_tokens: safeInteger(usage.prompt_tokens),
        completion_tokens: safeInteger(usage.completion_tokens),
        total_tokens: safeInteger(usage.total_tokens),
      },
      error_kind: safeCode(data.error && typeof data.error === 'object' ? data.error.kind : ''),
      real_inference: data.real_inference === true,
      upstream_attempted: Object.prototype.hasOwnProperty.call(data, 'upstream_attempted') ? data.upstream_attempted === true : null,
      streaming: data.streaming === true,
      request_kind: safeCode(data.request_kind, 60),
      // The strength actually sent, and the rule that produced it. Both come
      // from the plugin's closed sets, so they are safe to render verbatim.
      reasoning_effort: safeCode(data.reasoning_effort, 32),
      reasoning_mapping: reasoningConversion(data.reasoning_mapping),
    };
  }

  // The server sends {from,to} only when a rule matched, so an absent or
  // half-filled object means "no conversion happened".
  function reasoningConversion(raw) {
    const data = raw && typeof raw === 'object' ? raw : null;
    if (!data) return null;
    const from = safeCode(data.from, 32);
    const to = safeCode(data.to, 32);
    return from && to ? { from, to } : null;
  }

  // ---- Retry scheduling / request-scoped error rules --------------------------
  //
  // The policy is a list of CPA request-scoped error rules. The plugin owns the
  // policy and writes it into every credential's auth file; CPA owns the actual
  // scheduling decision (whether to rotate and whether to cool a credential).

  const ERROR_RULE_ACTIONS = [
    ['stop', '停止'],
    ['stop-and-cooldown', '停止并冷却'],
    ['continue', '继续'],
    ['continue-and-cooldown', '继续并冷却'],
  ];

  const ERROR_RULE_KINDS = [['contains', '包含文本'], ['regex', '正则表达式']];

  // Known markers map to a friendly name plus one sentence. Only markers this
  // plugin emits are listed, so an operator never sees a rule described as
  // something it cannot match.
  const ERROR_RULE_MARKERS = [
    ['[upstream:tier_limit_exceeded]', '账号并发上限', '上游返回会员并发上限（tier_limit_exceeded）；该账号暂不可用，可继续尝试其他账号。'],
    ['[upstream:rate_limit_exceeded]', '上游频率限制', '请求速率达到上游上限；冷却该账号并尝试其他账号。'],
    ['[upstream:insufficient_quota]', '账号额度不足', '账号额度已耗尽；冷却该账号并尝试其他额度可用的账号。'],
    ['[upstream:insufficient_balance]', '额度耗尽或余额不足', '上游返回 OpenDesign 的 insufficient_balance（Coding Plan 额度耗尽或钱包余额不足）；冷却该账号并尝试其他账号。'],
    ['[upstream:invalid_api_key]', '密钥无效', '上游拒绝该密钥；停止并冷却该账号，等待人工更换密钥。'],
    ['[upstream:server_error]', '上游服务错误', '上游返回服务端错误；冷却该账号并尝试其他账号。'],
    ['[upstream:server_overloaded]', '上游过载', '上游过载；冷却该账号并尝试其他账号。'],
    ['[upstream:model_not_found]', '模型不可用', '该账号没有这个模型；冷却该模型并尝试其他账号。'],
    ['[upstream:context_length_exceeded]', '上下文过长', '请求超出模型上下文；重试不会成功，停止切换账号。'],
    ['[upstream:response_limit_exceeded]', '响应过大', '上游响应超过插件上限；重试不会成功。'],
    ['[upstream:upstream_protocol_error]', '上游响应异常', '上游返回了无法解析的响应；冷却该账号并尝试其他账号。'],
    ['[upstream:upstream_transport_error]', '上游连接失败', '无法连接上游或连接中断；冷却该账号并尝试其他账号。'],
    ['[upstream:upstream_error]', '未分类上游错误', '上游返回了未被更具体规则覆盖的错误。'],
  ];

  function errorRuleActionLabel(action) {
    const entry = ERROR_RULE_ACTIONS.find(([value]) => value === text(action));
    return entry ? entry[1] : '';
  }

  function errorRuleKindLabel(kind) {
    const entry = ERROR_RULE_KINDS.find(([value]) => value === text(kind));
    return entry ? entry[1] : '';
  }

  // Conditions are read from either shape: the wire shape uses match /
  // match-regexr, while the editor draft uses one conditions list.
  function errorRuleConditions(rule) {
    const source = rule && typeof rule === 'object' ? rule : {};
    const out = [];
    for (const value of Array.isArray(source.match) ? source.match : []) {
      const condition = text(value);
      if (condition) out.push({ type: 'contains', text: condition });
    }
    for (const value of Array.isArray(source['match-regexr']) ? source['match-regexr'] : []) {
      const condition = text(value);
      if (condition) out.push({ type: 'regex', text: condition });
    }
    for (const entry of Array.isArray(source.conditions) ? source.conditions : []) {
      const condition = text(entry && entry.text);
      if (!condition) continue;
      const type = text(entry && entry.type) === 'regex' ? 'regex' : 'contains';
      // The wire shape was already collected above; do not double-count it.
      if (out.some(item => item.type === type && item.text === condition)) continue;
      out.push({ type, text: condition });
    }
    return out;
  }

  // explainErrorRule returns a display name and description. It never returns
  // HTML; callers assign the result with textContent.
  function explainErrorRule(rule, index = 0) {
    const conditions = errorRuleConditions(rule);
    const status = safeInteger(rule && rule.status, 100, 599);
    const matched = ERROR_RULE_MARKERS.filter(([marker]) => conditions.some(condition => condition.type === 'contains' && condition.text === marker));
    if (matched.length) {
      return {
        name: `规则 ${index + 1} · ${matched.map(item => item[1]).join(' / ')}`,
        detail: matched.map(item => item[2]).join(' '),
      };
    }
    const parts = [];
    if (status !== null) parts.push(`状态码 ${status}`);
    if (conditions.length) {
      const contains = conditions.filter(condition => condition.type === 'contains').length;
      const regex = conditions.length - contains;
      const shape = [];
      if (contains) shape.push(`${contains} 项包含文本`);
      if (regex) shape.push(`${regex} 项正则表达式`);
      parts.push(shape.join(' 与 '));
    }
    return {
      name: `规则 ${index + 1} · 自定义条件`,
      detail: parts.length ? `匹配 ${parts.join('，')}。` : '尚未设置匹配条件。',
    };
  }

  // normalizeErrorRules reads GET/PUT/POST /error-rules. Every value is bounded
  // and every unknown field is dropped, so the console can render server data
  // directly with textContent.
  function normalizeErrorRule(value) {
    const rule = value && typeof value === 'object' ? value : {};
    const status = safeInteger(rule.status, 100, 599);
    const action = ERROR_RULE_ACTIONS.some(([name]) => name === text(rule.action)) ? text(rule.action) : '';
    const conditions = [];
    for (const entry of Array.isArray(rule.match) ? rule.match : []) {
      const condition = shortText(entry, 4096);
      if (condition) conditions.push({ type: 'contains', text: condition });
    }
    for (const entry of Array.isArray(rule['match-regexr']) ? rule['match-regexr'] : []) {
      const condition = shortText(entry, 4096);
      if (condition) conditions.push({ type: 'regex', text: condition });
    }
    return { status: status === null ? 0 : status, action, conditions };
  }

  function normalizeErrorRules(value) {
    const data = value && typeof value === 'object' ? value : {};
    const syncSource = data.sync && typeof data.sync === 'object' ? data.sync : {};
    const failed = (Array.isArray(syncSource.failed) ? syncSource.failed : []).map(item => {
      const row = item && typeof item === 'object' ? item : {};
      return {
        id: safeIdentifier(row.id, 128),
        label: shortText(row.label, 100),
        error: shortText(row.error, 200),
      };
    });
    return {
      rules: (Array.isArray(data.rules) ? data.rules : []).map(normalizeErrorRule),
      defaults: (Array.isArray(data.defaults) ? data.defaults : []).map(normalizeErrorRule),
      inherits_cpa: data.inherits_cpa === true,
      load_error: shortText(data.load_error, 300),
      sync: {
        total: safeInteger(syncSource.total) ?? 0,
        succeeded: safeInteger(syncSource.succeeded) ?? 0,
        failed,
      },
    };
  }

  // buildErrorRulesPayload turns the editor draft into the wire shape. Empty
  // condition lists are omitted rather than sent as null, and an empty draft is
  // an empty array (which means "inherit the CPA configuration").
  function buildErrorRulesPayload(rules) {
    const out = [];
    for (const rule of Array.isArray(rules) ? rules : []) {
      const status = safeInteger(Number(rule && rule.status), 100, 599);
      const action = ERROR_RULE_ACTIONS.some(([name]) => name === text(rule && rule.action)) ? text(rule.action) : '';
      const match = [];
      const matchRegexr = [];
      for (const condition of Array.isArray(rule && rule.conditions) ? rule.conditions : []) {
        const value = text(condition && condition.text);
        if (!value) continue;
        if (text(condition && condition.type) === 'regex') matchRegexr.push(value);
        else match.push(value);
      }
      const entry = { status: status === null ? 0 : status, action };
      if (match.length) entry.match = match;
      if (matchRegexr.length) entry['match-regexr'] = matchRegexr;
      out.push(entry);
    }
    return out;
  }

  // validateErrorRules mirrors the backend's rejections so the operator gets an
  // immediate, specific message. The backend remains the authority.
  function validateErrorRules(rules) {
    const list = Array.isArray(rules) ? rules : [];
    if (list.length > 100) return '规则最多 100 条。';
    for (let index = 0; index < list.length; index += 1) {
      const rule = list[index] || {};
      const status = safeInteger(Number(rule.status), 100, 599);
      if (status === null) return `第 ${index + 1} 条规则的状态码必须是 100 到 599 之间的整数。`;
      if (!ERROR_RULE_ACTIONS.some(([name]) => name === text(rule.action))) return `第 ${index + 1} 条规则需要选择一个动作。`;
      const conditions = (Array.isArray(rule.conditions) ? rule.conditions : [])
        .map(condition => ({ type: text(condition && condition.type), text: text(condition && condition.text) }))
        .filter(condition => condition.text);
      if (!conditions.length) return `第 ${index + 1} 条规则至少需要一个匹配条件。`;
      if (conditions.length > 64) return `第 ${index + 1} 条规则的匹配条件过多。`;
      for (const condition of conditions) {
        if (condition.text.length > 4096) return `第 ${index + 1} 条规则的匹配条件超过 4096 个字符。`;
        if (condition.type === 'regex') {
          try {
            new RegExp(condition.text);
          } catch {
            return `第 ${index + 1} 条规则的正则表达式无效。`;
          }
        }
      }
    }
    return '';
  }

  // errorRulesSyncText renders the per-account sync result as plain text.
  function errorRulesSyncText(sync) {
    const data = sync && typeof sync === 'object' ? sync : {};
    const total = safeInteger(data.total) ?? 0;
    const succeeded = safeInteger(data.succeeded) ?? 0;
    const failed = Array.isArray(data.failed) ? data.failed : [];
    const head = `同步完成：${succeeded}/${total} 个账号`;
    if (!failed.length) return head;
    const lines = failed.map(item => `${text(item && item.label) || text(item && item.id) || '未命名账号'}：${text(item && item.error) || '同步失败'}`);
    return `${head}\n${lines.join('\n')}`;
  }

  function formatRatio(value) {
    return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 1
      ? `${(value * 100).toFixed(2)}%`
      : '';
  }

  const api = {
    DEFAULT_BASE_URL,
    parseAPIError,
    normalizeStatus,
    normalizeScheduling,
    normalizeQuotaPause,
    quotaPauseRemaining,
    quotaPauseEstimateRemaining,
    quotaPauseText,
    quotaPauseTitle,
    quotaPauseCountdown,
    quotaPauseSummary,
    quotaPauseCompact,
    formatQuotaTime,
    normalizeUpstreamModels,
    normalizeQuota,
    normalizeModelCoverage,
    modelCoverageState,
    modelCoverageCheckText,
    modelCoverageSignalText,
    modelCoverageErrorText,
    creditsPercent,
    creditsToUSD,
    formatCreditsUSD,
    rateText,
    sumCreditsUSD,
    formatCredits,
    formatUSD,
    formatDuration,
    formatTime,
    formatLogTime,
    formatLogDuration,
    formatCount,
    forecastView,
    buildModelsPayload,
    buildModelsSaveBody,
    hasUpstream,
    reasoningLabel,
    defaultReasoningRules,
    defaultReasoningMapping,
    normalizeReasoningMapping,
    reasoningKey,
    reasoningSourceOptions,
    reasoningTargetOptions,
    nextReasoningSource,
    defaultReasoningTarget,
    buildReasoningPayload,
    reasoningConversionText,
    aliasForUpstream,
    addUpstream,
    removeUpstream,
    filterUpstream,
    addUpstreams,
    removeUpstreams,
    // Optional observability/runtime helpers.
    normalizeRequestLog,
    normalizeRequestLogsPage,
    normalizeRequestStats,
    normalizeConfig,
    normalizeRuntime,
    normalizeModelTest,
    normalizeErrorRules,
    explainErrorRule,
    errorRuleActionLabel,
    errorRuleKindLabel,
    buildErrorRulesPayload,
    validateErrorRules,
    errorRulesSyncText,
    normalizeRetention,
    formatRatio,
    formatTokenRate,
    latencyTone,
    normalizeStatsRange,
    statsRangeLabel,
    statsRangeSince,
    STATS_RANGES,
    safeInteger,
    safeIdentifier,
    safeIdentity,
    utf8ByteLength,
    shortText,
    quotaForecastExpired,
    normalizeThemePreference,
    nextThemePreference,
    resolvedTheme,
    themePreferenceLabel,
  };
  Object.defineProperty(api, '__test', { value: { text }, enumerable: false });
  globalThis.OpenDesignCore = api;
})();
