package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const quotaPauseSyncError = "额度暂停状态未能同步到凭据文件；插件会重试。"

// observeQuotaBlock records an account-level block only for the narrow,
// structured upstream signal. A request for an old key or an old manual state
// may finish late and must not re-pause the current credential.
func (s *Service) observeQuotaBlock(source Credential, upstreamModel string, requestErr error) {
	var apiErr *APIError
	if !errors.As(requestErr, &apiErr) || apiErr == nil || !apiErr.quotaExhausted || source.ID == "" || source.APIKey == "" {
		return
	}
	// Resolve the policy before taking mu: config() takes mu.RLock and the block
	// below is built while holding the write lock.
	settings := s.config().pauseSettings()
	if !settings.enabled {
		return
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()

	now := s.quotaPauseTime().UTC()
	s.mu.Lock()
	current, exists := s.creds[source.ID]
	if !exists || s.revoked[source.ID] || current.APIKey != source.APIKey {
		s.mu.Unlock()
		return
	}
	if current.runtimeEpoch != source.runtimeEpoch && !current.quotaBlocked() {
		// An explicit resume or key/account edit fences old requests from
		// creating a *new* block. While an existing block is active, however,
		// a late 402 from the same inference key may name another failed model;
		// merge it even if the operator changed the manual switch meanwhile.
		s.mu.Unlock()
		return
	}
	var block *QuotaBlock
	if current.quotaBlocked() {
		block = cloneQuotaBlock(current.QuotaBlock)
		seen := block.ModelID == upstreamModel
		for _, model := range block.ModelIDs {
			seen = seen || model == upstreamModel
		}
		addQuotaBlockModel(block, upstreamModel)
		if now.After(block.ObservedAt) {
			block.ObservedAt = now
		}
		// A distinct model or an explicit later upstream reset can postpone the
		// first recheck, but only within the cap measured from the first block.
		// Repeated errors without a reset hint must not extend it indefinitely.
		if !seen || !apiErr.quotaResetAt.IsZero() {
			retry := quotaPauseDeadline(now, apiErr.quotaResetAt, settings)
			if latest := block.BlockedAt.Add(settings.maxWindow); retry.After(latest) {
				retry = latest
			}
			if retry.After(block.RetryAt) {
				block.RetryAt = retry
			}
		}
	} else {
		manual := current.Disabled
		block = &QuotaBlock{
			KeyHash: quotaPauseKeyHash(current.APIKey), ManualDisabled: &manual,
			Source: "upstream", Reason: "quota_exhausted", BlockedAt: now,
			ObservedAt: now, RetryAt: quotaPauseDeadline(now, apiErr.quotaResetAt, settings),
			ModelID: strings.TrimSpace(upstreamModel),
		}
		addQuotaBlockModel(block, upstreamModel)
	}
	current.QuotaBlock = block
	current.QuotaRevision = newID()
	// Publish the local guard first. If writing fails, no new request will reach
	// the exhausted upstream even if the host watcher still has an old snapshot.
	s.creds[current.ID] = current
	s.clearModelTestsLocked(current.ID)
	s.mu.Unlock()

	if err := s.persistQuotaBlockLocked(current.ID, current.QuotaRevision); err != nil {
		s.setQuotaPauseSyncError(current.ID, current.QuotaRevision, true)
	} else {
		s.setQuotaPauseSyncError(current.ID, current.QuotaRevision, false)
	}
	s.wakeQuotaPauseWorker()
}

// persistQuotaBlockLocked merges plugin-owned quota fields into the latest auth
// file, preserving host/operator extensions and the latest visible manual state.
// It follows ClinePass's atomic file + CPA watcher approach; it never calls the
// host while holding authFileMu. Caller holds credentialMu.
func (s *Service) persistQuotaBlockLocked(id, revision string) error {
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	s.mu.RLock()
	current, exists := s.creds[id]
	dir, filename := s.authDir, s.authFiles[id]
	revoked := s.revoked[id]
	s.mu.RUnlock()
	if !exists || revoked || current.QuotaRevision != revision || !current.quotaBlocked() {
		return errors.New("quota state changed")
	}
	if dir == "" || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
		return errors.New("credential file unavailable")
	}
	path := filepath.Join(dir, filename)
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var disk Credential
	var fields map[string]json.RawMessage
	if json.Unmarshal(original, &disk) != nil || json.Unmarshal(original, &fields) != nil || fields == nil {
		return errors.New("credential file changed")
	}
	if strings.TrimSpace(disk.ID) == "" {
		disk.ID = strings.TrimSuffix(filename, ".json")
	}
	if disk.Type != Provider || disk.ID != id || disk.APIKey != current.APIKey {
		return errors.New("credential file changed")
	}
	// A file edit that reached disk first wins for the operator-owned disabled
	// field. With a valid quota block, UnmarshalJSON already recovered the manual
	// flag from its snapshot rather than CPA's projected effective flag.
	manual := disk.Disabled
	block := cloneQuotaBlock(current.QuotaBlock)
	block.ManualDisabled = &manual
	fields["disabled"] = jsonBytes(manual)
	fields["quota_block"] = jsonBytes(block)
	fields["quota_revision"] = jsonBytes(revision)
	latest, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, latest) {
		return errors.New("credential file changed")
	}
	if err := writeJSONAtomic(path, fields); err != nil {
		return err
	}
	s.mu.Lock()
	if live, ok := s.creds[id]; ok && live.APIKey == current.APIKey && live.QuotaRevision == revision {
		live.Disabled = manual
		live.QuotaBlock = block
		s.storeCredentialLocked(live)
	}
	s.mu.Unlock()
	return nil
}

