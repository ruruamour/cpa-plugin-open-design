package opendesign

import (
	"bytes"
	"context"
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

// observeQuotaBlock records only the narrow structured upstream signal. A late
// error may not recreate a block after an explicit resume or identity change.
// Existing blocks for the same inference key, including other credential IDs,
// must nevertheless observe the model: those records share the upstream key.
func (s *Service) observeQuotaBlock(source Credential, upstreamModel string, requestErr error) {
	var apiErr *APIError
	if !errors.As(requestErr, &apiErr) || apiErr == nil || !apiErr.quotaExhausted || source.APIKey == "" {
		return
	}
	settings := s.config().pauseSettings()
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()

	type changedBlock struct {
		id, revision string
		existing     bool
	}
	var changed []changedBlock
	now := s.quotaPauseTime().UTC()
	s.mu.Lock()
	for id, current := range s.creds {
		if s.revoked[id] || current.APIKey != source.APIKey {
			continue
		}
		active := current.quotaBlocked()
		if !active && (source.ID == "" || id != source.ID || !settings.enabled || current.runtimeEpoch != source.runtimeEpoch) {
			// Never create a new block for another record, an unknown source, an
			// explicitly resumed account, or a disabled policy. Existing blocks
			// still collect late signals even after the policy is switched off.
			continue
		}
		var block *QuotaBlock
		if active {
			block = cloneQuotaBlock(current.QuotaBlock)
			seen := block.ModelID == upstreamModel
			for _, model := range block.ModelIDs {
				seen = seen || model == upstreamModel
			}
			addQuotaBlockModel(block, upstreamModel)
			if now.After(block.ObservedAt) {
				block.ObservedAt = now
			}
			// Distinct models or an explicit reset can postpone a recheck, but
			// repeated errors without a reset hint cannot extend it indefinitely.
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
		delete(s.quotaRecheckStatus, id)
		delete(s.quotaRecheckRetryAt, id)
		delete(s.quotaRecheckFailures, id)
		// Publish every in-memory guard before writing any of the files.
		s.creds[id] = current
		s.clearModelTestsLocked(id)
		changed = append(changed, changedBlock{id: id, revision: current.QuotaRevision, existing: active})
	}
	s.mu.Unlock()

	sort.Slice(changed, func(i, j int) bool { return changed[i].id < changed[j].id })
	for _, item := range changed {
		// A brand-new block normally inherits the file's disable choice; an
		// existing block or a known-stale file requires the conservative merge.
		_ = s.persistQuotaBlockLocked(item.id, item.revision, !item.existing)
	}
	if len(changed) != 0 {
		s.wakeQuotaPauseWorker()
	}
}

// persistQuotaBlockLocked merges plugin-owned quota fields into the latest auth
// file, preserving host/operator extensions.
//
// manualFromDisk decides where the operator's disable choice comes from. A newly
// created block takes it from the file unless the file is already known stale,
// because an operator may have disabled the account there before the pause
// existed (and the plugin console is not the only editor). A block that already
// exists takes it from the block snapshot: at that
// point the file may be a host rewrite of a pre-pause body whose `disabled` is
// only CPA's projection, so trusting it would silently drop a plugin-console
// choice that the snapshot still records.
//
// Caller holds credentialMu; never calls the host while holding authFileMu.
func (s *Service) persistQuotaBlockLocked(id, revision string, manualFromDisk bool) (result error) {
	// Follow the global order credentialMu -> quotaCommitGate -> authFileMu so a
	// pause write cannot interleave with a recovery/resume commit.
	s.quotaCommitGate <- struct{}{}
	defer func() { <-s.quotaCommitGate }()
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	s.mu.RLock()
	current, exists := s.creds[id]
	dir, filename := s.authDir, s.authFiles[id]
	revoked, stale := s.revoked[id], s.quotaDiskStale[id]
	s.mu.RUnlock()
	// Update synchronization bookkeeping before releasing authFileMu. Otherwise
	// a watcher can mark a newer divergence after the write, only for the old
	// caller to clear that marker/error after the watcher has returned.
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		live, ok := s.creds[id]
		if !ok || s.revoked[id] || live.APIKey != current.APIKey ||
			live.QuotaRevision != revision || !live.quotaBlocked() {
			return
		}
		if result != nil {
			s.quotaPauseErrors[id] = quotaPauseSyncError
		} else {
			delete(s.quotaPauseErrors, id)
			// A successfully persisted new block supersedes a pending repair of
			// the old cleared state. On failure, retain it and retry the block.
			delete(s.quotaDiskStale, id)
		}
	}()
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
	if disk.Type != Provider || disk.ID != id || disk.APIKey != current.APIKey ||
		conflictingQuotaField(disk.ControlKey, current.ControlKey) ||
		conflictingQuotaField(disk.WorkspaceID, current.WorkspaceID) {
		return errors.New("credential file changed")
	}
	// The live block owns the operator's choice. Disk `disabled` is only a
	// projection that CPA may have rewritten (including from a pre-pause
	// snapshot), so trusting it here would silently drop a plugin-console disable.
	// A new block normally takes the latest manual choice from disk. If the
	// cleared file was already known stale, however, that disk body can be an
	// older pause or pre-pause save; retain the restrictive union instead.
	manual := manualChoice(current.QuotaBlock, disk, manualFromDisk && !stale)
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

// resumeFailureReason maps an explicit resume error to a fixed, redacted
// explanation. A failed manual resume is not retried by the background worker;
// the operator must try again after correcting the underlying problem.
func resumeFailureReason(err error) string {
	switch err.Error() {
	case "quota state changed":
		return "凭据状态已变化，请刷新后重试"
	case "credential file changed":
		return "凭据文件已被修改，请刷新后重试"
	case "credential file unavailable":
		return "凭据文件路径不可用"
	default:
		return "凭据文件访问或写入失败，请检查文件状态后重试"
	}
}

func quotaRecheckUnavailable() error {
	return localFail(http.StatusServiceUnavailable, "plugin_unavailable", "plugin is shutting down")
}

// manualChoice decides the operator's disable state while a block exists. The
// result is the restrictive union of the live snapshot and the file, because the
// two can disagree in either direction and only one of those directions is safe:
// a stale host body can carry a projected `disabled=false` for an account the
// operator disabled in the plugin console, and trusting the file would silently
// re-enable it. Erring the other way only keeps a paused account paused, which
// the operator can always undo in the console. A brand-new block (manualFromDisk)
// normally takes the file's value, since an operator may have disabled the
// account there before the pause was created; it still unions the live choice so
// a host rewrite of an older pre-pause body cannot drop a plugin-console disable
// that the watcher has not reported yet.
func manualChoice(block *QuotaBlock, disk Credential, manualFromDisk bool) bool {
	if manualFromDisk {
		manual := disk.Disabled
		if block != nil && block.ManualDisabled != nil {
			manual = manual || *block.ManualDisabled
		}
		return manual
	}
	manual := true
	if block != nil && block.ManualDisabled != nil {
		manual = *block.ManualDisabled
	}
	return manual || disk.Disabled
}

// quotaCommitSnapshot is everything a quota-file write must re-check after it
// acquired authFileMu. A watcher may publish a new identity while a recheck waits
// for that lock, and an operator may edit the file directly without the watcher
// having reported it yet, so the in-memory check alone is not a fence.
type quotaCommitSnapshot struct {
	id, apiKey, controlKey, workspaceID, quotaRevision string
	runtimeEpoch                                       uint64
	enabled                                            bool
	dueOnly                                            bool
	now                                                time.Time
}

// quotaCommitSnapshotLocked snapshots the identity fields a quota write must
// re-verify. It acquires mu itself; callers must not already hold its write lock.
func (s *Service) quotaCommitSnapshotLocked(id string) (quotaCommitSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, exists := s.creds[id]
	if !exists {
		return quotaCommitSnapshot{}, false
	}
	return quotaCommitSnapshot{
		id: c.ID, apiKey: c.APIKey, controlKey: c.ControlKey, workspaceID: c.WorkspaceID,
		quotaRevision: c.QuotaRevision, runtimeEpoch: c.runtimeEpoch,
	}, true
}

func credentialFilePath(dir, filename string) (string, error) {
	if dir == "" || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
		return "", errors.New("credential file unavailable")
	}
	return filepath.Join(dir, filename), nil
}

// readQuotaAuthFile reads one auth file together with its preserved fields. The
// caller owns authFileMu, so no plugin writer can swap the file in between.
func readQuotaAuthFile(path, id string) ([]byte, map[string]json.RawMessage, Credential, error) {
	var disk Credential
	original, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, disk, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(original, &disk) != nil || json.Unmarshal(original, &fields) != nil || fields == nil {
		return nil, nil, disk, errors.New("credential file changed")
	}
	if strings.TrimSpace(disk.ID) == "" {
		disk.ID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	if disk.Type != Provider || disk.ID != id {
		return nil, nil, disk, errors.New("credential file changed")
	}
	return original, fields, disk, nil
}

// conflictingQuotaField reports a real identity conflict. An empty value in the
// file means the writer predates that optional field, not a change.
func conflictingQuotaField(diskValue, liveValue string) bool {
	return diskValue != "" && diskValue != liveValue
}

// publishQuotaResumeLocked writes the cleared block and publishes the matching
// in-memory state. The caller holds credentialMu, quotaCommitGate and
// authFileMu, and already verified the file's ownership and identity.
func (s *Service) publishQuotaResumeLocked(expected quotaCommitSnapshot, path string, original []byte, fields map[string]json.RawMessage, manual bool) error {
	revision := newID()
	delete(fields, "quota_block")
	fields["quota_revision"] = jsonBytes(revision)
	fields["disabled"] = jsonBytes(manual)
	latest, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, latest) {
		return errors.New("credential file changed")
	}
	if err := writeJSONAtomic(path, fields); err != nil {
		return err
	}
	s.mu.Lock()
	if live, ok := s.creds[expected.id]; ok && live.APIKey == expected.apiKey &&
		live.ControlKey == expected.controlKey && live.WorkspaceID == expected.workspaceID &&
		live.runtimeEpoch == expected.runtimeEpoch && live.QuotaRevision == expected.quotaRevision {
		live.Disabled = manual
		live.QuotaBlock = nil
		live.QuotaRevision = revision
		// Fence errors from requests admitted before the block was cleared.
		s.credentialEpoch[expected.id]++
		live.runtimeEpoch = s.credentialEpoch[expected.id]
		s.invalidateBillingLocked(expected.id)
		s.clearModelTestsLocked(expected.id)
		s.creds[expected.id] = live
		delete(s.quotaPauseErrors, expected.id)
		delete(s.quotaDiskStale, expected.id)
		delete(s.quotaRecheckStatus, expected.id)
		delete(s.quotaRecheckRetryAt, expected.id)
		delete(s.quotaRecheckFailures, expected.id)
	}
	s.mu.Unlock()
	return nil
}

// persistQuotaResumeLocked republishes the already-published cleared state of a
// credential whose auth file still carries an older body (for example a stale
// paused snapshot delivered by the host after an explicit resume). It never
// clears a live block and never needs recovery evidence: it only refuses when the
// live state is not the cleared one it is meant to mirror. Caller holds
// credentialMu.
func (s *Service) persistQuotaResumeLocked(id string) error {
	s.quotaCommitGate <- struct{}{}
	defer func() { <-s.quotaCommitGate }()
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	s.mu.RLock()
	current, exists := s.creds[id]
	dir, filename := s.authDir, s.authFiles[id]
	revoked, stopped, stale := s.revoked[id], s.stopped, s.quotaDiskStale[id]
	s.mu.RUnlock()
	if stopped {
		return errors.New("quota state changed")
	}
	if !exists || revoked || !stale || current.QuotaBlock != nil || current.QuotaRevision == "" {
		return errors.New("quota state changed")
	}
	path, err := credentialFilePath(dir, filename)
	if err != nil {
		return err
	}
	original, fields, disk, err := readQuotaAuthFile(path, id)
	if err != nil {
		return err
	}
	if disk.APIKey != current.APIKey ||
		conflictingQuotaField(disk.ControlKey, current.ControlKey) ||
		conflictingQuotaField(disk.WorkspaceID, current.WorkspaceID) {
		return errors.New("credential file changed")
	}
	manual := current.Disabled
	if disk.QuotaBlock == nil {
		// A cleared body is safe to converge: there is no pause to erase, and the
		// manual choice below only ever preserves a disable. A different revision
		// is an older clear, not evidence of a newer decision this repair would
		// drop. Only an identical body can retire the marker without a write.
		if disk.QuotaRevision == current.QuotaRevision && disk.Disabled == current.Disabled {
			s.mu.Lock()
			if live, ok := s.creds[id]; ok && live.APIKey == current.APIKey && live.QuotaBlock == nil &&
				live.QuotaRevision == current.QuotaRevision && live.Disabled == current.Disabled {
				delete(s.quotaDiskStale, id)
			}
			s.mu.Unlock()
			return nil
		}
		// An older clear lost the revision, but not necessarily a later manual
		// disable. Never turn either side back on during repair.
		manual = manual || disk.Disabled
	} else if !quotaBlockBelongsToKey(disk.QuotaBlock, disk.APIKey) {
		// An invalid or foreign block is not ours to remove.
		return errors.New("credential file changed")
	}
	revision := newID()
	delete(fields, "quota_block")
	fields["quota_revision"] = jsonBytes(revision)
	fields["disabled"] = jsonBytes(manual)
	latest, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, latest) {
		return errors.New("credential file changed")
	}
	if err := writeJSONAtomic(path, fields); err != nil {
		return err
	}
	// This is a file repair, not a resume. The revision advances so a later
	// stale body is recognised again; if a disk-only disable was discovered,
	// storeCredentialLocked also fences requests admitted before that change.
	s.mu.Lock()
	if live, ok := s.creds[id]; ok && live.APIKey == current.APIKey && live.QuotaBlock == nil &&
		live.QuotaRevision == current.QuotaRevision && live.Disabled == current.Disabled {
		live.Disabled = manual
		live.QuotaRevision = revision
		s.storeCredentialLocked(live)
		// Clear the marker in the same critical section as the revision advance.
		// A later attempt to clear it using the old revision is correctly fenced.
		delete(s.quotaDiskStale, id)
	}
	s.mu.Unlock()
	return nil
}

