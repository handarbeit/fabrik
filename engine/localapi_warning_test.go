//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tui"
	"github.com/handarbeit/fabrik/warnings"
)

const localAPIWarningKey = "local_api:unavailable"

// blockSocketPath puts a regular file where the local API socket belongs, so
// srv.Start() refuses to bind (it never replaces a non-socket file).
func blockSocketPath(t *testing.T, eng *Engine) string {
	t.Helper()
	path := localapi.SocketPath(eng.fabrikDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func localAPIPanelEntries(t *testing.T) []warnings.Entry {
	t.Helper()
	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// A local API that fails to start is one panel entry naming the path and the
// error; repeated failures keep one entry; a later successful start clears it.
func TestStartLocalAPI_FailureRecordsPanelEntryAndSuccessResolvesIt(t *testing.T) {
	isolateWarningsFile(t)
	eng := lifecycleEngine(t)
	events := make(chan tui.Event, 32)
	eng.events = events
	t.Cleanup(eng.closeLocalAPI)
	path := blockSocketPath(t, eng)

	if got := localAPIPanelEntries(t); len(got) != 0 {
		t.Fatalf("panel not empty before the failing start: %+v", got)
	}

	eng.startLocalAPI()

	got := localAPIPanelEntries(t)
	if len(got) != 1 || got[0].Key != localAPIWarningKey {
		t.Fatalf("panel entries after failed start = %+v, want exactly %s", got, localAPIWarningKey)
	}
	for _, want := range []string{path, "fabrik mcp", "not running", "directory", "permissions", "never replaced"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, got[0].Detail)
		}
	}
	if got[0].FixAction != "" {
		t.Errorf("FixAction = %q, want empty", got[0].FixAction)
	}
	if eng.localAPI != nil {
		t.Error("a failed start must leave no server behind")
	}

	// FR-004: one existing log line per failure.
	logged := 0
	drain := func() {
		for {
			select {
			case ev := <-events:
				if le, ok := ev.(tui.LogEvent); ok && strings.Contains(le.Message, "local API unavailable (continuing without it)") {
					logged++
				}
			default:
				return
			}
		}
	}
	drain()
	if logged != 1 {
		t.Errorf("failure log lines = %d, want 1", logged)
	}

	// A second failure keeps one keyed entry.
	eng.startLocalAPI()
	if got := localAPIPanelEntries(t); len(got) != 1 {
		t.Fatalf("panel entries after repeated failure = %+v, want 1", got)
	}

	// Fixing the cause and starting again resolves the entry.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	eng.startLocalAPI()
	if eng.localAPI == nil {
		t.Fatal("start should have succeeded once the file was removed")
	}
	if got := localAPIPanelEntries(t); len(got) != 0 {
		t.Errorf("panel entries after a successful start = %+v, want none", got)
	}
}

// A stale entry left by an earlier process is cleared by the next successful
// start, even though this process never failed.
func TestStartLocalAPI_SuccessClearsEntryFromEarlierProcess(t *testing.T) {
	isolateWarningsFile(t)
	eng := lifecycleEngine(t)
	t.Cleanup(eng.closeLocalAPI)

	if err := warnings.Report(nil, localAPIUnavailableCondition("/old/path.sock", os.ErrPermission)); err != nil {
		t.Fatal(err)
	}
	if got := localAPIPanelEntries(t); len(got) != 1 {
		t.Fatalf("setup: entries = %+v", got)
	}

	eng.startLocalAPI()
	if got := localAPIPanelEntries(t); len(got) != 0 {
		t.Errorf("stale entry survived a successful start: %+v", got)
	}
}

// A successful start with nothing recorded neither records nor errors.
func TestStartLocalAPI_SuccessWithNoEntryRecordsNothing(t *testing.T) {
	isolateWarningsFile(t)
	eng := lifecycleEngine(t)
	t.Cleanup(eng.closeLocalAPI)

	eng.startLocalAPI()
	if eng.localAPI == nil {
		t.Fatal("start should succeed")
	}
	if got := localAPIPanelEntries(t); len(got) != 0 {
		t.Errorf("entries = %+v, want none", got)
	}
}

// The fingerprint is the socket path, so a different error string for the same
// path is the same dismissible condition.
func TestLocalAPIUnavailableCondition_FingerprintIsThePath(t *testing.T) {
	a := localAPIUnavailableCondition("/p/fabrik.sock", os.ErrPermission)
	b := localAPIUnavailableCondition("/p/fabrik.sock", os.ErrExist)
	if a.Fingerprint != "/p/fabrik.sock" || a.Fingerprint != b.Fingerprint {
		t.Errorf("fingerprints = %q, %q", a.Fingerprint, b.Fingerprint)
	}
	if a.Key() != localAPIWarningKey {
		t.Errorf("key = %q", a.Key())
	}
}
