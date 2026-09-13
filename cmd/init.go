package cmd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
	"github.com/mattn/go-isatty"
)

// configYAMLTemplate is the all-commented-out template written by fabrik init.
// Required fields use placeholder comments; optional fields show their defaults.
const configYAMLTemplate = `# .fabrik/config.yaml — project-level configuration for Fabrik
# Commit this file to git so project settings travel with the repo.
# Keep secrets (FABRIK_TOKEN, GITHUB_TOKEN) in .env (gitignored).
#
# Precedence: CLI flag > shell env var > .env > .fabrik/config.yaml > built-in default

# Required: GitHub repository owner (org or username)
# owner: your-org

# Required: GitHub repository name
# repo: your-repo

# Required: GitHub project number (the number in the project URL)
# project: 1

# Required: Your GitHub username (Fabrik only processes issues assigned to you)
# user: your-github-username

# Optional settings (defaults shown):
# owner_type: organization         # Owner type parsed from project URL: "user" or "organization".
# ghes_host: ""                 # GitHub Enterprise Server hostname (no scheme, no trailing slash).
#                               # Absent means github.com. Also: --ghes-host flag or FABRIK_GHES_HOST env var.
# stages: ./.fabrik/stages      # Path to stage YAML configs directory.
# poll: 30                      # Polling interval in seconds. Lower = more responsive, higher = fewer API calls.
# max_concurrent: 5             # Max parallel Claude sessions. Tune based on your API tier capacity.
# max_retries: 3                # Max stage failures before pausing an issue (0 = unlimited retries).
# yolo: false                   # Auto-advance issues through stages without human card moves.
# auto_upgrade: false           # Self-upgrade from origin/main when idle (self-evolving workflow).
# git_ssh: false                # Use SSH clone URLs (git@github.com) instead of HTTPS. Also: --ssh flag or FABRIK_GIT_SSH env var.
# tui: false                    # Disable the interactive TUI dashboard (enabled by default when a real terminal is detected).
# debug_output: false           # Save raw Claude output to .fabrik/debug/ for diagnosing prompt issues.
# version: ""                   # Project version shown in TUI footer. Auto-inferred from package.json/go.mod if not set.
`

// parseProjectURL parses a GitHub Project URL and returns owner, project number
// (as string), and ownerType ("user" or "organization").
// Accepted forms (on github.com or, when ghesHost is non-empty, on the
// configured GHES host as well):
//
//	https://github.com/users/<username>/projects/<N>
//	https://github.com/users/<username>/projects/<N>/views/<V>
//	https://github.com/orgs/<orgname>/projects/<N>
//	https://github.com/orgs/<orgname>/projects/<N>/views/<V>
//
// A /views/<N> suffix is silently ignored.
//
// ghesHost is the already-normalized (bare hostname) GHES host resolved from
// --ghes-host/FABRIK_GHES_HOST, or "" if none is configured. When "", only
// github.com is accepted — byte-identical to pre-GHES behavior, including
// error text (existing callers/fixtures depend on this).
func parseProjectURL(rawURL, ghesHost string) (owner, project, ownerType string, err error) {
	u, parseErr := url.Parse(rawURL)
	if parseErr != nil {
		return "", "", "", fmt.Errorf("invalid URL %q: %w", rawURL, parseErr)
	}
	if ghesHost == "" {
		if u.Host != "github.com" {
			return "", "", "", fmt.Errorf("invalid project URL %q: host must be github.com", rawURL)
		}
	} else if u.Host != "github.com" && u.Host != ghesHost {
		return "", "", "", fmt.Errorf("invalid project URL %q: host must be github.com or %s", rawURL, ghesHost)
	}

	// Split path into clean segments, dropping empty strings.
	segments := splitPathSegments(u.Path)

	// Expected: [users|orgs, <name>, projects, <N>] optionally followed by [views, <V>]
	if len(segments) < 4 {
		return "", "", "", fmt.Errorf("invalid project URL %q: expected /users/<name>/projects/<N> or /orgs/<name>/projects/<N>", rawURL)
	}

	kindSeg := segments[0]
	nameSeg := segments[1]
	projectsLiteral := segments[2]
	numSeg := segments[3]

	if kindSeg != "users" && kindSeg != "orgs" {
		return "", "", "", fmt.Errorf("invalid project URL %q: path must start with /users/ or /orgs/", rawURL)
	}
	if projectsLiteral != "projects" {
		return "", "", "", fmt.Errorf("invalid project URL %q: expected /projects/<N> after owner name", rawURL)
	}
	// Validate that <N> is a positive integer.
	n, convErr := strconv.Atoi(numSeg)
	if convErr != nil || n <= 0 {
		return "", "", "", fmt.Errorf("invalid project URL %q: project number %q must be a positive integer", rawURL, numSeg)
	}

	if kindSeg == "users" {
		ownerType = "user"
	} else {
		ownerType = "organization"
	}
	return nameSeg, numSeg, ownerType, nil
}