// resumeQuotaBlockLocked clears only the plugin-owned block. The caller holds
// credentialMu; expectedRevision fences timer and preflight results against a
// newer upstream error or manual edit. If dueOnly is true, an updated deadline
// is checked again under the same lock, not just when scheduling the sweep.
func (s *Service) resumeQuotaBlockLocked(id, expectedRevision string, dueOnly bool) error {
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	s.mu.RLock()
	current, exists := s.creds[id]
	dir, filename := s.authDir, s.authFiles[id]
	revoked := s.revoked[id]
	s.mu.RUnlock()
	if !exists || revoked || current.QuotaRevision != expectedRevision || !current.quotaBlocked() {
		return errors.New("quota state changed")
	}
	if dueOnly && current.QuotaBlock.RetryAt.After(s.quotaPauseTime()) {
		return errors.New("quota block is not due")
	}
	if dir == "" || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
		return errors.New("credential file unavailable")
	}
	path := filepath.Join(dir, filename)
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var disk Credential
	var fields map[string]json.RawMessage
	if json.Unmarshal(original, &disk) != nil || json.Unmarshal(original, &fields) != nil || fields == nil {
		return errors.New("credential file changed")
	}
	if strings.TrimSpace(disk.ID) == "" {
		disk.ID = strings.TrimSuffix(filename, ".json")
	}
	if disk.Type != Provider || disk.ID != id || disk.APIKey != current.APIKey {
		return errors.New("credential file changed")
	}
	manual := disk.Disabled
	newRevision := newID()
	delete(fields, "quota_block")
	fields["quota_revision"] = jsonBytes(newRevision)
	fields["disabled"] = jsonBytes(manual)
	latest, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, latest) {
		return errors.New("credential file changed")
	}
	if err := writeJSONAtomic(path, fields); err != nil {
		return err
	}
	s.mu.Lock()
	if live, ok := s.creds[id]; ok && live.APIKey == current.APIKey && live.QuotaRevision == expectedRevision {
		live.Disabled = manual
		live.QuotaBlock = nil
		live.QuotaRevision = newRevision
		// Fence errors from requests admitted before the block was cleared.
		s.credentialEpoch[id]++
		live.runtimeEpoch = s.credentialEpoch[id]
		s.invalidateBillingLocked(id)
		s.clearModelTestsLocked(id)
		s.creds[id] = live
		delete(s.quotaPauseErrors, id)
	}
	s.mu.Unlock()
	return nil
}