// resumeQuotaBlockLocked clears only the plugin-owned block for the explicit
// operator resume. Caller holds credentialMu; expectedRevision fences timer and
// preflight results against a newer upstream error or manual edit. If dueOnly is
// true, an updated deadline is checked again under the same lock, not just when
// scheduling the sweep.
//
// The explicit resume deliberately keeps its operator semantics: it does not
// require recovery evidence, only that the identity it was requested for is
// still the published one. It is serialized with quiesce through quotaCommitGate
// so no file write can land after the plugin reported itself stopped.
func (s *Service) resumeQuotaBlockLocked(id, expectedRevision string, dueOnly bool) error {
	s.quotaCommitGate <- struct{}{}
	defer func() { <-s.quotaCommitGate }()
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	s.mu.RLock()
	current, exists := s.creds[id]
	dir, filename := s.authDir, s.authFiles[id]
	revoked, stopped := s.revoked[id], s.stopped
	s.mu.RUnlock()
	if stopped {
		// Quiesce already closed the lifecycle; a write here would land after the
		// plugin reported itself stopped.
		return quotaRecheckUnavailable()
	}
	if !exists || revoked || current.QuotaRevision != expectedRevision || !current.quotaBlocked() {
		return errors.New("quota state changed")
	}
	if dueOnly && current.QuotaBlock.RetryAt.After(s.quotaPauseTime()) {
		return errors.New("quota block is not due")
	}
	path, err := credentialFilePath(dir, filename)
	if err != nil {
		return err
	}
	original, fields, disk, err := readQuotaAuthFile(path, id)
	if err != nil {
		return err
	}
	// The file may have been edited (or rewritten by the host) after the last
	// in-memory publish. Refuse to clear a block the disk describes with another
	// identity, but do not require the block itself: when persistence failed, or
	// the host rewrote an older body that never carried the block, the live block
	// is still the authoritative plugin state and only the operator can clear it.
	// Optional fields are only rejected when the file carries a conflicting value,
	// so an older file that predates them keeps working.
	if disk.APIKey != current.APIKey ||
		conflictingQuotaField(disk.ControlKey, current.ControlKey) ||
		conflictingQuotaField(disk.WorkspaceID, current.WorkspaceID) {
		return errors.New("credential file changed")
	}
	// A block that does not belong to this exact key is not ours to remove,
	// regardless of its revision.
	if disk.QuotaBlock != nil && !quotaBlockBelongsToKey(disk.QuotaBlock, current.APIKey) {
		return errors.New("credential file changed")
	}
	// A differing quota revision normally means another writer advanced the
	// file. An explicit resume is the operator's own decision for this account,
	// and this plugin keeps one process-wide state (a shared auth-dir with a
	// second writer is outside the supported model, and those writes have no
	// cross-process CAS). When the file carries a pause for this exact key, it
	// can only be an older body (a host rewrite of a pre-pause snapshot or a
	// failed persistence). A cleared body has no pause to erase, and the manual
	// choice below only ever preserves a disable, so its revision is compatible
	// too. Either way the operator can get unstuck; a foreign block was refused
	// above.
	// Keep the operator's plugin-console choice, which lives in the block. A file
	// that still has an intact block is the better source when the live one was
	// recorded elsewhere, but the live snapshot always wins when present.
	manual := manualChoice(current.QuotaBlock, disk, false)
	expected, _ := s.quotaCommitSnapshotLocked(id)
	return s.publishQuotaResumeLocked(expected, path, original, fields, manual)
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
	s.mu.RLock()
	stop := s.stopCh
	s.mu.RUnlock()
	s.sweepQuotaBlocks(stop)
	now := s.quotaPauseTime()
	s.mu.RLock()
	pauses := make([]map[string]any, 0, len(s.creds))
	for id, c := range s.creds {
		if s.revoked[id] || !c.quotaBlocked() {
			continue
		}
		pause := quotaPauseView(c, now, s.quotaPauseErrors[id], s.quotaRecheckStatus[id])
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
			return managementError(http.StatusBadGateway, "无法清除凭据文件上的额度暂停："+resumeFailureReason(err))
		}
		return managementJSON(http.StatusOK, s.quotaPauseResponse())
	}
	s.mu.RLock()
	revision := s.creds[id].QuotaRevision
	s.mu.RUnlock()
	err := s.recheckQuotaBlock(r.HostCallbackID, id)
	s.setQuotaRecheckStatus(id, revision, err)
	if err != nil {
		return managementError(statusOf(err), err.Error())
	}
	return managementJSON(http.StatusOK, s.quotaPauseResponse())
}