// splitPathSegments splits a URL path into non-empty segments.
func splitPathSegments(p string) []string {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// writeConfigTemplate writes the .fabrik/config.yaml template.
// owner, repoFlag, project, ownerType, user are pre-populated values (from a
// URL or flag). repoFlag is only known on the --create-board path (a project
// URL carries no repo); the plain URL-provided flow always passes "" for it,
// same as before this parameter existed.
// ghesHost is the resolved --ghes-host/FABRIK_GHES_HOST value, or "" if none
// configured; it is persisted into the written config regardless of which
// branch below runs, so an operator who supplies it once to `init` does not
// have to supply it again to every subsequent `fabrik` invocation.
// If any are empty and stdin is a TTY, the user is prompted for missing values.
// When owner is non-empty (URL provided), only user is prompted (if empty and TTY).
// When owner is empty, the full interactive prompt runs for all four fields.
func writeConfigTemplate(owner, repoFlag, project, ownerType, user, ghesHost string, force bool) error {
	configPath := ".fabrik/config.yaml"

	if !force {
		if _, err := os.Stat(configPath); err == nil {
			fmt.Printf("  skip   %s (already exists; use --force to overwrite)\n", configPath)
			return nil
		}
	}

	content := configYAMLTemplate

	isTTY := isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())

	switch {
	case owner != "":
		// URL-provided (or --create-board) flow: owner/project/ownerType are
		// known; only prompt for user.
		if user == "" && isTTY {
			user = promptForUser()
		}
		content = buildConfigWithValues(owner, repoFlag, project, ownerType, user, ghesHost)
	case isTTY:
		// Full interactive flow: prompt for all four required fields.
		o, repo, proj, u := promptRequiredValues()
		if o != "" || repo != "" || proj != "" || u != "" || ghesHost != "" {
			content = buildConfigWithValues(o, repo, proj, "", u, ghesHost)
		}
	case ghesHost != "":
		// No project URL and no TTY to prompt, but a GHES host was still
		// resolved from --ghes-host/FABRIK_GHES_HOST — persist it so it
		// doesn't need to be supplied again on every subsequent run.
		content = buildConfigWithValues("", "", "", "", "", ghesHost)
	}

	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", configPath, err)
	}
	fmt.Printf("  config: %s\n", configPath)
	return nil
}

// promptForUser prompts for a single GitHub username interactively.
func promptForUser() string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("\nFabrik interactive setup — press Enter to skip and fill in later.\n")
	fmt.Printf("  Your GitHub username: ")
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

// promptRequiredValues reads owner/repo/project/user from stdin interactively.
// Returns empty strings for any field the user skips (just hits enter).
func promptRequiredValues() (owner, repo, project, user string) {
	reader := bufio.NewReader(os.Stdin)
	prompt := func(label string) string {
		fmt.Printf("  %s: ", label)
		line, _ := reader.ReadString('\n')
		return strings.TrimSpace(line)
	}
	fmt.Println("\nFabrik interactive setup — press Enter to skip a field and fill it in later.")
	owner = prompt("GitHub owner (org or username)")
	repo = prompt("GitHub repository name (leave blank for multi-repo)")
	project = prompt("GitHub project number")
	user = prompt("Your GitHub username")
	return
}