// quotaPauseTime obtains the clock pointer under mu. Quota tests replace that
// pointer before issuing requests; the background worker must synchronize with
// that replacement as well.
func (s *Service) quotaPauseTime() time.Time {
	s.mu.RLock()
	clock := s.quotaNow
	s.mu.RUnlock()
	return clock()
}

// quotaPauseResponse lists current pauses without exposing key material. The
// local sweep retries failed persistence; reading the list never proves recovery.
func (s *Service) quotaPauseResponse() map[string]any {
	// sweepQuotaBlocks takes credentialMu per candidate itself; taking it here
	// as well would deadlock on the first due block.
	s.sweepQuotaBlocks(s.stopCh)
	now := s.quotaPauseTime()
	s.mu.RLock()
	pauses := make([]map[string]any, 0, len(s.creds))
	for id, c := range s.creds {
		if s.revoked[id] || !c.quotaBlocked() {
			continue
		}
		pause := quotaPauseView(c, now, s.quotaPauseErrors[id])
		pause["credential_id"] = c.ID
		pause["label"] = c.Label
		pause["effective_disabled"] = c.effectiveDisabled()
		pauses = append(pauses, pause)
	}
	s.mu.RUnlock()
	sort.Slice(pauses, func(i, j int) bool {
		return stringOf(pauses[i]["credential_id"]) < stringOf(pauses[j]["credential_id"])
	})
	return map[string]any{"pauses": pauses}
}

// quotaPauseAction serves the explicit manual resume and the model-level quota
// preflight. Both require an exact credential id; the preflight also proves that
// a fresh official check succeeded after the recorded block was observed.
func (s *Service) quotaPauseAction(r ManagementRequest) (any, error) {
	if len(r.Body) > maxConfigBodyBytes {
		return managementError(http.StatusRequestEntityTooLarge, "quota pause request is too large")
	}
	var in struct {
		CredentialID string `json:"credential_id"`
	}
	if err := jsonUnmarshalObject(r.Body, &in); err != nil {
		return managementError(http.StatusBadRequest, "invalid quota pause JSON")
	}
	id := strings.TrimSpace(in.CredentialID)
	if id == "" {
		return managementError(http.StatusBadRequest, "credential_id is required")
	}
	resume := strings.HasSuffix(strings.TrimRight(r.Path, "/"), "/resume")
	if resume {
		s.credentialMu.Lock()
		s.mu.RLock()
		current, exists := s.creds[id]
		revoked := s.revoked[id]
		s.mu.RUnlock()
		if !exists || revoked {
			s.credentialMu.Unlock()
			return managementError(http.StatusNotFound, "credential not found")
		}
		if !current.quotaBlocked() {
			s.credentialMu.Unlock()
			return managementJSON(http.StatusOK, s.quotaPauseResponse())
		}
		err := s.resumeQuotaBlockLocked(id, current.QuotaRevision, false)
		s.credentialMu.Unlock()
		if err != nil {
			return managementError(http.StatusBadGateway, "could not clear the quota pause on the credential file")
		}
		return managementJSON(http.StatusOK, s.quotaPauseResponse())
	}
	if err := s.recheckQuotaBlock(r.HostCallbackID, id); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	return managementJSON(http.StatusOK, s.quotaPauseResponse())
}

// recheckQuotaBlock runs an operator-requested check even before the deadline.
func (s *Service) recheckQuotaBlock(callbackID, id string) error {
	return s.recheckQuotaBlockMode(callbackID, id, false)
}

