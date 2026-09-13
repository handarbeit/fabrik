package cmd

import (
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

// requiredStageColumnNames returns the ordered list of stage names that must
// have a matching board Status column, per the same rule
// engine/startup.go's checkStageColumnAlignment uses for its "required" set:
// every non-cleanup, non-unmanaged stage. Holding stages (e.g. Queued) are
// included unconditionally here — unlike checkStageColumnAlignment's
// merge_train-gated startup deferral (ADR-1421), both create and repair are
// explicit operator actions with no reason to withhold a column the operator
// is about to create/repair anyway, and `fabrik init` extracts every
// embedded default stage (including queued.yaml) unconditionally regardless
// of merge_train. Returned in stage Order.
func requiredStageColumnNames(allStages []*stages.Stage) []string {
	type nameOrder struct {
		name  string
		order int
	}
	var filtered []nameOrder
	for _, s := range allStages {
		if s.CleanupWorktree || s.Unmanaged {
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
