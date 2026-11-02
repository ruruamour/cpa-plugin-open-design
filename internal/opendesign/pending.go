package opendesign

import (
	"path/filepath"
	"strings"
)

// pendingSave is the credential body currently being handed to host.auth.save.
//
// CPA saves an auth file in this order (internal/pluginhost/auth_callbacks.go,
// callHostAuthSave → saveAuthFile):
//
//	buildAuthsFromFileData(ctx, dst, newData)   → re-enters auth.parse
//	os.WriteFile(dst, newData, 0600)            → disk only updated here
//	upsertAuthRecordsForPath(...)
//
// auth.parse therefore runs while the file on disk still holds the *previous*
// content. Re-reading disk at that point would resurrect stale fields — notably
// `disabled: true` when the operator just enabled the credential, leaving the
// host record disabled while the console shows it enabled.
//
// The fix is to publish the in-flight body before the host call and use it, for
// this file only, instead of the stale disk copy. The watcher's stale-callback
// protection (s.revoked) is untouched: a pending snapshot never revives a
// deleted credential.
type pendingSave struct {
	id       string
	filename string
	body     []byte
}

// mirrorPending records a body about to be saved by host.auth.save.
func (s *Service) mirrorPending(filename string, body []byte, id string) {
	if filename == "" || len(body) == 0 {
		return
	}
	s.pendingMu.Lock()
	s.pendingSaves[filename] = pendingSave{id: id, filename: filename, body: append([]byte(nil), body...)}
	if id != "" {
		s.pendingSaves[id] = pendingSave{id: id, filename: filename, body: append([]byte(nil), body...)}
	}
	s.pendingMu.Unlock()
}

// clearPending forgets a finished (or failed) save.
func (s *Service) clearPending(filename, id string) {
	s.pendingMu.Lock()
	delete(s.pendingSaves, filename)
	if id != "" {
		if entry, ok := s.pendingSaves[id]; !ok || entry.filename == filename {
			delete(s.pendingSaves, id)
		}
	}
	s.pendingMu.Unlock()
}

// pendingFor resolves the in-flight body for the auth material being parsed.
//
// CPA passes FileName as authIDForPath(path), which is normally the file's base
// name but can be the credential ID instead, so match every key a host may use:
// the exact file name, its base name, "<base>.json", or the credential ID.
// Only bodies saved through saveCredential live here, so a match is always a
// writer this plugin started; a watcher callback can never be satisfied by it
// unless a save for that very credential is in flight.
func (s *Service) pendingFor(candidates ...string) ([]byte, bool) {
	keys := make([]string, 0, len(candidates)*2)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		keys = append(keys, candidate)
		if base := filepath.Base(candidate); base != "" && base != candidate {
			keys = append(keys, base)
		} else {
			keys = append(keys, candidate+".json")
		}
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for _, key := range keys {
		if entry, ok := s.pendingSaves[key]; ok {
			return append([]byte(nil), entry.body...), true
		}
	}
	return nil, false
}