// recheckQuotaBlockMode serializes recovery checks without limiting inference.
// Every known failed model must have a fresh official available result for the
// same workspace member, and no same-key inference may still be in flight. The
// operator must configure the control key and inference key for the same account;
// this API cannot independently prove that relationship.
func (s *Service) recheckQuotaBlockMode(callbackID, id string, dueOnly bool) error {
	s.mu.RLock()
	stop := s.stopCh
	s.mu.RUnlock()
	select {
	case s.quotaRecoveryGate <- struct{}{}:
		defer func() { <-s.quotaRecoveryGate }()
	case <-stop:
		return localFail(http.StatusServiceUnavailable, "plugin_unavailable", "OpenDesign plugin is shutting down")
	}

	now := s.quotaPauseTime()
	s.mu.RLock()
	current, exists := s.creds[id]
	revoked := s.revoked[id]
	billingEpoch := s.billingEpoch[id]
	inflight := s.quotaHasInFlightLocked(current)
	syncPending := s.quotaPauseErrors[id] != ""
	enabled := s.cfg.pauseSettings().enabled
	stopped := s.stopped
	s.mu.RUnlock()
	if stopped {
		return localFail(http.StatusServiceUnavailable, "plugin_unavailable", "OpenDesign plugin is shutting down")
	}
	if !exists || revoked {
		return fail(http.StatusNotFound, "credential not found")
	}
	if !current.quotaBlocked() {
		return nil
	}
	if dueOnly && (!enabled || current.QuotaBlock.RetryAt.After(now)) {
		return localFail(http.StatusConflict, "quota_recheck_not_due", "额度暂停尚未到复查时间，或自动暂停已关闭")
	}
	if inflight {
		return localFail(http.StatusConflict, "quota_recheck_inflight", "仍有同一推理密钥的请求在途，请稍后复查")
	}
	if syncPending {
		return localFail(http.StatusConflict, "quota_sync_pending", "额度暂停尚未同步到凭据文件，请稍后复查")
	}
	if current.ControlKey == "" || current.WorkspaceID == "" {
		return localFail(http.StatusConflict, "quota_recheck_unconfigured", "未配置官方额度授权或 Workspace ID，无法复查")
	}
	block := cloneQuotaBlock(current.QuotaBlock)
	models, complete := quotaBlockModels(block)
	if !complete {
		return localFail(http.StatusConflict, "quota_recheck_ambiguous", "耗尽模型集合不完整，无法自动确认恢复")
	}
	type proof struct {
		view    modelCoverageResponse
		model   string
		started time.Time
	}
	proofs := make([]proof, 0, len(models))
	member := ""
	for _, model := range models {
		s.mu.RLock()
		clock := s.modelCoverageNow
		s.mu.RUnlock()
		started := clock()
		view, err := s.readModelCoverage(id, model, callbackID, true)
		if err != nil {
			return err
		}
		one := cloneQuotaBlock(block)
		one.ModelID, one.MultipleModels = model, false
		if !quotaRecoveryProven(view, one, id, current.WorkspaceID, started, clock()) {
			return localFail(http.StatusConflict, "quota_recheck_unproven", "本次官方复查未能证明额度已恢复")
		}
		if member != "" && member != view.evidence.workspaceMemberID {
			return localFail(http.StatusConflict, "quota_recheck_identity", "不同模型的官方复查身份不一致")
		}
		member = view.evidence.workspaceMemberID
		proofs = append(proofs, proof{view: view, model: model, started: started})
	}

	// Match the global lock order used by model/config updates. No new same-key
	// inference can be admitted while the block remains published in memory.
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	now = s.quotaPauseTime()
	s.mu.RLock()
	live, exists := s.creds[id]
	unchanged := exists && !s.revoked[id] && !s.stopped && live.APIKey == current.APIKey &&
		live.ControlKey == current.ControlKey && live.WorkspaceID == current.WorkspaceID &&
		live.runtimeEpoch == current.runtimeEpoch && live.QuotaRevision == current.QuotaRevision &&
		s.billingEpoch[id] == billingEpoch && live.quotaBlocked() && !s.quotaHasInFlightLocked(live) && s.quotaPauseErrors[id] == ""
	if dueOnly {
		unchanged = unchanged && s.cfg.pauseSettings().enabled && !live.QuotaBlock.RetryAt.After(now)
	}
	clock := s.modelCoverageNow
	s.mu.RUnlock()
	if !unchanged {
		return localFail(http.StatusConflict, "quota_state_changed", "额度暂停状态或凭据已变化，请刷新后重试")
	}
	// For a large model set, also reject evidence that aged out while the other
	// models were checked. Never extrapolate one model's result to another.
	finished := clock()
	for _, item := range proofs {
		one := cloneQuotaBlock(block)
		one.ModelID, one.MultipleModels = item.model, false
		if !quotaRecoveryProven(item.view, one, id, current.WorkspaceID, item.started, finished) {
			return localFail(http.StatusConflict, "quota_recheck_unproven", "官方复查结果已过期，请稍后重试")
		}
	}
	if err := s.resumeQuotaBlockLocked(id, current.QuotaRevision, dueOnly); err != nil {
		return localFail(http.StatusBadGateway, "host_contract_error", "额度复查通过，但凭据文件未能更新")
	}
	return nil
}

