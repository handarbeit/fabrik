package cmd

import (
	"context"
	"fmt"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// This file holds helpers shared by the `fabrik init --create-board` (R1)
// and `fabrik repair-board` (R3) entry points, so the two can never disagree
// about token/client construction, the R7 org-only refusal, or what
// "missing column" means. See adrs/1714-board-admin-create-and-repair.md.

// loadGitHubToken resolves a GitHub token the same way the daemon path does
// (flag > FABRIK_TOKEN > GITHUB_TOKEN), for the two board-admin entry points
// that have no daemon Config to draw from. flagToken is the already-parsed
// --token value, or "" if none was given.
func loadGitHubToken(flagToken string) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if t := config.Token(); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("GitHub token required: use --token, FABRIK_TOKEN, or GITHUB_TOKEN")
}

// newBoardGHClient constructs a *gh.Client for the board-admin entry points,
// targeting a GitHub Enterprise Server host when ghesHost is non-empty
// (mirroring cmd/watch.go's existing client-construction precedent).
func newBoardGHClient(token, ghesHost string) *gh.Client {
	if ghesHost != "" {
		return gh.NewClientForHost(token, ghesHost)
	}
	return gh.NewClient(token)
}

// refuseIfUserOwnedBoard is the shared R7 gate: board creation and repair are
// organization-only. ownerType is "organization" or "user" — for repair,
// resolved from the existing board's FetchProjectBoard; for creation,
// resolved fresh via ResolveOwner (there is no existing board to read it
// from). Both call sites share this one check so the refusal wording and
// behavior can never diverge between them.
func refuseIfUserOwnedBoard(ownerType string) error {
	if ownerType != "user" {
		return nil
	}
	return fmt.Errorf("refusing: this operation is organization-only (see #770) — the target board/owner is user-owned. " +
		"Under GitHub App authentication this operation is unavailable for a user-owned board; under a personal access " +
		"token it may work, but Fabrik does not attempt it automatically. Create/repair the board manually instead, " +
		"or move it to an organization")
}

// boardColumnNames returns the ordered list of Status columns a Fabrik board
// needs: one per configured stage, in stage Order — including the cleanup
// stage (Done), excluding unmanaged parking stages (Backlog), which are
// optional. Board creation and repair build the board itself, so the
// terminal column must exist: without Done, no item can ever complete
// (every terminal advance fails and the item escalates via
// fabrik:awaiting-advance). This is deliberately wider than the "required" set
// engine/startup.go's checkStageColumnAlignment enforces at startup, which
// only has to refuse a board missing a column a managed stage dispatches
// into. Holding stages (e.g. Queued) are included unconditionally: create
// and repair are explicit operator actions, and `fabrik init` extracts
// every embedded default stage (including queued.yaml) regardless of
// merge_train. Returned in stage Order (ties keep source order).
func boardColumnNames(allStages []*stages.Stage) []string {
	type nameOrder struct {
		name  string
		order int
	}
	var filtered []nameOrder
	for _, s := range allStages {
		if s.Unmanaged {
			continue
		}
		filtered = append(filtered, nameOrder{name: s.Name, order: s.Order})
	}
	// Stable insertion-order sort by Order (ties keep source order).
	for i := 1; i < len(filtered); i++ {
		for j := i; j > 0 && filtered[j].order < filtered[j-1].order; j-- {
			filtered[j], filtered[j-1] = filtered[j-1], filtered[j]
		}
	}
	names := make([]string, len(filtered))
	for i, f := range filtered {
		names[i] = f.name
	}
	return names
}

// missingStageColumns returns the subset of required (in order) that has no
// matching entry in sf.Options — i.e., the columns creation must add or
// repair must append.
func missingStageColumns(required []string, sf *gh.StatusField) []string {
	var missing []string
	for _, name := range required {
		if sf == nil {
			missing = append(missing, name)
			continue
		}
		if _, ok := sf.Options[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// appBoardClient builds an installation-token client for the board-admin
// entry points that have no daemon Config to draw from, when the resolved
// config (FABRIK_GITHUB_APP_* env over .fabrik/config.yaml) fully configures
// GitHub App auth. It returns (nil, nil) when App auth is not configured, so
// callers fall back to loadGitHubToken; a partial configuration is an error,
// as it is for the daemon (R5). It reuses the `init --github-app` adopt path,
// which verifies the installation's grants with the engine's own rule, so an
// App-only deployment (no user, no PAT — #1893) can still run repair-board.
func appBoardClient(ctx context.Context, owner string, pc config.ProjectConfig, ghesHost string) (*gh.Client, error) {
	var app Config
	if err := resolveGitHubAppConfig(&app, pc); err != nil {
		return nil, err
	}
	set := 0
	for _, ok := range []bool{app.GitHubAppID != 0, app.GitHubAppPrivateKeyPath != "", app.GitHubAppInstallationID != 0} {
		if ok {
			set++
		}
	}
	switch set {
	case 0:
		return nil, nil
	case 3:
	default:
		return nil, fmt.Errorf("GitHub App authentication is partially configured: github_app_id, github_app_private_key_path and github_app_installation_id must all be set, or none")
	}
	if ghesHost != "" {
		return nil, fmt.Errorf("GitHub App authentication is not supported with a GitHub Enterprise Server host; use --token")
	}
	res, err := runGitHubAppSetup(ctx, githubAppSetupOptions{
		Owner:          owner,
		AppID:          app.GitHubAppID,
		PrivateKeyPath: app.GitHubAppPrivateKeyPath,
		InstallationID: app.GitHubAppInstallationID,
		NoBrowser:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("GitHub App authentication: %w", err)
	}
	return res.Client, nil
}
