package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// sessionMeta is the durable per-session record kept in a JSON sidecar next to
// a stage's .session pointer (ADR 2300), following the .resumefails precedent:
// plain sibling file, mode 0600, restart-safe, best-effort.
//
//   - SessionID / ContextTokens: the context size of the session's last
//     main-thread API call (input + cache-read + cache-creation tokens) and the
//     session it was measured for. A reader only trusts ContextTokens when
//     SessionID equals the pointer's current ID, so a size can never be paired
//     with a different session. nil ContextTokens means unknown (never 0).
//   - Closed: the last stage-dispatch run of this stage ended with
//     FABRIK_STAGE_COMPLETE. A session that is not closed is a continuation
//     (time slice, incomplete or errored run, blocked on input); a closed one
//     is a re-entry. Comment-family runs never change it.
//   - StageRun: a stage dispatch has recorded its outcome in Closed. A record
//     created only by the comment path (a pre-ADR-2300 session that went through
//     a comment-review pass) has StageRun false, so Closed carries no
//     information and the continuation decision falls back to the legacy rule.
//   - Reset: a one-shot "the next stage dispatch starts fresh" flag, set where
//     the engine explicitly resets a stage (clearFailedStage, reenterValidate,
//     handleValidateSHAInvalidation, unblockAwaitingInput).
type sessionMeta struct {
	SessionID     string `json:"session_id,omitempty"`
	ContextTokens *int   `json:"context_tokens,omitempty"`
	Closed        bool   `json:"closed,omitempty"`
	StageRun      bool   `json:"stage_run,omitempty"`
	Reset         bool   `json:"reset,omitempty"`
}

// sessionMetaMu serialises read-modify-write of the sidecar. Writers are
// already sequential per item; this only guards two items sharing a process
// from interleaving a tmp+rename on one path (they never share one in practice).
var sessionMetaMu sync.Mutex

// sessionMetaPath returns the sidecar path for the session at sessFilePath.
func sessionMetaPath(sessFilePath string) string {
	return sessFilePath + ".meta"
}

// readSessionMeta reads the sidecar. A missing, unreadable or unparseable file
// is "no record" (found == false): the same absence-means-unknown convention as
// the session file and .resumefails.
func readSessionMeta(sessFilePath string) (m sessionMeta, found bool) {
	data, err := os.ReadFile(sessionMetaPath(sessFilePath))
	if err != nil {
		return sessionMeta{}, false
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return sessionMeta{}, false
	}
	return m, true
}

func writeSessionMeta(issueNumber int, sessFilePath string, m sessionMeta) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	path := sessionMetaPath(sessFilePath)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		claudeLog(issueNumber, "warn", "failed to save session meta to %s: %v\n", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		claudeLog(issueNumber, "warn", "failed to save session meta to %s: %v\n", path, err)
	}
}

// removeSessionMeta discards the sidecar. It must sit next to every
// os.Remove(sessFilePath) (resume-failure abandonment, stale session): the
// pointer is gone, so the record describes nothing.
func removeSessionMeta(sessFilePath string) {
	os.Remove(sessionMetaPath(sessFilePath))
}

// updateSessionMeta applies fn to the current record and writes it back. It is
// a no-op when there is no session file: a record without a session describes
// nothing, and the next run that creates one starts from the legacy rule.
func updateSessionMeta(issueNumber int, sessFilePath string, fn func(*sessionMeta)) {
	if _, err := os.Stat(sessFilePath); err != nil {
		return
	}
	sessionMetaMu.Lock()
	defer sessionMetaMu.Unlock()
	m, _ := readSessionMeta(sessFilePath)
	fn(&m)
	writeSessionMeta(issueNumber, sessFilePath, m)
}

// savedSessionContextTokens returns the recorded context size for the session
// currently pointed at by sessFilePath, if it is known and bound to that
// session ID.
func savedSessionContextTokens(sessFilePath string) (tokens int, known bool) {
	id, status, _ := readSessionIDForResume(sessFilePath)
	if status != resumeFound {
		return 0, false
	}
	m, found := readSessionMeta(sessFilePath)
	if !found || m.ContextTokens == nil || m.SessionID != id {
		return 0, false
	}
	return *m.ContextTokens, true
}

// streamAssistantUsageLine is the slice of one stream-json line that carries a
// single API call's usage.
type streamAssistantUsageLine struct {
	Type            string  `json:"type"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		Usage *struct {
			InputTokens         *int `json:"input_tokens"`
			CacheCreationTokens *int `json:"cache_creation_input_tokens"`
			CacheReadTokens     *int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// lastCallContextTokens returns the context size of the session's last
// main-thread API call in the raw stream-json: input + cache-read +
// cache-creation tokens of the last assistant line of sessionID. It is the
// size a resume starts from — never the cumulative total of the run.
// Sub-agent lines (parent_tool_use_id set), lines of another session, lines
// missing any of the three usage fields and all-zero (synthetic) lines are
// skipped; with no usable line the size is unknown (known == false), never 0.
func lastCallContextTokens(rawOutput []byte, sessionID string) (tokens int, known bool) {
	if sessionID == "" {
		return 0, false
	}
	lines := bytes.Split(rawOutput, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.Contains(line, []byte(`"assistant"`)) {
			continue
		}
		var l streamAssistantUsageLine
		if err := json.Unmarshal(line, &l); err != nil || l.Type != "assistant" {
			continue
		}
		if l.ParentToolUseID != nil && *l.ParentToolUseID != "" {
			continue
		}
		if l.SessionID != sessionID {
			continue
		}
		u := l.Message.Usage
		if u == nil || u.InputTokens == nil || u.CacheCreationTokens == nil || u.CacheReadTokens == nil {
			continue
		}
		sum := *u.InputTokens + *u.CacheCreationTokens + *u.CacheReadTokens
		if sum <= 0 {
			continue
		}
		return sum, true
	}
	return 0, false
}

// recordSessionContextSize persists the last-call context size next to the
// session pointer after a run. The size is paired with the session ID read
// back from the pointer and only taken from stream lines of that same session,
// so a run that did not leave this session as the pointer's target (a fresh
// session killed before its result) can never write its size against another
// session's ID. Best-effort; no session file or no usable line is a no-op,
// and the reader ignores a size whose ID no longer matches.
func recordSessionContextSize(issueNumber int, sessFilePath string, rawOutput []byte) {
	id, status, _ := readSessionIDForResume(sessFilePath)
	if status != resumeFound {
		return
	}
	tokens, known := lastCallContextTokens(rawOutput, strings.TrimSpace(id))
	if !known {
		updateSessionMeta(issueNumber, sessFilePath, func(m *sessionMeta) {
			if m.SessionID != id {
				m.SessionID = id
				m.ContextTokens = nil
			}
		})
		return
	}
	updateSessionMeta(issueNumber, sessFilePath, func(m *sessionMeta) {
		m.SessionID = id
		m.ContextTokens = &tokens
	})
}