func (s *Service) wakeQuotaPauseWorker() {
	select {
	case s.quotaPauseWake <- struct{}{}:
	default:
	}
}

func (s *Service) setQuotaPauseSyncError(id, revision string, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.creds[id]
	if !exists || current.QuotaRevision != revision {
		return
	}
	if failed {
		s.quotaPauseErrors[id] = quotaPauseSyncError
	} else {
		delete(s.quotaPauseErrors, id)
	}
}

// startQuotaPauseWorker retries failed file writes and starts serial official
// preflights for due blocks. A deadline by itself never clears a block.
func (s *Service) startQuotaPauseWorker() {
	s.mu.Lock()
	if s.stopped || s.quotaPauseStarted {
		s.mu.Unlock()
		return
	}
	stop := s.stopCh
	s.quotaPauseStarted = true
	s.quotaPauseWorker.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.quotaPauseWorker.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-s.quotaPauseWake:
			case <-ticker.C:
			}
			select {
			case <-stop:
				return
			default:
			}
			s.sweepQuotaBlocks(stop)
			s.recheckDueQuotaBlocks(stop)
		}
	}()
	s.wakeQuotaPauseWorker()
}

// sweepQuotaBlocks retries failed persistence without treating a deadline as
// proof of restored quota. It performs no network requests.
func (s *Service) sweepQuotaBlocks(stop <-chan struct{}) {
	type candidate struct {
		id, revision string
	}
	s.mu.RLock()
	var candidates []candidate
	for id, c := range s.creds {
		if s.revoked[id] || !c.quotaBlocked() {
			continue
		}
		if s.quotaPauseErrors[id] != "" {
			candidates = append(candidates, candidate{id: id, revision: c.QuotaRevision})
		}
	}
	s.mu.RUnlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })
	for _, item := range candidates {
		select {
		case <-stop:
			return
		default:
		}
		s.credentialMu.Lock()
		err := s.persistQuotaBlockLocked(item.id, item.revision)
		s.credentialMu.Unlock()
		s.setQuotaPauseSyncError(item.id, item.revision, err != nil)
	}
}

// recheckDueQuotaBlocks runs only on the worker, not on management reads. One
// account is checked at a time, each model is checked serially, and failures
// leave the block in place. The model preflight's normal refresh interval bounds
// retry traffic even when many wakeups arrive between ticker ticks.
func (s *Service) recheckDueQuotaBlocks(stop <-chan struct{}) {
	now := s.quotaPauseTime()
	s.mu.RLock()
	if s.stopped || !s.cfg.pauseSettings().enabled {
		s.mu.RUnlock()
		return
	}
	var ids []string
	for id, c := range s.creds {
		if !s.revoked[id] && c.quotaBlocked() && !c.QuotaBlock.RetryAt.After(now) && s.quotaPauseErrors[id] == "" {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	sort.Strings(ids)
	for _, id := range ids {
		select {
		case <-stop:
			return
		default:
		}
		_ = s.recheckQuotaBlockMode("", id, true)
	}
}
