package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// recordingCoverageTransport records every setInstallationCoverageNote call.
type recordingCoverageTransport struct {
	repos     []string
	calls     int
	missing   []string
	truncated bool
}

func (r *recordingCoverageTransport) ManagedRepos() []string { return r.repos }
func (r *recordingCoverageTransport) setInstallationCoverageNote(missing []string, truncated bool) {
	r.calls++
	r.missing = missing
	r.truncated = truncated
}

func stubInstallationRepos(t *testing.T, fn func(string, string) ([]string, bool, error)) {
	t.Helper()
	orig := fetchInstallationRepositoriesFn
	t.Cleanup(func() { fetchInstallationRepositoriesFn = orig })
	fetchInstallationRepositoriesFn = fn
}

// A truncated installation listing is passed through to the transport's note and
// named in the panel entry, so a false positive is not mistaken for a real gap.
func TestCheckAppInstallationCoverage_TruncationReachesNoteAndPanel(t *testing.T) {
	isolateWarnings(t)
	stubInstallationRepos(t, func(string, string) ([]string, bool, error) {
		return []string{"a/one"}, true, nil
	})
	hm := &recordingCoverageTransport{repos: []string{"b/two", "a/one", "c/three"}}
	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	e.checkAppInstallationCoverage(hm, appCoverageSpec{Tag: "direct", PanelType: "direct_coverage", Ref: "#2142"})

	if want := []string{"b/two", "c/three"}; !reflect.DeepEqual(hm.missing, want) || !hm.truncated {
		t.Errorf("note = (%v, %v), want (%v, true)", hm.missing, hm.truncated, want)
	}
	es, err := warnings.Load()
	if err != nil || len(es) != 1 {
		t.Fatalf("Load = %v, %v", es, err)
	}
	if !strings.Contains(es[0].Detail, "b/two, c/three") || !strings.Contains(es[0].Detail, "truncated by the pagination ceiling") {
		t.Errorf("detail = %q", es[0].Detail)
	}
}

// A failed installation listing leaves the previous note and panel entry in place.
func TestCheckAppInstallationCoverage_FetchErrorKeepsPriorState(t *testing.T) {
	isolateWarnings(t)
	fail := false
	stubInstallationRepos(t, func(string, string) ([]string, bool, error) {
		if fail {
			return nil, false, errors.New("boom")
		}
		return nil, false, nil
	})
	hm := &recordingCoverageTransport{repos: []string{"a/one"}}
	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	spec := appCoverageSpec{Tag: "direct", PanelType: "direct_coverage", Ref: "#2142"}

	e.checkAppInstallationCoverage(hm, spec)
	if hm.calls != 1 || !warningKeys(t)["direct_coverage:repos"] {
		t.Fatalf("setup: calls=%d keys=%v", hm.calls, warningKeys(t))
	}
	fail = true
	e.checkAppInstallationCoverage(hm, spec)
	if hm.calls != 1 {
		t.Errorf("note rewritten on a failed fetch (calls = %d)", hm.calls)
	}
	if !warningKeys(t)["direct_coverage:repos"] {
		t.Errorf("panel entry cleared on a failed fetch")
	}
}

// With no managed repos the note is cleared, a stale panel entry is swept and the
// installation is never queried.
func TestCheckAppInstallationCoverage_NoManagedReposClearsWithoutFetch(t *testing.T) {
	isolateWarnings(t)
	fetched := false
	stubInstallationRepos(t, func(string, string) ([]string, bool, error) {
		fetched = true
		return nil, false, nil
	})
	var buf strings.Builder
	if err := warnings.Reconcile(&buf, "direct_coverage", []warnings.Condition{{
		Subject: "repos", Title: "stale", Detail: "d", LogLine: "stale",
	}}); err != nil {
		t.Fatal(err)
	}
	hm := &recordingCoverageTransport{repos: nil, missing: []string{"x/y"}, truncated: true}
	// A nil hostClient must not matter: the early return comes first.
	e := &Engine{}
	e.checkAppInstallationCoverage(hm, appCoverageSpec{Tag: "direct", PanelType: "direct_coverage", Ref: "#2142"})

	if fetched {
		t.Errorf("installation listing fetched with no managed repos")
	}
	if hm.calls != 1 || hm.missing != nil || hm.truncated {
		t.Errorf("note = (%v, %v) after %d calls, want cleared once", hm.missing, hm.truncated, hm.calls)
	}
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("stale entry survived: %v", k)
	}
}
