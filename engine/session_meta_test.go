package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func readSizeFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "session_size", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func TestLastCallContextTokens_LastMessageNotSum(t *testing.T) {
	got, ok := lastCallContextTokens(readSizeFixture(t, "multi_assistant.ndjson"), "sess-A")
	if !ok || got != 1+200+60000 {
		t.Fatalf("got (%d,%v), want (60201,true): the last call's figure, not the sum or the result's cumulative usage", got, ok)
	}
}

func TestLastCallContextTokens_SidechainIgnored(t *testing.T) {
	got, ok := lastCallContextTokens(readSizeFixture(t, "sidechain_last.ndjson"), "sess-A")
	if !ok || got != 2+500+50000 {
		t.Fatalf("got (%d,%v), want (50502,true): the sub-agent line must be skipped", got, ok)
	}
}

func TestLastCallContextTokens_MissingFieldsUnknownNotZero(t *testing.T) {
	if got, ok := lastCallContextTokens(readSizeFixture(t, "missing_fields.ndjson"), "sess-A"); ok {
		t.Fatalf("got (%d,true), want unknown when cache fields are absent", got)
	}
}

func TestLastCallContextTokens_OtherSessionIgnored(t *testing.T) {
	if got, ok := lastCallContextTokens(readSizeFixture(t, "other_session.ndjson"), "sess-A"); ok {
		t.Fatalf("got (%d,true), want unknown for another session's lines", got)
	}
	if _, ok := lastCallContextTokens(readSizeFixture(t, "multi_assistant.ndjson"), ""); ok {
		t.Fatal("empty session id must be unknown")
	}
}

func sessionMetaTestPath(t *testing.T, id string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Implement.session")
	if err := os.WriteFile(p, []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRecordSessionContextSize_PersistsBoundToSession(t *testing.T) {
	p := sessionMetaTestPath(t, "sess-A")
	recordSessionContextSize(1, p, readSizeFixture(t, "multi_assistant.ndjson"))
	if tok, ok := savedSessionContextTokens(p); !ok || tok != 60201 {
		t.Fatalf("saved = (%d,%v), want (60201,true)", tok, ok)
	}
	info, err := os.Stat(sessionMetaPath(p))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("meta file mode = %v err=%v, want 0600", info, err)
	}
	// The pointer moves to another session: the old size must not be paired with it.
	if err := os.WriteFile(p, []byte("sess-B"), 0600); err != nil {
		t.Fatal(err)
	}
	if tok, ok := savedSessionContextTokens(p); ok {
		t.Fatalf("stale size %d paired with a new session id", tok)
	}
}

func TestRecordSessionContextSize_ReplacedWithSession(t *testing.T) {
	p := sessionMetaTestPath(t, "sess-A")
	recordSessionContextSize(1, p, readSizeFixture(t, "multi_assistant.ndjson"))
	if err := os.WriteFile(p, []byte("sess-B"), 0600); err != nil {
		t.Fatal(err)
	}
	recordSessionContextSize(1, p, readSizeFixture(t, "other_session.ndjson"))
	if tok, ok := savedSessionContextTokens(p); !ok || tok != 2+500+50000 {
		t.Fatalf("saved = (%d,%v), want the new session's 50502", tok, ok)
	}
}

func TestRecordSessionContextSize_NoSessionFileIsNoop(t *testing.T) {
	p := filepath.Join(t.TempDir(), "Implement.session")
	recordSessionContextSize(1, p, readSizeFixture(t, "multi_assistant.ndjson"))
	if _, err := os.Stat(sessionMetaPath(p)); !os.IsNotExist(err) {
		t.Fatalf("meta written without a session file: %v", err)
	}
}

func TestUpdateSessionMeta_PreservesOtherFields(t *testing.T) {
	p := sessionMetaTestPath(t, "sess-A")
	recordSessionContextSize(1, p, readSizeFixture(t, "multi_assistant.ndjson"))
	updateSessionMeta(1, p, func(m *sessionMeta) { m.Closed = true })
	updateSessionMeta(1, p, func(m *sessionMeta) { m.Reset = true })
	m, found := readSessionMeta(p)
	if !found || !m.Closed || !m.Reset || m.ContextTokens == nil || *m.ContextTokens != 60201 {
		t.Fatalf("meta = %+v found=%v, want closed+reset+size kept", m, found)
	}
	// A later run's size write keeps the flags.
	recordSessionContextSize(1, p, readSizeFixture(t, "multi_assistant.ndjson"))
	if m, _ := readSessionMeta(p); !m.Closed || !m.Reset {
		t.Fatalf("size write dropped flags: %+v", m)
	}
}

func TestSessionMeta_CorruptIsAbsent(t *testing.T) {
	p := sessionMetaTestPath(t, "sess-A")
	if err := os.WriteFile(sessionMetaPath(p), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found := readSessionMeta(p); found {
		t.Fatal("corrupt meta must read as absent")
	}
	if _, ok := savedSessionContextTokens(p); ok {
		t.Fatal("corrupt meta must read as unknown size")
	}
}

func TestClassifyResumeFailure_AbandonmentRemovesMeta(t *testing.T) {
	p := sessionMetaTestPath(t, "sess-A")
	recordSessionContextSize(1, p, readSizeFixture(t, "multi_assistant.ndjson"))
	classifyResumeFailure(1, p, "sess-A", 1, os.ErrInvalid)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("session pointer should be abandoned")
	}
	if _, err := os.Stat(sessionMetaPath(p)); !os.IsNotExist(err) {
		t.Fatal("abandonment must remove the meta with the pointer")
	}
}

func TestInterpretClaudeResult_StaleSession_RemovesMeta(t *testing.T) {
	p := sessionMetaTestPath(t, "3fefda47-9ea5-422a-9cdb-33da6e13244d")
	updateSessionMeta(1, p, func(m *sessionMeta) { m.Closed = true })
	raw := []byte(`{"subtype":"error_during_execution","session_id":"3fefda47-9ea5-422a-9cdb-33da6e13244d","is_error":true,` +
		`"errors":["No conversation found with session ID: 3fefda47-9ea5-422a-9cdb-33da6e13244d"]}`)
	if _, _, _, err := interpretClaudeResult(context.Background(), 815, raw, nil, false, p, t.TempDir(), "", 2, -1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(sessionMetaPath(p)); !os.IsNotExist(err) {
		t.Fatalf("stale-session removal must remove the meta, stat err = %v", err)
	}
}
