package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func testOutcomeRecord() *stageOutcomeRecord {
	return &stageOutcomeRecord{
		Kind: outcomeKindStage, Phase: outcomePhaseReady,
		Repo: "owner/repo", Number: 7, Stage: "Research", HeadSHA: "abc123def456",
		Nonce: "n0nce", PostOutput: "findings",
	}
}

func TestStageOutcomeStore_RoundTripAndReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stage-outcomes")
	s := newStageOutcomeStore(dir)
	rec := testOutcomeRecord()
	rec.PRCommentDone = true
	if err := s.put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}

	s2 := newStageOutcomeStore(dir)
	if notes := s2.load(); len(notes) != 0 {
		t.Fatalf("load notes = %v, want none", notes)
	}
	got := s2.get(rec.key())
	if got == nil || got.Stage != "Research" || got.HeadSHA != "abc123def456" || !got.PRCommentDone || got.Nonce != "n0nce" {
		t.Fatalf("reloaded record = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, stageOutcomeFileName(rec.key())))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record file mode = %v (err %v), want 0600", info, err)
	}
	if !s2.remove(rec.key()) || s2.get(rec.key()) != nil {
		t.Fatal("remove did not drop the record")
	}
	if _, err := os.Stat(filepath.Join(dir, stageOutcomeFileName(rec.key()))); !os.IsNotExist(err) {
		t.Fatalf("record file still present after remove: %v", err)
	}
}

