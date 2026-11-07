package engine

import (
	"fmt"
	"sort"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// sortedRepoList returns a sorted slice of repos' keys ("owner/repo").
func sortedRepoList(repos map[string]bool) []string {
	out := make([]string, 0, len(repos))
	for r := range repos {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// fetchInstallationRepositoriesFn overrides gh.FetchInstallationRepositories
// in tests, mirroring webhookManager's killFn/startSubprocessFn override
// convention — the real function issues a live HTTP call keyed on a base
// URL (always "" in production; GHES is refused for App auth entirely) and
// an installation token, neither of which a test can point at an httptest
// server without this seam.
var fetchInstallationRepositoriesFn = gh.FetchInstallationRepositories

// appCoverageTransport is what the App-installation coverage check needs from
// an App-webhook transport (the direct and tailscale transports).
type appCoverageTransport interface {
	ManagedRepos() []string
	setInstallationCoverageNote(missing []string, truncated bool)
}

// appCoverageSpec names a transport in the coverage check's logs and
// warnings-panel entries. PanelType is persisted in .fabrik/warnings.json, so
// each transport keeps its own (two transports' Reconcile sweeps must not
// clobber each other's subject sets).
type appCoverageSpec struct {
	Tag       string
	PanelType string
	Ref       string
}

// checkAppInstallationCoverage is the transport-neutral body of the R5
// installation-repo-coverage assertion (#1142, generalised for #2142).
func (e *Engine) checkAppInstallationCoverage(hm appCoverageTransport, spec appCoverageSpec) {
	repos := hm.ManagedRepos()
	if len(repos) == 0 {
		hm.setInstallationCoverageNote(nil, false)
		_ = warnings.Reconcile(e.warnWriter(spec.Tag), spec.PanelType, nil)
		return
	}
	if e.hostClient == nil {
		return
	}
	granted, truncated, err := fetchInstallationRepositoriesFn("", e.hostClient.Token())
	if err != nil {
		e.logf(0, spec.Tag, "WARNING: installation-repo-coverage check failed: %v — leaving prior coverage note in place\n", err)
		return
	}
	grantedSet := make(map[string]bool, len(granted))
	for _, r := range granted {
		grantedSet[r] = true
	}
	var missing []string
	for _, r := range repos {
		if !grantedSet[r] {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		truncationNote := ""
		if truncated {
			// FetchInstallationRepositories already logged the pagination-
			// ceiling detail; this ties that fact to the coverage warning
			// it can invalidate, so an operator reading this line alone
			// isn't misled into granting access the installation may
			// already have (PR review finding).
			truncationNote = " (installation-repo listing was truncated by the pagination ceiling — this may be a false positive; see the FetchInstallationRepositories warning above)"
		}
		list := strings.Join(missing, ", ")
		if err := warnings.Reconcile(e.warnWriter(spec.Tag), spec.PanelType, []warnings.Condition{{
			Subject: "repos",
			Title:   fmt.Sprintf("GitHub App installation does not cover %d managed repo(s)", len(missing)),
			Detail: fmt.Sprintf("These repos are receiving no webhooks (poll-only): %s%s\n\n"+
				"Fix: grant the installation access to them (App settings → Install App → Configure); see %s.", list, truncationNote, spec.Ref),
			LogLine: fmt.Sprintf("GitHub App installation does not cover %d of %d managed repo(s): %s — "+
				"these repos are receiving no webhooks (poll-only); grant the installation access to them (App settings "+
				"→ Install App → Configure); see %s%s", len(missing), len(repos), list, spec.Ref, truncationNote),
		}}); err != nil {
			e.logf(0, spec.Tag, "recording %s warning: %v\n", spec.PanelType, err)
		}
	} else if err := warnings.Reconcile(e.warnWriter(spec.Tag), spec.PanelType, nil); err != nil {
		e.logf(0, spec.Tag, "clearing %s warning: %v\n", spec.PanelType, err)
	}
	hm.setInstallationCoverageNote(missing, truncated)
}
