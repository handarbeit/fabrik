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