// buildConfigWithValues returns a config.yaml where the supplied values are
// written as uncommented entries; unset values remain commented out.
// ownerType and ghesHost are written into the optional section when non-empty.
func buildConfigWithValues(owner, repo, project, ownerType, user, ghesHost string) string {
	lines := strings.Split(configYAMLTemplate, "\n")
	var out []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "# owner:") && owner != "":
			out = append(out, "owner: "+owner)
		case strings.HasPrefix(line, "# repo:") && repo != "":
			out = append(out, "repo: "+repo)
		case strings.HasPrefix(line, "# project:") && project != "":
			out = append(out, "project: "+project)
		case strings.HasPrefix(line, "# user:") && user != "":
			out = append(out, "user: "+user)
		case strings.HasPrefix(line, "# owner_type:") && ownerType != "":
			out = append(out, "owner_type: "+ownerType)
		case strings.HasPrefix(line, "# ghes_host:") && ghesHost != "":
			out = append(out, "ghes_host: "+ghesHost)
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// runInit implements the `fabrik init` subcommand.
// It extracts the embedded default stage YAML files into .fabrik/stages/
// and the Fabrik plugin into .fabrik/plugin/ in the current directory.
// Existing files are skipped unless --force is passed.
//
// An optional positional argument may be a GitHub Project URL of the form:
//
//	https://github.com/users/<name>/projects/<N>
//	https://github.com/orgs/<name>/projects/<N>
//
// When provided, owner, project, and owner_type are parsed from the URL.
// The --user flag sets the GitHub username for fully non-interactive setup.
//
// A GitHub Enterprise Server project URL (host matching --ghes-host or
// FABRIK_GHES_HOST) is also accepted. init runs before .fabrik/config.yaml
// exists, so the host can only come from the flag or env var, never from
// config.yaml — resolveGHESHost is called with a zero-value ProjectConfig
// for exactly this reason. The resolved host is persisted into the written
// config so it doesn't need to be supplied again on every subsequent run.
func runInit(args []string) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fset.Bool("force", false, "Overwrite existing files")
	userFlag := fset.String("user", "", "Your GitHub username")
	ghesHostFlag := fset.String("ghes-host", "", "GitHub Enterprise Server hostname, e.g. github.example.com (also FABRIK_GHES_HOST)")
	createBoard := fset.Bool("create-board", false, "Create a new GitHub Project (v2) board from the just-extracted stage configs, linked to --owner/--repo. Organization-owned repos only (see #770). Mutually exclusive with the positional <project-url> argument.")
	ownerFlag := fset.String("owner", "", "GitHub org (owner) to create the board under; required with --create-board")
	repoFlag := fset.String("repo", "", "GitHub repository to link the new board to; required with --create-board")
	titleFlag := fset.String("title", "", "Project board title for --create-board (default: \"<repo> Fabrik Pipeline\")")
	tokenFlag := fset.String("token", "", "GitHub token for --create-board (or FABRIK_TOKEN / GITHUB_TOKEN)")

	fset.Usage = func() {
		fmt.Fprintf(fset.Output(), "Usage: fabrik init [<project-url>] [flags]\n\n")
		fmt.Fprintf(fset.Output(), "Arguments:\n")
		fmt.Fprintf(fset.Output(), "  <project-url>    GitHub Project URL (optional); pre-fills owner, project number,\n")
		fmt.Fprintf(fset.Output(), "                   and owner_type in .fabrik/config.yaml.\n")
		fmt.Fprintf(fset.Output(), "                   Forms: https://github.com/orgs/<org>/projects/<N>\n")
		fmt.Fprintf(fset.Output(), "                          https://github.com/users/<user>/projects/<N>\n")
		fmt.Fprintf(fset.Output(), "                   A GitHub Enterprise Server host is also accepted when\n")
		fmt.Fprintf(fset.Output(), "                   --ghes-host or FABRIK_GHES_HOST is set.\n")
		fmt.Fprintf(fset.Output(), "                   Not used together with --create-board, which creates a\n")
		fmt.Fprintf(fset.Output(), "                   board rather than linking to an existing one.\n\n")
		fmt.Fprintf(fset.Output(), "Flags:\n")
		fset.PrintDefaults()
	}

	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() > 1 {
		return fmt.Errorf("init: too many positional arguments (expected at most one project URL)")
	}
	if *createBoard && fset.NArg() == 1 {
		return fmt.Errorf("init: --create-board creates a new project board and cannot be combined with a <project-url> argument, which links to an existing one")
	}
	if *createBoard && *ownerFlag == "" {
		return fmt.Errorf("init: --create-board requires --owner")
	}
	if *createBoard && *repoFlag == "" {
		return fmt.Errorf("init: --create-board requires --repo")
	}

	// Resolve GHES host from flag > FABRIK_GHES_HOST env var. No config.yaml
	// fallback — it doesn't exist yet at init time — so a zero-value
	// ProjectConfig is passed deliberately, not loaded from disk.
	ghesHost := resolveGHESHost(*ghesHostFlag, config.ProjectConfig{})

	// Parse URL if provided — must happen before any filesystem writes.
	var owner, project, ownerType, repo string
	if fset.NArg() == 1 {
		var err error
		owner, project, ownerType, err = parseProjectURL(fset.Arg(0), ghesHost)
		if err != nil {
			return err
		}
	}

	// Extract stage configs
	stagesDir := ".fabrik/stages"
	if err := os.MkdirAll(stagesDir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", stagesDir, err)
	}

	entries, err := fs.ReadDir(stages.DefaultStages, "examples")
	if err != nil {
		return fmt.Errorf("reading embedded stages: %w", err)
	}

	wrote := 0
	skipped := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		destPath := filepath.Join(stagesDir, entry.Name())
		if !*force {
			if _, statErr := os.Stat(destPath); statErr == nil {
				skipped++
				continue
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("checking %s: %w", destPath, statErr)
			}
		}
		data, err := stages.DefaultStages.ReadFile(path.Join("examples", entry.Name()))
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", destPath, err)
		}
		wrote++
	}

	if skipped > 0 {
		fmt.Printf("  stages: %d written, %d skipped (use --force to overwrite)\n", wrote, skipped)
	} else {
		fmt.Printf("  stages: %d written\n", wrote)
	}

	// Extract plugin
	pluginDir := ".fabrik/plugin"
	pluginWrote := 0
	pluginSkipped := 0
	err = fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		destPath := filepath.Join(pluginDir, rel)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		if !*force {
			if _, statErr := os.Stat(destPath); statErr == nil {
				pluginSkipped++
				return nil
			}
		}

		data, readErr := fabrikplugin.FabrikPlugin.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", p, readErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
		}
		if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
			return fmt.Errorf("writing %s: %w", destPath, writeErr)
		}
		pluginWrote++
		return nil
	})
	if err != nil {
		return fmt.Errorf("extracting plugin: %w", err)
	}

	if pluginSkipped > 0 {
		fmt.Printf("  plugin: %d written, %d skipped\n", pluginWrote, pluginSkipped)
	} else {
		fmt.Printf("  plugin: %d skill files written\n", pluginWrote)
	}

	// R1: create a fully-configured board from the stage configs just
	// written above, before .fabrik/config.yaml is generated so its
	// owner/project/owner_type can be pre-filled from the result — the same
	// role the URL-provided flow's owner/project/ownerType play below.
	if *createBoard {
		number, resolvedOwnerType, err := runCreateBoard(*ownerFlag, *repoFlag, *titleFlag, ghesHost, *tokenFlag, stagesDir)
		if err != nil {
			return fmt.Errorf("--create-board: %w", err)
		}
		owner = *ownerFlag
		repo = *repoFlag
		project = number
		ownerType = resolvedOwnerType
	}

	// Generate .fabrik/config.yaml template
	if err := writeConfigTemplate(owner, repo, project, ownerType, *userFlag, ghesHost, *force); err != nil {
		return err
	}

	// If running inside a git repo, add .fabrik working directories to
	// .git/info/exclude so they don't pollute the user's git status.
	// This is a no-op in non-git directories (the recommended setup).
	if err := writeGitExclude(); err != nil {
		return err
	}

	fmt.Println("\nFabrik is ready. Stage configs and plugin skills are in .fabrik/")
	fmt.Println("Edit .fabrik/config.yaml with your project settings, then run fabrik.")
	return nil
}

