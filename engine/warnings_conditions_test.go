package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/testenv"
	"github.com/handarbeit/fabrik/internal/workerenv"
	"github.com/handarbeit/fabrik/tui"
	"github.com/handarbeit/fabrik/warnings"
)

// Tests for the operator-level conditions routed to the warnings panel by
// #2135 (ADR 2135): each is recorded when the condition holds and clears
// itself when it no longer does, and a repeated evaluation yields one entry.

func warningDetail(t *testing.T, key string) string {
	t.Helper()
	es, err := warnings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range es {
		if e.Key == key {
			return e.Detail
		}
	}
	t.Fatalf("no warning %q", key)
	return ""
}

func isolateWarnings(t *testing.T) {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
}

func warningKeys(t *testing.T) map[string]bool {
	t.Helper()
	es, err := warnings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	keys := map[string]bool{}
	for _, e := range es {
		keys[e.Key] = true
	}
	return keys
}

func TestTimeoutOrdering_CIBackstop_RecordsAndClears(t *testing.T) {
	isolateWarnings(t)
	var buf bytes.Buffer
	for i := 0; i < 3; i++ { // recurring: one entry
		warnCIBackstopTimeoutOrdering(time.Hour, time.Minute, &buf)
	}
	if k := warningKeys(t); len(k) != 1 || !k["timeout_ordering:ci_backstop"] {
		t.Fatalf("keys = %v", k)
	}
	if !strings.Contains(buf.String(), "[startup] warning: CIBackstopTimeout") {
		t.Errorf("log = %q", buf.String())
	}
	warnCIBackstopTimeoutOrdering(time.Minute, time.Hour, &buf)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared once valid: %v", k)
	}
}

func TestTimeoutOrdering_DrainDeadline_RecordsAndClears(t *testing.T) {
	isolateWarnings(t)
	var buf bytes.Buffer
	warnDrainDeadlineOrdering(time.Second, 10*time.Second, 10*time.Second, &buf)
	if k := warningKeys(t); !k["timeout_ordering:drain_deadline"] {
		t.Fatalf("keys = %v", k)
	}
	warnDrainDeadlineOrdering(time.Minute, 10*time.Second, 10*time.Second, &buf)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared once valid: %v", k)
	}
}

func TestLogFileConditions_RecordAndClear(t *testing.T) {
	isolateWarnings(t)
	var buf bytes.Buffer
	conds := logFileConditions("/x/fabrik.log", []string{"rename failed"}, errors.New("permission denied"))
	if len(conds) != 2 {
		t.Fatalf("conds = %d, want 2", len(conds))
	}
	if err := warnings.Reconcile(&buf, "log_file", conds); err != nil {
		t.Fatal(err)
	}
	k := warningKeys(t)
	if !k["log_file:rotation"] || !k["log_file:open"] {
		t.Fatalf("keys = %v", k)
	}
	// Fixed on the next start: both checks pass, so Reconcile clears both.
	if err := warnings.Reconcile(&buf, "log_file", logFileConditions("/x/fabrik.log", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
}

func TestCheckHTTPSCredentials_RecordsAndClears(t *testing.T) {
	isolateWarnings(t)
	testenv.IsolateGit(t, "")
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.GitSSH = false

	eng.checkHTTPSCredentials(false)
	eng.checkHTTPSCredentials(false) // recurring: one entry
	if k := warningKeys(t); len(k) != 1 || !k["git_credentials:https"] {
		t.Fatalf("keys = %v", k)
	}

	// Fixed: a helper is configured.
	testenv.IsolateGit(t, "[credential]\n\thelper = store\n")
	eng.checkHTTPSCredentials(false)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared once a helper is configured: %v", k)
	}

	// Not applicable (SSH) also clears a stale entry.
	_ = warnings.Record(warnings.Entry{Key: "git_credentials:https", Type: "git_credentials", Title: "stale"})
	eng.cfg.GitSSH = true
	eng.checkHTTPSCredentials(false)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared under SSH: %v", k)
	}
}

func TestCheckAPIKeyHelper_UnparseableSettingsRecordsAndClears(t *testing.T) {
	isolateWarnings(t)
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()

	bad := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := eng.checkAPIKeyHelper(); err != nil {
		t.Fatalf("unparseable file must not fail startup: %v", err)
	}
	if k := warningKeys(t); !k["api_key_helper_check:settings"] {
		t.Fatalf("keys = %v", k)
	}

	if err := os.WriteFile(bad, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := eng.checkAPIKeyHelper(); err != nil {
		t.Fatal(err)
	}
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared once the file parses: %v", k)
	}
}

