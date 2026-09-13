package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// This file implements R3's opt-in board repair: `fabrik repair-board`
// diagnoses an existing board's Status column set against the configured
// stages and, only with --apply, adds whatever is missing — never removing
// a column, never reordering existing options, and (R4, the single highest-
// risk part of this issue) never regenerating an existing option's id. See
// adrs/1714-board-admin-create-and-repair.md.

// runRepairBoard implements the `fabrik repair-board` subcommand.
func runRepairBoard(args []string) error {
	fset := flag.NewFlagSet("repair-board", flag.ContinueOnError)
	apply := fset.Bool("apply", false, "Add missing stage columns to the board (never removes columns, never reorders or regenerates existing options)")
	tokenFlag := fset.String("token", "", "GitHub token (or FABRIK_TOKEN / GITHUB_TOKEN)")
	ghesHostFlag := fset.String("ghes-host", "", "GitHub Enterprise Server hostname (also FABRIK_GHES_HOST)")
	stagesDirFlag := fset.String("stages", "", "Stage configs directory (default: .fabrik/config.yaml's stages:, or ./.fabrik/stages)")

	fset.Usage = func() {
		fmt.Fprintf(fset.Output(), "Usage: fabrik repair-board [--apply]\n\n")
		fmt.Fprintf(fset.Output(), "Diagnose (or, with --apply, add) missing Status columns on the project\n")
		fmt.Fprintf(fset.Output(), "board configured in .fabrik/config.yaml, derived from .fabrik/stages/.\n\n")
		fmt.Fprintf(fset.Output(), "By default (no flags): print which columns are missing; make no changes.\n")
		fmt.Fprintf(fset.Output(), "  --apply    Add the missing columns. Never removes a column or reorders\n")
		fmt.Fprintf(fset.Output(), "             existing options; existing option ids are always preserved\n")
		fmt.Fprintf(fset.Output(), "             so no item loses its current Status.\n\n")
		fmt.Fprintf(fset.Output(), "Organization-owned boards only (see #770) — refuses on a user-owned board.\n")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		return err
	}

	_ = config.LoadDotenv()
	pc, err := config.LoadProjectConfig()
	if err != nil {
		return fmt.Errorf("repair-board: %w", err)
	}
	if pc.Owner == "" || pc.ProjectNum == nil || *pc.ProjectNum <= 0 {
		return fmt.Errorf("repair-board: .fabrik/config.yaml must have owner and project set (run `fabrik init` first)")
	}

	stagesDir := *stagesDirFlag
	if stagesDir == "" {
		stagesDir = pc.StagesDir
	}
	if stagesDir == "" {
		stagesDir = "./.fabrik/stages"
	}
	allStages, err := stages.LoadAll(stagesDir)
	if err != nil {
		return fmt.Errorf("repair-board: loading stage configs from %s: %w", stagesDir, err)
	}

	token, err := loadGitHubToken(*tokenFlag)
	if err != nil {
		return err
	}
	ghesHost := resolveGHESHost(*ghesHostFlag, pc)
	client := newBoardGHClient(token, ghesHost)

	return repairBoardCore(client, pc.Owner, pc.Repo, *pc.ProjectNum, pc.OwnerType, allStages, *apply, os.Stdout)
}

// repairBoardCore is the testable core of runRepairBoard, taking an
// already-constructed *gh.Client so tests can supply an httptest-backed one.
// ownerTypeHint is pc.OwnerType (may be "" — FetchProjectBoard falls back to
// probing organization then user); the AUTHORITATIVE ownerType for the R7
// refusal check is always board.OwnerType, resolved live by FetchProjectBoard,
// never the hint (which is advisory/legacy — see project_admin.go and the
// Research findings this issue's Plan stage recorded).
func repairBoardCore(client *gh.Client, owner, repo string, projectNum int, ownerTypeHint string, allStages []*stages.Stage, apply bool, w io.Writer) error {
	board, err := client.FetchProjectBoard(owner, repo, projectNum, ownerTypeHint)
	if err != nil {
		return fmt.Errorf("fetching project board: %w", err)
	}
	if board.ProjectID == "" {
		return fmt.Errorf("project board has no ID")
	}
	if err := refuseIfUserOwnedBoard(board.OwnerType); err != nil {
		return err
	}

	sf, err := client.FetchStatusField(board.ProjectID)
	if err != nil {
		return fmt.Errorf("fetching Status field: %w", err)
	}

	required := requiredStageColumnNames(allStages)
	missing := missingStageColumns(required, sf)

	if len(missing) == 0 {
		fmt.Fprintf(w, "repair-board: board already has all %d required stage column(s); nothing to do.\n", len(required))
		return nil
	}

	fmt.Fprintf(w, "repair-board: %d missing column(s): %s\n", len(missing), strings.Join(missing, ", "))
	if !apply {
		fmt.Fprintf(w, "Dry run — no changes made. Re-run with --apply to add the missing column(s).\n")
		return nil
	}

	// R4/R6: build the option list by echoing every existing option's
	// id/color/description back UNCHANGED (never omitted, never
	// regenerated — that would silently clear every item's Status) and
	// appending only the missing columns, each with a nil id so the server
	// mints a fresh one. There is no code path here that omits an existing
	// entry, so a column is never deleted (R6) — by construction, not by a
	// separate "check for items first" step.
	options := make([]gh.StatusOptionInput, 0, len(sf.OrderedOptionNames)+len(missing))
	for _, name := range sf.OrderedOptionNames {
		detail, ok := sf.OptionDetails[name]
		if !ok {
			return fmt.Errorf("internal error: existing option %q has no details from FetchStatusField — refusing to risk an id-losing repair", name)
		}
		id := detail.ID
		options = append(options, gh.StatusOptionInput{
			ID:          &id,
			Name:        detail.Name,
			Color:       detail.Color,
			Description: detail.Description,
		})
	}
	for _, name := range missing {
		options = append(options, gh.StatusOptionInput{Name: name, Color: "GRAY", Description: ""})
	}

	if err := client.SetStatusFieldOptions(sf.FieldID, options); err != nil {
		return fmt.Errorf("adding missing column(s): %w", err)
	}
	fmt.Fprintf(w, "repair-board: added %d column(s): %s\n", len(missing), strings.Join(missing, ", "))
	return nil
}