// quotaRecoveryProof is one model's successful preflight within a recovery
// round. It is re-validated against a fresh clock at commit time.
type quotaRecoveryProof struct {
	view    modelCoverageResponse
	model   string
	started time.Time
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
		return quotaRecheckUnavailable()
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.quotaRecoveryBudget)
	defer cancel()

	now := s.quotaPauseTime()
	s.mu.RLock()
	current, exists := s.creds[id]
	revoked := s.revoked[id]
	billingEpoch := s.billingEpoch[id]
	inflight := s.quotaHasInFlightLocked(current)
	syncPending := s.quotaPauseErrors[id] != ""
	enabled := s.cfg.pauseSettings().enabled
	generation := s.pauseGeneration
	stopped := s.stopped
	s.mu.RUnlock()
	if stopped {
		return quotaRecheckUnavailable()
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
	proofs := make([]quotaRecoveryProof, 0, len(models))
	member := ""
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return localFail(http.StatusConflict, "quota_recheck_timeout", "官方复查超时，请稍后重试")
		}
		if dueOnly && !s.quotaRecheckRoundCurrent(id, generation, billingEpoch) {
			// The operator turned the policy off (or it was turned off and back on)
			// while this round ran; the old round must not continue or commit.
			return localFail(http.StatusConflict, "quota_recheck_not_due", "额度暂停策略已变化，已放弃本轮复查")
		}
		s.mu.RLock()
		clock := s.modelCoverageNow
		s.mu.RUnlock()
		started := clock()
		// A recovery proof must come from a request started by this round, so the
		// read never honors the shared refresh throttle or joins an older attempt.
		view, err := s.readModelCoverageForProof(ctx, id, model, callbackID)
		if err != nil {
			return err
		}
		one := cloneQuotaBlock(block)
		one.ModelID, one.MultipleModels = model, false
		if view.Error != nil {
			switch view.Error.Kind {
			case "unauthorized":
				return localFail(http.StatusConflict, "quota_recheck_unauthorized", "官方拒绝额度授权，请检查 controlKey")
			case "forbidden":
				return localFail(http.StatusConflict, "quota_recheck_forbidden", "官方拒绝访问该 Workspace，请检查额度授权")
			}
		}
		if !quotaRecoveryProven(view, one, id, current.WorkspaceID, started, clock()) {
			return localFail(http.StatusConflict, "quota_recheck_unproven", "本次官方复查未能证明额度已恢复")
		}
		if member != "" && member != view.evidence.workspaceMemberID {
			return localFail(http.StatusConflict, "quota_recheck_identity", "不同模型的官方复查身份不一致")
		}
		member = view.evidence.workspaceMemberID
		proofs = append(proofs, quotaRecoveryProof{view: view, model: model, started: started})
	}

	// Match the global lock order used by model/config updates. No new same-key
	// inference can be admitted while the block remains published in memory.
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	if err := s.commitQuotaRecovery(current, block, proofs, generation, dueOnly); err != nil {
		return err
	}
	return nil
}

