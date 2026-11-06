package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestTrialWakeTarget(t *testing.T) {
	pr := `{"action":"synchronize","repository":{"full_name":"o/r"},"pull_request":{"number":7,"head":{"sha":"abc"}}}`
	if got, ok := trialWakeTarget("pull_request", []byte(pr)); !ok || got != (trialEventTarget{"o/r", "abc", 7}) {
		t.Fatalf("pull_request: %+v ok=%v", got, ok)
	}
	cr := `{"action":"completed","repository":{"full_name":"o/r"},"check_run":{"head_sha":"abc","pull_requests":[]}}`
	if got, ok := trialWakeTarget("check_run", []byte(cr)); !ok || got.prNum != 0 || got.headSHA != "abc" {
		t.Fatalf("check_run with no PRs: %+v ok=%v", got, ok)
	}
	cr = `{"action":"completed","repository":{"full_name":"o/r"},"check_run":{"head_sha":"abc","pull_requests":[{"number":9}]}}`
	if got, ok := trialWakeTarget("check_run", []byte(cr)); !ok || got.prNum != 9 {
		t.Fatalf("check_run: %+v ok=%v", got, ok)
	}
	cs := `{"action":"completed","repository":{"full_name":"o/r"},"check_suite":{"head_sha":"abc"}}`
	if _, ok := trialWakeTarget("check_suite", []byte(cs)); !ok {
		t.Fatal("completed check_suite should be a target")
	}
	for _, a := range []string{"created", "rerequested", "requested_action"} {
		p := fmt.Sprintf(`{"action":%q,"repository":{"full_name":"o/r"},"check_run":{"head_sha":"abc"}}`, a)
		if _, ok := trialWakeTarget("check_run", []byte(p)); ok {
			t.Errorf("action %q must not wake", a)
		}
	}
	if _, ok := trialWakeTarget("issues", []byte(pr)); ok {
		t.Error("other event types must not be targets")
	}
	if _, ok := trialWakeTarget("pull_request", []byte("not json")); ok {
		t.Error("unreadable payload must not be a target")
	}
	if _, ok := trialWakeTarget("pull_request", []byte(`{"pull_request":{"number":1}}`)); ok {
		t.Error("a payload without repo/head must not be a target")
	}
}

func wakeCheckRun(repo, sha string, pr int, action string) []byte {
	prs := []map[string]int{}
	if pr != 0 {
		prs = append(prs, map[string]int{"number": pr})
	}
	b, _ := json.Marshal(map[string]any{
		"action":     action,
		"repository": map[string]string{"full_name": repo},
		"check_run":  map[string]any{"head_sha": sha, "pull_requests": prs},
	})
	return b
}

// An open trial caps the idle interval at one configured interval, below even a
// shorter-than-idle cooldown cap; no trial, a finished run and the seam leave it alone.
func TestOpenTrialCapAndWake(t *testing.T) {
	a := newAsyncTrain(t, 3, poisonedBy())
	eng := a.eng
	eng.SetClock(stubClock{t: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
	eng.cfg.PollSeconds = 30
	configured := 30 * time.Second

	clk := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	idle := func() time.Duration {
		clk = clk.Add(time.Minute) // past the minimum poll interval floor
		eng.SetClock(stubClock{t: clk})
		eng.SetIdleStartForTest(time.Now().Add(-time.Hour))
		res, err := eng.PollWithBackoff(context.Background(), configured)
		if err != nil {
			t.Fatalf("PollWithBackoff: %v", err)
		}
		return res.NextInterval
	}

	if eng.openTrialPresent() {
		t.Fatal("no run yet")
	}
	if got := idle(); got <= configured {
		t.Fatalf("without a trial the idle interval backs off, got %v", got)
	}

	a.dispatch()
	runs := eng.trainRuns.liveRuns()
	if len(runs) != 1 || runs[0].trial() == nil {
		t.Fatalf("expected one run with an open trial, got %d", len(runs))
	}
	// The scripted validator opens no real PR, so give the record the identity a
	// real trial carries.
	runs[0].mu.Lock()
	runs[0].rec.Trial.HeadSHA = "trialsha"
	runs[0].rec.Trial.PRNum = 900
	runs[0].mu.Unlock()
	tr := runs[0].trial()
	if !eng.openTrialPresent() {
		t.Fatal("openTrialPresent should be true")
	}
	if got := idle(); got != configured {
		t.Errorf("open trial: interval = %v, want %v", got, configured)
	}
	eng.SetOpenTrialCapDisabledForTest(true)
	if got := idle(); got <= configured {
		t.Errorf("seam off must restore the idle backoff, got %v", got)
	}
	eng.SetOpenTrialCapDisabledForTest(false)

	// Wake matrix.
	wake := make(chan struct{}, 1)
	eng.SetWakeCh(wake)
	fire := func(p []byte) bool {
		eng.wakeForOpenTrial("check_run", p)
		select {
		case <-wake:
			return true
		default:
			return false
		}
	}
	repo := runs[0].repoKey()
	if !fire(wakeCheckRun(repo, tr.HeadSHA, tr.PRNum, "completed")) {
		t.Error("completed check_run for the open trial must wake")
	}
	if !fire(wakeCheckRun(repo, tr.HeadSHA, 0, "completed")) {
		t.Error("a check_run with no PR number matches on head SHA")
	}
	if fire(wakeCheckRun(repo, tr.HeadSHA, tr.PRNum+1000, "completed")) {
		t.Error("a different PR number must not wake")
	}
	if fire(wakeCheckRun(repo, "deadbeef", tr.PRNum, "completed")) {
		t.Error("a replaced trial's head must not wake")
	}
	if fire(wakeCheckRun("other/repo", tr.HeadSHA, tr.PRNum, "completed")) {
		t.Error("another repo must not wake")
	}
	if fire(wakeCheckRun(repo, tr.HeadSHA, tr.PRNum, "created")) {
		t.Error("a non-completed check_run must not wake")
	}
	// A full one-slot channel never blocks.
	wake <- struct{}{}
	eng.wakeForOpenTrial("check_run", wakeCheckRun(repo, tr.HeadSHA, tr.PRNum, "completed"))
	<-wake
	eng.SetTrialWakeDisabledForTest(true)
	if fire(wakeCheckRun(repo, tr.HeadSHA, tr.PRNum, "completed")) {
		t.Error("seam off must send nothing")
	}
	eng.SetTrialWakeDisabledForTest(false)

	// A finished run does not count.
	a.runToEnd(10)
	if eng.openTrialPresent() {
		t.Error("a finished run has no open trial")
	}
	if fire(wakeCheckRun(repo, tr.HeadSHA, tr.PRNum, "completed")) {
		t.Error("no open trial: no wake")
	}
}
