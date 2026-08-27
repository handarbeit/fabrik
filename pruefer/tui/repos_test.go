package tui

import (
	"strings"
	"testing"
	"time"
)

func TestRepoPane_SeedsWatchedReposAsNeverPolled(t *testing.T) {
	var r RepoPaneComponent
	r.SetWatchedRepos([]string{"handarbeit/fabrik", "handarbeit/other"})

	view := r.View(120)
	if !strings.Contains(view, "handarbeit/fabrik") || !strings.Contains(view, "handarbeit/other") {
		t.Fatalf("View() missing seeded repos:\n%s", view)
	}
	if !strings.Contains(view, "never polled") {
		t.Errorf("View() does not show 'never polled' before first poll:\n%s", view)
	}
}

func TestRepoPane_PollEventUpdatesStatus(t *testing.T) {
	var r RepoPaneComponent
	r.SetWatchedRepos([]string{"handarbeit/fabrik"})
	now := time.Now()

	comp, _ := r.Update(RepoPollEvent{Repo: "handarbeit/fabrik", At: now, PRCount: 4})
	r = comp.(RepoPaneComponent)
	comp, _ = r.Update(TickEvent{At: now})
	r = comp.(RepoPaneComponent)

	view := r.View(120)
	if !strings.Contains(view, "4 PR(s)") {
		t.Errorf("View() does not show PR count:\n%s", view)
	}
	if strings.Contains(view, "never polled") {
		t.Errorf("View() still shows 'never polled' after a poll:\n%s", view)
	}
}

func TestRepoPane_PollErrorRenders(t *testing.T) {
	var r RepoPaneComponent
	r.SetWatchedRepos([]string{"handarbeit/fabrik"})

	comp, _ := r.Update(RepoPollEvent{Repo: "handarbeit/fabrik", At: time.Now(), Err: "listing PRs: 500"})
	r = comp.(RepoPaneComponent)

	if !strings.Contains(r.View(120), "error: listing PRs: 500") {
		t.Errorf("View() does not show poll error:\n%s", r.View(120))
	}
}

func TestRepoPane_UnseenRepoFromPollEventIsAdded(t *testing.T) {
	// A RepoPollEvent for a repo not seeded via SetWatchedRepos (e.g. daemon
	// wired without seeding) must still be tracked, not dropped.
	var r RepoPaneComponent
	comp, _ := r.Update(RepoPollEvent{Repo: "handarbeit/fabrik", At: time.Now(), PRCount: 1})
	r = comp.(RepoPaneComponent)
	if len(r.order) != 1 {
		t.Fatalf("order = %v, want 1 entry", r.order)
	}
}

// TestRepoPane_DerivedRepoSetEvent_AddsAndRemovesRepos is #1641/R4's core
// live-update regression test: a re-derivation that gains a repo must add
// it, one that loses a repo must remove it, and neither operation may
// disturb an already-present repo's poll status (the Risk this pane's
// plumbing is explicitly called out for: "a poorly-designed event could
// race the initial seed or double-count repos").
func TestRepoPane_DerivedRepoSetEvent_AddsAndRemovesRepos(t *testing.T) {
	var r RepoPaneComponent
	r.SetWatchedRepos([]string{"handarbeit/fabrik"})
	now := time.Now()
	comp, _ := r.Update(RepoPollEvent{Repo: "handarbeit/fabrik", At: now, PRCount: 3})
	r = comp.(RepoPaneComponent)

	// First derivation: gains handarbeit/other alongside the already-seeded
	// (and already-polled) handarbeit/fabrik.
	comp, _ = r.Update(DerivedRepoSetEvent{
		Repos: []DerivedRepoEntry{
			{Repo: "handarbeit/fabrik", InstallationID: 111},
			{Repo: "handarbeit/other", InstallationID: 111},
		},
	})
	r = comp.(RepoPaneComponent)
	if len(r.order) != 2 {
		t.Fatalf("order after gaining a repo = %v, want 2 entries", r.order)
	}
	if got := r.repos["handarbeit/fabrik"].PRCount; got != 3 {
		t.Errorf("handarbeit/fabrik's PRCount = %d, want 3 preserved across the derivation event, not reset", got)
	}

	// Second derivation: loses handarbeit/other (e.g. the installation's
	// repository_selection narrowed, or it was dropped from watched_repos).
	comp, _ = r.Update(DerivedRepoSetEvent{
		Repos: []DerivedRepoEntry{{Repo: "handarbeit/fabrik", InstallationID: 111}},
	})
	r = comp.(RepoPaneComponent)
	if len(r.order) != 1 || r.order[0] != "handarbeit/fabrik" {
		t.Fatalf("order after losing a repo = %v, want [handarbeit/fabrik] only", r.order)
	}
	if _, ok := r.repos["handarbeit/other"]; ok {
		t.Error("handarbeit/other should have been dropped from repos, not just order")
	}
	if got := r.repos["handarbeit/fabrik"].PRCount; got != 3 {
		t.Errorf("handarbeit/fabrik's PRCount = %d, want 3 still preserved", got)
	}
}

// TestRepoPane_DerivedRepoSetEvent_ShowsInstallationProvenance covers R4's
// per-repo "from which installation" half.
func TestRepoPane_DerivedRepoSetEvent_ShowsInstallationProvenance(t *testing.T) {
	var r RepoPaneComponent
	comp, _ := r.Update(DerivedRepoSetEvent{
		Repos:         []DerivedRepoEntry{{Repo: "handarbeit/fabrik", InstallationID: 111}},
		Installations: []DerivedInstallationSummary{{Account: "handarbeit", InstallationID: 111, RepositorySelection: "all", RepoCount: 1}},
	})
	r = comp.(RepoPaneComponent)

	view := r.View(120)
	if !strings.Contains(view, "installation 111") {
		t.Errorf("View() does not show the granting installation ID:\n%s", view)
	}
	if !strings.Contains(view, "1 installation(s)") {
		t.Errorf("View() does not show the installation count in the title:\n%s", view)
	}
}

// TestRepoPane_DerivedRepoSetEvent_SurfacesProvenanceWarnings covers R4/R5's
// TUI-visible warnings: a truncated enumeration, a max_derived_repos cap,
// and a watched_repos entry not covered by any installation must all be
// surfaced, not silently absorbed.
func TestRepoPane_DerivedRepoSetEvent_SurfacesProvenanceWarnings(t *testing.T) {
	var r RepoPaneComponent
	comp, _ := r.Update(DerivedRepoSetEvent{
		Repos:       []DerivedRepoEntry{{Repo: "handarbeit/fabrik", InstallationID: 111}},
		FilteredOut: []string{"handarbeit/uninstalled-repo"},
		Truncated:   true,
		Capped:      true,
		CapApplied:  200,
	})
	r = comp.(RepoPaneComponent)

	view := r.View(120)
	if !strings.Contains(view, "pagination ceiling") {
		t.Errorf("View() does not surface the truncation warning:\n%s", view)
	}
	if !strings.Contains(view, "capped at 200") {
		t.Errorf("View() does not surface the cap warning:\n%s", view)
	}
	if !strings.Contains(view, "handarbeit/uninstalled-repo") {
		t.Errorf("View() does not surface the filtered-out watched_repos entry:\n%s", view)
	}
}