// runCreateBoard implements R1: it creates a new GitHub Project (v2) board
// under owner, linked to owner/repo, with Status options set from the stage
// configs already written to stagesDir (by the caller, before this runs) —
// in stage Order, so the board and config can never disagree from minute
// one. title defaults to "<repo> Fabrik Pipeline" when empty.
//
// Organization-only (R7): the owner is resolved fresh via ResolveOwner (a
// brand-new board has no existing project to read OwnerType from, unlike
// repair's path) and refused before any mutation is attempted.
//
// The freshly created board's default Status field (Todo/In Progress/Done,
// GitHub's own project template) is replaced outright rather than repaired:
// unlike repairBoardCore's existing-board path, a brand-new project has no
// items yet, so there is nothing for R4's id-preservation contract to
// protect here — every option is created fresh, deliberately not reusing
// SetStatusFieldOptions's id-echo contract (StatusOptionInput.ID left nil
// for every entry).
//
// Returns the new project's number (as a string, for direct use in
// .fabrik/config.yaml's project: key) and the resolved owner type
// ("organization" — refuseIfUserOwnedBoard already rejected "user").
func runCreateBoard(owner, repo, title, ghesHost, tokenFlag, stagesDir string) (project, ownerType string, err error) {
	token, err := loadGitHubToken(tokenFlag)
	if err != nil {
		return "", "", err
	}
	client := newBoardGHClient(token, ghesHost)
	return createBoardCore(client, owner, repo, title, stagesDir)
}

