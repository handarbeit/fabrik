package pruefer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/githubauth"
)

func captureLogs(t *testing.T) func() []string {
	t.Helper()
	var lines []string
	old := Logf
	Logf = func(pr int, tag, format string, args ...any) {
		lines = append(lines, tag+": "+fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { Logf = old })
	return func() []string { return lines }
}

// #1951: a stale installation is reported as stale, at warn level, naming the
// retained count and the reset — never as "N repo(s) accessible".
func TestLogRederivedRepos_StaleInstallation(t *testing.T) {
	lines := captureLogs(t)
	reset := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	logRederivedRepos(githubauth.DerivedRepoSet{Installations: []githubauth.DerivedInstallation{{
		Account: "verveguy", InstallationID: 149677864, RepositorySelection: "all",
		RepoCount: 61, RepoListError: "rate limited", Stale: true, RetryNotBefore: reset,
	}}})
	got := strings.Join(lines(), "\n")
	for _, want := range []string{"warn: STALE", "61 repo(s)", "2026-09-29T18:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "repo(s) accessible") {
		t.Errorf("a stale installation must not read as a successful listing:\n%s", got)
	}
}

// #1951 AC6: a cold-start failure is an error-tagged failure, never zero accessible.
func TestLogRederivedRepos_ColdStartFailureIsErrorNotZero(t *testing.T) {
	lines := captureLogs(t)
	logRederivedRepos(githubauth.DerivedRepoSet{Installations: []githubauth.DerivedInstallation{{
		Account: "verveguy", InstallationID: 149677864, RepositorySelection: "all",
		RepoListError: "rate limited",
	}}})
	got := strings.Join(lines(), "\n")
	if !strings.Contains(got, "error: installation 149677864") || !strings.Contains(got, "unknown, not confirmed empty") {
		t.Errorf("cold start must log an error-tagged, unknown-not-empty line:\n%s", got)
	}
	if strings.Contains(got, "0 repo(s) accessible") {
		t.Errorf("a failed listing must never read as zero accessible:\n%s", got)
	}
}

func TestDerivedInstallationSummaries_CarriesStaleState(t *testing.T) {
	reset := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	got := derivedInstallationSummaries(githubauth.DerivedRepoSet{Installations: []githubauth.DerivedInstallation{
		{InstallationID: 1, RepoCount: 5, RepoListError: "x", Stale: true, RetryNotBefore: reset},
		{InstallationID: 2, RepoCount: 0, RepoListError: "x"},
		{InstallationID: 3, RepoCount: 7},
	}})
	if !got[0].ListingFailed || !got[0].Stale || !got[0].RetryNotBefore.Equal(reset) {
		t.Errorf("stale summary = %+v", got[0])
	}
	if !got[1].ListingFailed || got[1].Stale {
		t.Errorf("cold-start summary = %+v", got[1])
	}
	if got[2].ListingFailed || got[2].Stale {
		t.Errorf("healthy summary = %+v", got[2])
	}
}