// FR-010: an unusable record is quarantined and treated as absent, never fatal.
func TestStageOutcomeStore_UnusableFilesAreQuarantined(t *testing.T) {
	dir := t.TempDir()
	good := testOutcomeRecord()
	s := newStageOutcomeStore(dir)
	if err := s.put(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"corrupt.json":                "{not json",
		"badversion.json":             `{"version": 99, "repo": "o/r", "number": 1, "stage": "X"}`,
		"noitem.json":                 `{"version": 1, "stage": "X"}`,
		stageOutcomeFileName("o/r#1"): `{"version": 1, "repo": "o/r", "number": 2, "stage": "X"}`,
	}
	for name, body := range bad {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s2 := newStageOutcomeStore(dir)
	notes := s2.load()
	if len(notes) != len(bad) {
		t.Fatalf("notes = %v, want one per unusable file (%d)", notes, len(bad))
	}
	if got := s2.snapshot(); len(got) != 1 || got[0].key() != good.key() {
		t.Fatalf("snapshot = %+v, want only the good record", got)
	}
	for name := range bad {
		if _, err := os.Stat(filepath.Join(dir, name+".corrupt")); err != nil {
			t.Errorf("%s was not quarantined: %v", name, err)
		}
	}
}

func TestStageOutcomeStore_NilStoreRecordsNothing(t *testing.T) {
	var s *stageOutcomeStore
	if err := s.put(testOutcomeRecord()); err != nil {
		t.Fatalf("nil put: %v", err)
	}
	if s.get("owner/repo#7") != nil || s.remove("owner/repo#7") || len(s.snapshot()) != 0 || len(s.load()) != 0 {
		t.Fatal("a nil store must be inert")
	}
	if newStageOutcomeStore("") != nil {
		t.Fatal("an empty directory must give a nil store")
	}
}

// R3: every reason a record must not be published.
func TestOutcomeDiscardReason(t *testing.T) {
	research := &stages.Stage{Name: "Research"}
	plan := &stages.Stage{Name: "Plan"}
	validate := &stages.Stage{Name: "Validate"}
	live := func() gh.ProjectItem { return gh.ProjectItem{Number: 7, Status: "Research"} }
	cases := []struct {
		name    string
		mutate  func(*stageOutcomeRecord, *gh.ProjectItem)
		stage   *stages.Stage
		head    string
		headErr error
		want    string // substring; "" = publishable
	}{
		{"publishable", nil, research, "abc123def456", nil, ""},
		{"closed", func(_ *stageOutcomeRecord, i *gh.ProjectItem) { i.IsClosed = true }, research, "abc123def456", nil, "closed"},
		{"paused", func(_ *stageOutcomeRecord, i *gh.ProjectItem) { i.Labels = []string{"fabrik:paused"} }, research, "abc123def456", nil, "paused"},
		{"moved column", func(_ *stageOutcomeRecord, i *gh.ProjectItem) { i.Status = "Plan" }, plan, "abc123def456", nil, "moved to column"},
		{"no stage for the column", nil, nil, "abc123def456", nil, "moved to column"},
		{"interrupted before checkpoint", func(r *stageOutcomeRecord, _ *gh.ProjectItem) { r.Phase = outcomePhaseRecorded }, research, "abc123def456", nil, "interrupted"},
		{"head moved", nil, research, "ffff00001111", nil, "no longer matches"},
		{"worktree gone", nil, research, "", errors.New("no worktree"), "cannot be read"},
		{"revalidate requested", func(r *stageOutcomeRecord, i *gh.ProjectItem) {
			r.Stage, i.Status, i.Labels = "Validate", "Validate", []string{"fabrik:revalidate"}
		}, validate, "abc123def456", nil, "fabrik:revalidate"},
		{"forced re-run outstanding", func(r *stageOutcomeRecord, i *gh.ProjectItem) {
			r.Stage, i.Status, i.Labels = "Validate", "Validate", []string{validateRerunLabel}
		}, validate, "abc123def456", nil, "re-run is outstanding"},
		// A record written by the forced re-run itself outlives the marker's presence: the
		// marker is cleared by the very handleStageComplete it retries.
		{"record of the re-run itself", func(r *stageOutcomeRecord, i *gh.ProjectItem) {
			r.Stage, r.Rerun, i.Status, i.Labels = "Validate", true, "Validate", []string{validateRerunLabel}
		}, validate, "abc123def456", nil, ""},
		{"re-run labels only matter for Validate", func(_ *stageOutcomeRecord, i *gh.ProjectItem) {
			i.Labels = []string{"fabrik:revalidate", validateRerunLabel}
		}, research, "abc123def456", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, item := testOutcomeRecord(), live()
			if tc.mutate != nil {
				tc.mutate(rec, &item)
			}
			got := outcomeDiscardReason(rec, item, tc.stage, tc.head, tc.headErr, true)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// R4/FR-009: only a non-transport failure counts toward the bound.
func TestCountsAgainstBound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded (transport)", fmt.Errorf("executing request: %w", &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}), false},
		{"git push network failure", errors.New("fatal: unable to access: connection reset by peer"), false},
		{"permission failure", errors.New("Resource not accessible by integration"), true},
	}
	for _, tc := range cases {
		if got := countsAgainstBound(tc.err); got != tc.want {
			t.Errorf("%s: countsAgainstBound = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSameBody(t *testing.T) {
	if !sameBody("a\r\nb\n", "a\nb") {
		t.Error("line-ending and trailing-space differences must not matter")
	}
	if sameBody("a", "b") {
		t.Error("different bodies compared equal")
	}
}

func TestOutcomeSignalPresent(t *testing.T) {
	yes := true
	plain := &stages.Stage{Name: "Research"}
	ci := &stages.Stage{Name: "Validate", WaitForCI: &yes}
	if !outcomeSignalPresent(gh.ProjectItem{Labels: []string{"stage:Research:complete"}}, plain) {
		t.Error("completion label not recognised")
	}
	if outcomeSignalPresent(gh.ProjectItem{Labels: []string{"fabrik:awaiting-ci"}}, plain) {
		t.Error("awaiting-ci is a completion signal only for a wait_for_ci stage")
	}
	if !outcomeSignalPresent(gh.ProjectItem{Labels: []string{"fabrik:awaiting-ci"}}, ci) {
		t.Error("awaiting-ci not recognised for a wait_for_ci stage")
	}
}

// R2: a live record refuses the stage's dispatch; the gate is on the recorded stage only,
// and a corrupt record (absent after load) blocks nothing.
func TestStageOutcomePending_GatesAdmission(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.EnableStageOutcomesForTest(t.TempDir())
	item := gh.ProjectItem{Number: 7, Repo: "owner/repo", Status: "Research", UpdatedAt: time.Now()}
	stage := stages.FindStage(eng.cfg.Stages, "Research")
	if stage == nil {
		t.Fatal("test engine has no Research stage")
	}
	if !eng.itemMayNeedWork(item) {
		t.Fatal("setup: an item with no record must be admitted")
	}

	rec := testOutcomeRecord()
	rec.Repo = issueKeyRepo(item, eng)
	if err := eng.stageOutcomes.put(rec); err != nil {
		t.Fatal(err)
	}
	if eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork admitted an item with an unpublished outcome")
	}
	if needs, _ := eng.itemNeedsWorkRetryAt(item); needs {
		t.Error("itemNeedsWorkRetryAt admitted an item with an unpublished outcome")
	}
	if !eng.stageRanOnEntry(item, stage, false, false) {
		t.Error("a recorded outcome must count as the stage having run")
	}

	// A record for another stage does not gate this one.
	rec.Stage = "Plan"
	if err := eng.stageOutcomes.put(rec); err != nil {
		t.Fatal(err)
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("a record for a different stage gated this stage")
	}

	// Neutralised, nothing is gated by a freshly written record either.
	eng.stageOutcomes.remove(rec.key())
	if eng.stageOutcomePending(item, stage) {
		t.Error("removed record still gates")
	}
}

func issueKeyRepo(item gh.ProjectItem, eng *Engine) string {
	return itemOwnerRepoString(item, eng.defaultRepo())
}

// A wait_for_ci stage's completion signal is fabrik:awaiting-ci: when that write fails,
// handleStageComplete must say so, or a recorded outcome is dropped as published and the
// stage runs again (#2394).
func TestHandleStageComplete_WaitForCI_AwaitingCIWriteFailureIsReported(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, n int, label string) error {
			if label == "fabrik:awaiting-ci" {
				return errors.New("boom")
			}
			return nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	item := completionItem("stage:Implement:complete")
	err := eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, eng.cfg.Stages[2])
	if err == nil {
		t.Fatal("handleStageComplete returned nil although fabrik:awaiting-ci could not be written")
	}

	client.addLabelToIssueFn = nil
	if err := eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, eng.cfg.Stages[2]); err != nil {
		t.Fatalf("handleStageComplete = %v once the write succeeds, want nil", err)
	}
}

// A Validate landing that fails on an outage is a publication to retry (ADR 2394 decision 6):
// handleStageComplete reports it so the record stays. Any other merge failure keeps its
// historical effect (nil: no label, Validate runs again).
func TestHandleStageComplete_ValidateMergeFailure_TransportIsReported(t *testing.T) {
	// The feedback gate reads the linked PR first and defers on a failure; the landing
	// itself reads it later. failFrom is the call from which every read fails, so the
	// error reaches attemptMergeOnValidate's own read.
	run := func(failFrom int, prErr error) error {
		calls := 0
		client := &mockGitHubClient{
			fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
				calls++
				if calls >= failFrom {
					return nil, prErr
				}
				return &gh.PRDetails{Number: 99, HeadSHA: "abc123"}, nil
			},
		}
		eng := testEngineWithStages(t, client, testStagesWithValidate())
		item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
		return eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Validate"})
	}
	reported := false
	for failFrom := 2; failFrom <= 6; failFrom++ {
		if run(failFrom, context.DeadlineExceeded) != nil {
			reported = true
			if err := run(failFrom, &gh.StatusError{Code: 422}); err != nil {
				t.Errorf("a non-transport landing failure returned %v, want nil (historical effect)", err)
			}
			break
		}
	}
	if !reported {
		t.Error("a transport failure while landing was never reported — the record would be dropped and the stage re-run")
	}
}

// ADR 2394 decision 11: a deliberate fresh start of a stage drops its record, and the rule
// is switched off by the neutralisation seam.
func TestDiscardOutcomeForFreshStart(t *testing.T) {
	e := &Engine{stageOutcomes: newStageOutcomeStore(t.TempDir())}
	item := gh.ProjectItem{Number: 7, Repo: "owner/repo"}
	put := func(stage string) {
		rec := testOutcomeRecord()
		rec.Stage = stage
		if err := e.stageOutcomes.put(rec); err != nil {
			t.Fatal(err)
		}
	}
	live := func() bool { return e.stageOutcomes.get("owner/repo#7") != nil }

	put("Research")
	e.discardOutcomeForFreshStart(item, "Validate", "other stage")
	if !live() {
		t.Fatal("a fresh start of another stage must leave the record alone")
	}
	e.SetOutcomeFreshStartDiscardDisabledForTest(true)
	e.discardOutcomeForFreshStart(item, "Research", "neutralised")
	if !live() {
		t.Fatal("the neutralisation seam must keep the record")
	}
	e.SetOutcomeFreshStartDiscardDisabledForTest(false)
	e.discardOutcomeForFreshStart(item, "Research", "fresh start")
	if live() {
		t.Fatal("a fresh start of the recorded stage must drop the record")
	}
	var nilEngine Engine
	nilEngine.discardOutcomeForFreshStart(item, "Research", "no store") // must not panic
}

// A worker still inside finalizeStageOutcome has not reached the ready checkpoint, so its
// record is still in phase "recorded"; the settle scan must leave it alone rather than judge
// (and discard) it. Without the worker the same record is discarded as interrupted.
func TestSettleStageOutcome_LeavesRecordOfInFlightWorkerAlone(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.EnableStageOutcomesForTest(t.TempDir())
	item := gh.ProjectItem{Number: 7, Repo: "owner/repo", Status: "Research", UpdatedAt: time.Now()}
	repoStr := issueKeyRepo(item, eng)

	rec := testOutcomeRecord()
	rec.Repo = repoStr
	rec.Phase = outcomePhaseRecorded
	if err := eng.stageOutcomes.put(rec); err != nil {
		t.Fatal(err)
	}
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{item}}

	eng.store.Apply(itemstate.WorkerEntered{Repo: repoStr, Number: item.Number, StageName: "Research", StartedAt: time.Now()})
	eng.settleStageOutcomes(context.Background(), board)
	if eng.stageOutcomes.get(rec.key()) == nil {
		t.Fatal("the scan discarded the record of an item whose worker is still running")
	}

	eng.store.Apply(itemstate.WorkerExited{Repo: repoStr, Number: item.Number})
	eng.settleStageOutcomes(context.Background(), board)
	if eng.stageOutcomes.get(rec.key()) != nil {
		t.Fatal("with no worker, a record that never reached ready must be discarded")
	}
}