func TestLogWorkerEnvOptIn_IgnoredAndDroppedRecordAndClear(t *testing.T) {
	isolateWarnings(t)
	var buf bytes.Buffer
	logWorkerEnvOptIn(workerenv.Resolved{Ignored: []string{"PATH"}, Dropped: []string{"HD_KEY"}}, &buf)
	logWorkerEnvOptIn(workerenv.Resolved{Ignored: []string{"PATH"}, Dropped: []string{"HD_KEY"}}, &buf)
	k := warningKeys(t)
	if len(k) != 2 || !k["worker_env:ignored"] || !k["worker_env:hookdeck_dropped"] {
		t.Fatalf("keys = %v", k)
	}
	logWorkerEnvOptIn(workerenv.Resolved{}, &buf)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
}

func TestCheckWebhookHookCoverage_PanelRecordsAndClears(t *testing.T) {
	isolateWarnings(t)
	wm, events := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.repos = map[string]bool{"a/one": true, "b/two": true}
	wm.emitFn = func(e tui.Event) { events <- e }
	wm.mu.Unlock()

	hasHook := false
	e := &Engine{client: &mockGitHubClient{
		hasForwardingHookFn: func(owner, repo string) (bool, error) { return hasHook, nil },
	}}
	e.checkWebhookHookCoverage(wm)
	e.checkWebhookHookCoverage(wm)
	if k := warningKeys(t); len(k) != 1 || !k["webhook_coverage:repos"] {
		t.Fatalf("keys = %v", k)
	}
	hasHook = true
	e.checkWebhookHookCoverage(wm)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
}

func TestCheckWebhookHookCoverage_LookupErrorIsNotRecordedAsMissing(t *testing.T) {
	isolateWarnings(t)
	wm, events := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.repos = map[string]bool{"a/one": true, "b/two": true}
	wm.emitFn = func(e tui.Event) { events <- e }
	wm.mu.Unlock()

	// b/two's lookup errors (transient); a/one is confirmed missing.
	e := &Engine{client: &mockGitHubClient{
		hasForwardingHookFn: func(owner, repo string) (bool, error) {
			if repo == "two" {
				return false, errors.New("502 bad gateway")
			}
			return false, nil
		},
	}}
	e.checkWebhookHookCoverage(wm)
	if k := warningKeys(t); len(k) != 1 || !k["webhook_coverage:repos"] {
		t.Fatalf("keys = %v", k)
	}
	if d := warningDetail(t, "webhook_coverage:repos"); strings.Contains(d, "b/two") || !strings.Contains(d, "a/one") {
		t.Errorf("detail should list only the confirmed-missing repo: %q", d)
	}

	// Only an errored lookup remains: the prior entry must be left alone, not
	// cleared and not rewritten with the unknown repo.
	e.client = &mockGitHubClient{
		hasForwardingHookFn: func(owner, repo string) (bool, error) {
			if repo == "two" {
				return false, errors.New("502 bad gateway")
			}
			return true, nil
		},
	}
	e.checkWebhookHookCoverage(wm)
	if k := warningKeys(t); len(k) != 1 || !k["webhook_coverage:repos"] {
		t.Fatalf("prior entry should be untouched on an incomplete check, keys = %v", k)
	}

	// A complete, clean check clears it.
	e.client = &mockGitHubClient{
		hasForwardingHookFn: func(owner, repo string) (bool, error) { return true, nil },
	}
	e.checkWebhookHookCoverage(wm)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
}

func TestCheckWebhookHookCoverage_OnlyLookupErrorsRecordsNothing(t *testing.T) {
	isolateWarnings(t)
	wm, events := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.repos = map[string]bool{"a/one": true}
	wm.emitFn = func(e tui.Event) { events <- e }
	wm.mu.Unlock()
	e := &Engine{client: &mockGitHubClient{
		hasForwardingHookFn: func(owner, repo string) (bool, error) { return false, errors.New("rate limited") },
	}}
	e.checkWebhookHookCoverage(wm)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("transient lookup error must not create a panel entry: %v", k)
	}
}

func TestCheckHookdeckInstallationCoverage_PanelRecordsAndClears(t *testing.T) {
	isolateWarnings(t)
	orig := fetchInstallationRepositoriesFn
	defer func() { fetchInstallationRepositoriesFn = orig }()
	granted := []string{"a/one"}
	fetchInstallationRepositoriesFn = func(baseURL, token string) ([]string, bool, error) {
		return granted, false, nil
	}
	hm, _ := newTestHookdeckManager(t)
	hm.UpdateRepos(map[string]bool{"a/one": true, "b/two": true})
	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}

	e.checkHookdeckInstallationCoverage(hm)
	if k := warningKeys(t); len(k) != 1 || !k["hookdeck_coverage:repos"] {
		t.Fatalf("keys = %v", k)
	}
	granted = []string{"a/one", "b/two"}
	e.checkHookdeckInstallationCoverage(hm)
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
}