// createBoardCore is the testable core of runCreateBoard, taking an
// already-constructed *gh.Client so tests can supply an httptest-backed one
// (mirroring refreshStagesWithReader's CLI-wrapper/testable-core split).
func createBoardCore(client *gh.Client, owner, repo, title, stagesDir string) (project, ownerType string, err error) {
	ownerID, ownerType, err := client.ResolveOwner(owner)
	if err != nil {
		return "", "", fmt.Errorf("resolving owner %q: %w", owner, err)
	}
	if err := refuseIfUserOwnedBoard(ownerType); err != nil {
		return "", "", err
	}

	repoID, err := client.FetchRepositoryID(owner, repo)
	if err != nil {
		return "", "", fmt.Errorf("resolving repository %s/%s: %w", owner, repo, err)
	}

	if title == "" {
		title = repo + " Fabrik Pipeline"
	}

	projectID, number, err := client.CreateProjectV2(ownerID, title, repoID)
	if err != nil {
		return "", "", fmt.Errorf("creating project board: %w", err)
	}
	fmt.Printf("  board: created %q (#%d)\n", title, number)

	desc := fmt.Sprintf("Managed by Fabrik — https://github.com/%s/%s (see .fabrik/stages/)", owner, repo)
	if err := client.SetProjectDescription(projectID, desc); err != nil {
		// Non-fatal: the board is usable without a description. Fail loud
		// but continue — R1 does not require a description to succeed.
		fmt.Fprintf(os.Stderr, "  warning: could not set project description: %v\n", err)
	}

	sf, err := client.FetchStatusField(projectID)
	if err != nil {
		return "", "", fmt.Errorf("fetching Status field of newly created board: %w", err)
	}

	allStages, err := stages.LoadAll(stagesDir)
	if err != nil {
		return "", "", fmt.Errorf("loading stage configs from %s: %w", stagesDir, err)
	}
	names := requiredStageColumnNames(allStages)
	if len(names) == 0 {
		return "", "", fmt.Errorf("no stage configs with board columns found in %s — nothing to create Status columns for", stagesDir)
	}

	options := make([]gh.StatusOptionInput, 0, len(names))
	for _, name := range names {
		options = append(options, gh.StatusOptionInput{Name: name, Color: "GRAY", Description: ""})
	}
	if err := client.SetStatusFieldOptions(sf.FieldID, options); err != nil {
		return "", "", fmt.Errorf("setting Status columns on newly created board: %w", err)
	}
	fmt.Printf("  board: Status columns set: %s\n", strings.Join(names, ", "))

	return strconv.Itoa(number), ownerType, nil
}

// writeGitExclude adds Fabrik working directories to .git/info/exclude
// if running inside a git repository. Idempotent — skips entries that
// already exist. Does nothing if not in a git repo.
func writeGitExclude() error {
	excludePath := filepath.Join(".git", "info", "exclude")
	if _, err := os.Stat(filepath.Join(".git", "info")); os.IsNotExist(err) {
		return nil // not in a git repo
	}

	entries := []string{
		".fabrik/repos/",
		".fabrik/worktrees/",
		".fabrik/debug/",
		".fabrik/history.json",
		".fabrik/warnings.json",
	}

	existing, _ := os.ReadFile(excludePath)
	content := string(existing)

	var added int
	for _, entry := range entries {
		if strings.Contains(content, entry) {
			continue
		}
		if !strings.HasSuffix(content, "\n") && len(content) > 0 {
			content += "\n"
		}
		content += entry + "\n"
		added++
	}

	if added > 0 {
		if err := os.WriteFile(excludePath, []byte(content), 0644); err != nil {
			return fmt.Errorf("writing .git/info/exclude: %w", err)
		}
		fmt.Printf("  git exclude: %d entries added to .git/info/exclude\n", added)
	}
	return nil
}