// quotaRecheckRoundCurrent reports whether an automatic round may continue. It
// fails closed when the policy changed, the credential identity changed, or the
// block disappeared.
func (s *Service) quotaRecheckRoundCurrent(id string, generation uint64, billingEpoch uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, exists := s.creds[id]
	return exists && !s.revoked[id] && !s.stopped && s.cfg.pauseSettings().enabled &&
		s.pauseGeneration == generation && s.billingEpoch[id] == billingEpoch && c.quotaBlocked()
}

// commitQuotaRecovery writes the cleared block only after re-validating every
// fence at the actual write point: the credential identity and revision, the
// in-flight count, the sync state, the policy generation (for automatic rounds),
// the on-disk identity, and the freshness of every proof. The caller holds
// credentialMu; this function takes quotaCommitGate and authFileMu.
func (s *Service) commitQuotaRecovery(current Credential, block *QuotaBlock, proofs []quotaRecoveryProof, generation uint64, dueOnly bool) error {
	s.quotaCommitGate <- struct{}{}
	defer func() { <-s.quotaCommitGate }()
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()

	now := s.quotaPauseTime()
	s.mu.RLock()
	live, exists := s.creds[current.ID]
	dir, filename := s.authDir, s.authFiles[current.ID]
	stopped := s.stopped
	inflight := s.quotaHasInFlightLocked(live)
	syncPending := s.quotaPauseErrors[current.ID] != ""
	// Capture the policy under the same RLock that reads the live credential. The
	// config writer publishes s.cfg/s.pauseGeneration under mu, so re-reading
	// them after the unlock would race and could accept a round the operator
	// cancelled.
	enabled := s.cfg.pauseSettings().enabled
	liveGeneration := s.pauseGeneration
	clock := s.modelCoverageNow
	s.mu.RUnlock()
	if stopped {
		return quotaRecheckUnavailable()
	}
	if !exists || !live.quotaBlocked() || live.APIKey != current.APIKey ||
		live.ControlKey != current.ControlKey || live.WorkspaceID != current.WorkspaceID ||
		live.runtimeEpoch != current.runtimeEpoch || live.QuotaRevision != current.QuotaRevision ||
		inflight || syncPending {
		return localFail(http.StatusConflict, "quota_state_changed", "额度暂停状态或凭据已变化，请刷新后重试")
	}
	if dueOnly && (!enabled || live.QuotaBlock.RetryAt.After(now) || liveGeneration != generation) {
		return localFail(http.StatusConflict, "quota_recheck_not_due", "额度暂停策略已变化，已放弃本轮复查")
	}
	path, err := credentialFilePath(dir, filename)
	if err != nil {
		return err
	}
	original, fields, disk, err := readQuotaAuthFile(path, current.ID)
	if err != nil {
		return err
	}
	// A direct file edit (or a delayed host rewrite) can change the identity the
	// proof was made for without any watcher callback. Refuse to clear a block
	// the disk no longer describes with this exact identity and revision.
	if disk.APIKey != current.APIKey || disk.QuotaBlock == nil ||
		conflictingQuotaField(disk.ControlKey, current.ControlKey) ||
		conflictingQuotaField(disk.WorkspaceID, current.WorkspaceID) ||
		conflictingQuotaField(disk.QuotaRevision, current.QuotaRevision) {
		return localFail(http.StatusConflict, "quota_state_changed", "凭据文件与本次复查身份不一致，请刷新后重试")
	}
	// Re-check every proof's freshness at the write point: acquiring authFileMu
	// may have taken long enough for the earliest model's evidence to expire.
	finished := clock()
	for _, item := range proofs {
		one := cloneQuotaBlock(block)
		one.ModelID, one.MultipleModels = item.model, false
		if !quotaRecoveryProven(item.view, one, current.ID, current.WorkspaceID, item.started, finished) {
			return localFail(http.StatusConflict, "quota_recheck_unproven", "官方复查结果已过期，请稍后重试")
		}
	}
	expected := quotaCommitSnapshot{
		id: current.ID, apiKey: current.APIKey, controlKey: current.ControlKey,
		workspaceID: current.WorkspaceID, quotaRevision: current.QuotaRevision,
		runtimeEpoch: current.runtimeEpoch,
	}
	// A host can rewrite an older paused body after the console changed the
	// manual choice, without its watcher having run yet. A missing revision is
	// compatible, not evidence that disk's manual enable is newer than live.
	manual := manualChoice(live.QuotaBlock, disk, false)
	return s.publishQuotaResumeLocked(expected, path, original, fields, manual)
}

