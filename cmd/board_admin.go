package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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

// displayFieldSpec is one display-only ProjectV2 field a Fabrik board needs
// (#2110): the status line (#2048), "Last activity" and "Last run" (#2049).
type displayFieldSpec struct {
	Key      string // the project_fields.* key, for messages
	Name     string // resolved field name; never empty (disabled fields are dropped)
	DataType string // gh.FieldTypeText or gh.FieldTypeDate
}

// requiredDisplayFields is the single definition of which display fields a
// board needs, shared by `init --create-board` and `repair-board` so the two
// cannot diverge (FR-012). Names and `off` handling come from the same
// config resolvers the engine's startup uses (cmd/root.go), so a custom name
// creates that name and a disabled field is simply absent from the result.
func requiredDisplayFields(pf config.ProjectFieldsConfig) []displayFieldSpec {
	var specs []displayFieldSpec
	add := func(key, name, dataType string) {
		if name != "" {
			specs = append(specs, displayFieldSpec{Key: key, Name: name, DataType: dataType})
		}
	}
	add("status_line", pf.ResolveStatusLineField(), gh.FieldTypeText)
	add("last_activity", pf.ResolveLastActivityField(), gh.FieldTypeDate)
	add("last_run", pf.ResolveLastRunField(), gh.FieldTypeText)
	return specs
}

// displayFieldState is the per-field outcome shown to the operator.
type displayFieldState string

const (
	displayFieldPresent   displayFieldState = "present"
	displayFieldMissing   displayFieldState = "missing"
	displayFieldMismatch  displayFieldState = "type-mismatch"
	displayFieldCollision displayFieldState = "name-collision"
	displayFieldCreated   displayFieldState = "created"
	displayFieldFailed    displayFieldState = "failed"
)

// displayFieldStatus is a spec plus its classification. Found is the data
// type of the existing same-named field (for a mismatch); Err is set only
// when State is displayFieldFailed.
type displayFieldStatus struct {
	displayFieldSpec
	State displayFieldState
	Found string
	Err   error
}

// displayFieldClient is the slice of *gh.Client the display-field helpers
// use; a narrow interface so tests can record every create call.
type displayFieldClient interface {
	FetchProjectFields(projectID string) ([]gh.ProjectField, error)
	CreateProjectV2Field(projectID, name, dataType string) (string, error)
}

// planDisplayFields classifies each spec against the board's actual fields.
// Names are matched case-insensitively (GitHub's field-name uniqueness is
// not case-sensitive). A same-named field of the right type is present; of
// another type (including a built-in such as Labels) a type mismatch, left
// untouched (R3); a spec whose name an earlier spec already claimed is a
// name collision and is never created. Only `missing` is ever acted on.
func planDisplayFields(specs []displayFieldSpec, fields []gh.ProjectField) []displayFieldStatus {
	byName := make(map[string]gh.ProjectField, len(fields))
	for _, f := range fields {
		key := strings.ToLower(strings.TrimSpace(f.Name))
		if _, dup := byName[key]; !dup {
			byName[key] = f
		}
	}
	claimed := make(map[string]bool, len(specs))
	out := make([]displayFieldStatus, 0, len(specs))
	for _, sp := range specs {
		key := strings.ToLower(strings.TrimSpace(sp.Name))
		st := displayFieldStatus{displayFieldSpec: sp}
		switch f, exists := byName[key]; {
		case claimed[key]:
			st.State = displayFieldCollision
		case !exists:
			st.State = displayFieldMissing
		case f.DataType == sp.DataType:
			st.State = displayFieldPresent
		default:
			st.State = displayFieldMismatch
			st.Found = f.DataType
		}
		claimed[key] = true
		out = append(out, st)
	}
	return out
}

// applyDisplayFields creates every `missing` field, attempting all of them
// even when one fails (FR-013). It updates the statuses in place and is never
// called on a dry run. Nothing but a create call is issued: no field is
// deleted, renamed, retyped or recreated (FR-007).
func applyDisplayFields(client displayFieldClient, projectID string, plan []displayFieldStatus) {
	for i := range plan {
		if plan[i].State != displayFieldMissing {
			continue
		}
		id, err := client.CreateProjectV2Field(projectID, plan[i].Name, plan[i].DataType)
		if err == nil && id == "" {
			err = errors.New("no field ID returned")
		}
		if err != nil {
			plan[i].State = displayFieldFailed
			plan[i].Err = fmt.Errorf("display field %q (%s): %w", plan[i].Name, plan[i].DataType, err)
			continue
		}
		plan[i].State = displayFieldCreated
	}
}

// displayFieldsErr joins the creation failures, each naming its field, or
// returns nil. A type mismatch or collision is deliberately not an error: it
// needs an operator decision and is shown in the report.
func displayFieldsErr(plan []displayFieldStatus) error {
	var errs []error
	for _, st := range plan {
		if st.State == displayFieldFailed {
			errs = append(errs, st.Err)
		}
	}
	return errors.Join(errs...)
}

// displayFieldLine renders one report line.
func displayFieldLine(st displayFieldStatus) string {
	head := fmt.Sprintf("display field %q (%s)", st.Name, st.DataType)
	switch st.State {
	case displayFieldMismatch:
		return fmt.Sprintf("%s: type mismatch — expected %s, found %s; left untouched", head, st.DataType, st.Found)
	case displayFieldCollision:
		return fmt.Sprintf("%s: name collision — another display field (project_fields.%s) is configured with the same name; not created", head, st.Key)
	case displayFieldFailed:
		return fmt.Sprintf("%s: FAILED — %v", head, st.Err)
	default:
		return fmt.Sprintf("%s: %s", head, st.State)
	}
}

// planBoardDisplayFields lists the board's fields and classifies the
// required display fields against them. A listing failure is returned and
// the plan is nil: nothing is created blind.
func planBoardDisplayFields(client displayFieldClient, projectID string, pf config.ProjectFieldsConfig) ([]displayFieldStatus, error) {
	specs := requiredDisplayFields(pf)
	if len(specs) == 0 {
		return nil, nil
	}
	fields, err := client.FetchProjectFields(projectID)
	if err != nil {
		return nil, fmt.Errorf("reading display fields: %w", err)
	}
	return planDisplayFields(specs, fields), nil
}

// countDisplayFields returns how many plan entries are in state.
func countDisplayFields(plan []displayFieldStatus, state displayFieldState) int {
	n := 0
	for _, st := range plan {
		if st.State == state {
			n++
		}
	}
	return n
}

// printDisplayFields writes one line per plan entry, optionally skipping
// those already present.
func printDisplayFields(w io.Writer, prefix string, plan []displayFieldStatus, skipPresent bool) {
	for _, st := range plan {
		if skipPresent && st.State == displayFieldPresent {
			continue
		}
		fmt.Fprintf(w, "%s: %s\n", prefix, displayFieldLine(st))
	}
}