// setQuotaRecheckStatus stores only allowlisted, non-sensitive reason codes and
// never associates an old result with a newer block revision.
// quotaRecheckBackoff bounds how often the automatic worker retries a round that
// could not prove recovery. A coding-plan window can be days away, so polling the
// official API every ticker would be wasteful; a manual recheck is never delayed.
const (
	quotaRecheckBackoffMin = 5 * time.Minute
	quotaRecheckBackoffMax = 30 * time.Minute
)

// setQuotaRecheckStatus records the fixed reason code of the last automatic or
// manual round and schedules the next automatic attempt.
func (s *Service) setQuotaRecheckStatus(id, revision string, err error) {
	code := ""
	if err != nil {
		code = "error"
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr != nil {
			switch apiErr.Kind {
			case "quota_recheck_inflight":
				code = "inflight"
			case "quota_sync_pending":
				code = "sync_pending"
			case "quota_recheck_unconfigured":
				code = "unconfigured"
			case "quota_recheck_ambiguous":
				code = "ambiguous"
			case "quota_recheck_unauthorized":
				code = "unauthorized"
			case "quota_recheck_forbidden":
				code = "forbidden"
			case "quota_recheck_identity":
				code = "identity"
			case "quota_recheck_unproven":
				code = "unproven"
			case "quota_state_changed", "quota_recheck_not_due", "plugin_unavailable":
				code = ""
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, exists := s.creds[id]
	if !exists || !c.quotaBlocked() || c.QuotaRevision != revision {
		return
	}
	if code == "" {
		delete(s.quotaRecheckStatus, id)
		delete(s.quotaRecheckRetryAt, id)
		delete(s.quotaRecheckFailures, id)
		return
	}
	s.quotaRecheckStatus[id] = code
	// Escalate the retry delay with consecutive failures so a long exhaustion is
	// polled slowly. Nothing else limits this: the block itself still gates
	// scheduling, and the recorded deadline is only a review hint. The shift is
	// capped before it can overflow int64 (26 doublings of 5m already exceed it),
	// which would otherwise produce a negative delay and retry every tick.
	if s.quotaRecheckFailures[id] < 32 {
		s.quotaRecheckFailures[id]++
	}
	delay := quotaRecheckBackoffMax
	if failures := s.quotaRecheckFailures[id]; failures > 0 && failures <= 8 {
		if grown := quotaRecheckBackoffMin << (failures - 1); grown > 0 && grown < delay {
			delay = grown
		}
	}
	// mu is already held, so read the injected clock directly instead of calling
	// quotaPauseTime, which takes the same lock.
	now := s.quotaNow()
	if remaining := c.QuotaBlock.RetryAt.Sub(now); remaining > delay {
		// A later upstream reset hint is the better estimate; cap it anyway.
		delay = remaining
		if delay > quotaRecheckBackoffMax {
			delay = quotaRecheckBackoffMax
		}
	}
	s.quotaRecheckRetryAt[id] = now.Add(delay)
}

func (s *Service) wakeQuotaPauseWorker() {
	select {
	case s.quotaPauseWake <- struct{}{}:
	default:
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

// sweepQuotaBlocks repairs auth files that no longer describe the published
// state, in both directions, without treating a deadline as proof of restored
// quota. It performs no network requests.
func (s *Service) sweepQuotaBlocks(stop <-chan struct{}) {
	type candidate struct {
		id, revision string
		blocked      bool
	}
	s.mu.RLock()
	var candidates []candidate
	for id, c := range s.creds {
		if s.revoked[id] {
			continue
		}
		blocked := c.quotaBlocked()
		if (blocked && s.quotaPauseErrors[id] != "") || s.quotaDiskStale[id] {
			candidates = append(candidates, candidate{id: id, revision: c.QuotaRevision, blocked: blocked})
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
		if item.blocked {
			_ = s.persistQuotaBlockLocked(item.id, item.revision, false)
		} else {
			// The live state is "no block", so the file may only be repaired by
			// republishing that same cleared state. This is not a resume: it never
			// removes a live block and never needs recovery evidence, it only
			// prevents a stale paused body from resurrecting after a restart.
			_ = s.persistQuotaResumeLocked(item.id)
		}
		s.credentialMu.Unlock()
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
	type candidate struct{ id, revision string }
	var candidates []candidate
	for id, c := range s.creds {
		if !s.revoked[id] && c.quotaBlocked() && !c.QuotaBlock.RetryAt.After(now) && s.quotaPauseErrors[id] == "" &&
			!s.quotaRecheckRetryAt[id].After(now) {
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
		err := s.recheckQuotaBlockMode("", item.id, true)
		s.setQuotaRecheckStatus(item.id, item.revision, err)
	}
}
