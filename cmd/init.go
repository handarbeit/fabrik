package cmd

import (
	"bufio"
	"context"
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
	"github.com/handarbeit/fabrik/engine"
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

# GitHub App authentication (alternative to FABRIK_TOKEN; see fabrik init --github-app):
# github_app_id: 0                          # GitHub App ID.
# github_app_private_key_path: ""           # Path to the App's private key PEM.
# github_app_installation_id: 0             # Installation ID pinned for this org.
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

// configValues holds every value writeConfigTemplate/buildConfigWithValues
// can write into .fabrik/config.yaml — a struct rather than positional
// string/int64 params because the set has grown past what's readable
// positionally (owner/repo/project/ownerType/user/ghesHost, plus the three
// GitHub App auth fields #1715 adds below).
type configValues struct {
	Owner     string
	Repo      string // only known on the --create-board path; a plain project URL carries no repo.
	Project   string
	OwnerType string
	User      string
	// GHESHost is the resolved --ghes-host/FABRIK_GHES_HOST value, or "" if
	// none configured; persisted regardless of which writeConfigTemplate
	// branch runs, so an operator who supplies it once to `init` does not
	// have to supply it again to every subsequent `fabrik` invocation.
	GHESHost string
	// GitHubAppID, GitHubAppPrivateKeyPath, and GitHubAppInstallationID are
	// populated only by `fabrik init --github-app` (#1715), once its setup
	// flow has resolved a client — see runGitHubAppSetup. Zero/"" means
	// unset, matching config.ProjectConfig's own nil-means-unset convention
	// for the first and third (though these are plain int64/string here,
	// not pointers — writeConfigTemplate only ever adds these fields, never
	// clears an existing config's values, so there's no "explicitly zero"
	// case to distinguish from "not given this run").
	GitHubAppID             int64
	GitHubAppPrivateKeyPath string
	GitHubAppInstallationID int64
}

// writeConfigTemplate writes the .fabrik/config.yaml template from v.
// If any of v.Owner/v.Repo/v.Project/v.User are empty and stdin is a TTY,
// the user is prompted for missing values.
// When v.Owner is non-empty (URL provided, or --create-board/--github-app),
// only user is prompted (if empty and TTY). When v.Owner is empty, the full
// interactive prompt runs for all four fields.
func writeConfigTemplate(v configValues, force bool) error {
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
	case v.Owner != "":
		// URL-provided (or --create-board/--github-app) flow: owner/project/
		// ownerType are known; only prompt for user.
		if v.User == "" && isTTY {
			v.User = promptForUser()
		}
		content = buildConfigWithValues(v)
	case isTTY:
		// Full interactive flow: prompt for all four required fields.
		o, repo, proj, u := promptRequiredValues()
		if o != "" || repo != "" || proj != "" || u != "" || v.GHESHost != "" {
			v.Owner, v.Repo, v.Project, v.OwnerType, v.User = o, repo, proj, "", u
			content = buildConfigWithValues(v)
		}
	case v.GHESHost != "":
		// No project URL and no TTY to prompt, but a GHES host was still
		// resolved from --ghes-host/FABRIK_GHES_HOST — persist it so it
		// doesn't need to be supplied again on every subsequent run.
		content = buildConfigWithValues(v)
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

// buildConfigWithValues returns a config.yaml where v's non-zero values are
// written as uncommented entries; unset (zero-value) fields remain commented
// out.
func buildConfigWithValues(v configValues) string {
	lines := strings.Split(configYAMLTemplate, "\n")
	var out []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "# owner:") && v.Owner != "":
			out = append(out, "owner: "+v.Owner)
		case strings.HasPrefix(line, "# repo:") && v.Repo != "":
			out = append(out, "repo: "+v.Repo)
		case strings.HasPrefix(line, "# project:") && v.Project != "":
			out = append(out, "project: "+v.Project)
		case strings.HasPrefix(line, "# user:") && v.User != "":
			out = append(out, "user: "+v.User)
		case strings.HasPrefix(line, "# owner_type:") && v.OwnerType != "":
			out = append(out, "owner_type: "+v.OwnerType)
		case strings.HasPrefix(line, "# ghes_host:") && v.GHESHost != "":
			out = append(out, "ghes_host: "+v.GHESHost)
		case strings.HasPrefix(line, "# github_app_id:") && v.GitHubAppID != 0:
			out = append(out, fmt.Sprintf("github_app_id: %d", v.GitHubAppID))
		case strings.HasPrefix(line, "# github_app_private_key_path:") && v.GitHubAppPrivateKeyPath != "":
			out = append(out, "github_app_private_key_path: "+v.GitHubAppPrivateKeyPath)
		case strings.HasPrefix(line, "# github_app_installation_id:") && v.GitHubAppInstallationID != 0:
			out = append(out, fmt.Sprintf("github_app_installation_id: %d", v.GitHubAppInstallationID))
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// resolveGitHubAppInitFlagsFromEnv fills in idFlag/keyPathFlag/
// installationIDFlag from FABRIK_GITHUB_APP_ID/FABRIK_GITHUB_APP_PRIVATE_KEY_PATH/
// FABRIK_GITHUB_APP_INSTALLATION_ID for any flag left at its zero value —
// flag > env, mirroring resolveGitHubAppConfig's precedence (cmd/root.go)
// minus its config.yaml layer, which doesn't exist yet at init time. Mutates
// the flag values in place (like resolveGitHubAppConfig mutates cfg) so
// every subsequent check in runInit (the adopt-pair all-or-nothing
// validation, runGitHubAppSetup itself) sees the fully-resolved value
// without needing to know whether it came from a flag or the environment.
func resolveGitHubAppInitFlagsFromEnv(idFlag *int64, keyPathFlag *string, installationIDFlag *int64) error {
	if *idFlag == 0 {
		if v := os.Getenv("FABRIK_GITHUB_APP_ID"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("FABRIK_GITHUB_APP_ID=%q is invalid (must be an integer)", v)
			}
			*idFlag = n
		}
	}
	if *keyPathFlag == "" {
		if v := os.Getenv("FABRIK_GITHUB_APP_PRIVATE_KEY_PATH"); v != "" {
			*keyPathFlag = v
		}
	}
	if *installationIDFlag == 0 {
		if v := os.Getenv("FABRIK_GITHUB_APP_INSTALLATION_ID"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("FABRIK_GITHUB_APP_INSTALLATION_ID=%q is invalid (must be an integer)", v)
			}
			*installationIDFlag = n
		}
	}
	return nil
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
	ownerFlag := fset.String("owner", "", "GitHub org (owner) to create the board under; required with --create-board and --github-app")
	repoFlag := fset.String("repo", "", "GitHub repository to link the new board to; required with --create-board")
	titleFlag := fset.String("title", "", "Project board title for --create-board (default: \"<repo> Fabrik Pipeline\")")
	tokenFlag := fset.String("token", "", "GitHub token for --create-board (or FABRIK_TOKEN / GITHUB_TOKEN)")
	githubApp := fset.Bool("github-app", false, "Guided GitHub App auth setup: register a new App via the manifest flow (or adopt an existing one with --github-app-id/--github-app-private-key-path), verify its granted permissions, and populate github_app_* in .fabrik/config.yaml. Requires --owner. Organization-owned targets only (see #770).")
	githubAppIDFlag := fset.Int64("github-app-id", 0, "Adopt an existing GitHub App by ID instead of creating one via the manifest flow; requires --github-app-private-key-path (also FABRIK_GITHUB_APP_ID)")
	githubAppKeyPathFlag := fset.String("github-app-private-key-path", "", "Path to an existing GitHub App's private key PEM, for --github-app-id adoption; requires --github-app-id (also FABRIK_GITHUB_APP_PRIVATE_KEY_PATH)")
	githubAppInstallationIDFlag := fset.Int64("github-app-installation-id", 0, "Explicit installation ID to pin, skipping discovery (optional; also FABRIK_GITHUB_APP_INSTALLATION_ID)")
	webhooksFlag := fset.Bool("webhooks", false, "Include webhook-management permission in the App's manifest/verification, matching a --webhooks engine deployment")
	noBrowserFlag := fset.Bool("no-browser", false, "Skip automatic browser launch during --github-app setup; auto-enabled when stdin is not a terminal")

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
		fmt.Fprintf(fset.Output(), "                   board rather than linking to an existing one, or with --github-app,\n")
		fmt.Fprintf(fset.Output(), "                   which sets owner/project from --owner instead.\n\n")
		fmt.Fprintf(fset.Output(), "                   --github-app drives guided GitHub App auth setup (register via\n")
		fmt.Fprintf(fset.Output(), "                   the manifest flow, or adopt an existing App with --github-app-id/\n")
		fmt.Fprintf(fset.Output(), "                   --github-app-private-key-path), verifies the installation's\n")
		fmt.Fprintf(fset.Output(), "                   granted permissions, and populates github_app_* in\n")
		fmt.Fprintf(fset.Output(), "                   .fabrik/config.yaml. Combine with --create-board to also create\n")
		fmt.Fprintf(fset.Output(), "                   the board using the App's own client.\n\n")
		fmt.Fprintf(fset.Output(), "Flags:\n")
		fset.PrintDefaults()
	}

	if err := fset.Parse(args); err != nil {
		return err
	}
	// Review finding (PR #1731): the three --github-app-id/--github-app-
	// private-key-path/--github-app-installation-id flags' help text claims
	// FABRIK_GITHUB_APP_* env-var fallback, matching the wording used for
	// the top-level `fabrik` command's identically-named flags (which
	// genuinely fall back via resolveGitHubAppConfig, cmd/root.go) — but
	// nothing here previously read those env vars. An operator who already
	// sets them for the engine and expects the same behavior from `fabrik
	// init --github-app` would otherwise silently fall through to the
	// create-a-new-App path instead of adopting, registering a duplicate
	// App. Only flag > env is needed here (no config.yaml layer — it
	// doesn't exist yet at init time), mirroring resolveGHESHost's own
	// zero-value-ProjectConfig treatment below.
	//
	// Bot review finding (PR #1731, second pass): this must run only when
	// --github-app was actually given. Run unconditionally, an operator who
	// simply has FABRIK_GITHUB_APP_ID/FABRIK_GITHUB_APP_PRIVATE_KEY_PATH/
	// FABRIK_GITHUB_APP_INSTALLATION_ID exported in their shell (the exact
	// setup the top-level `fabrik` command's own help text encourages) would
	// see those values pulled into the flag variables on a plain `fabrik
	// init` with no GitHub-App flags at all — tripping the "requires
	// --github-app" validation below and failing the most basic command
	// outright. A malformed FABRIK_GITHUB_APP_ID would fail it even harder
	// (a hard parse error, again with --github-app never mentioned). Gating
	// on *githubApp makes this resolution — and its validation — exist only
	// in the context it was designed for.
	if *githubApp {
		if err := resolveGitHubAppInitFlagsFromEnv(githubAppIDFlag, githubAppKeyPathFlag, githubAppInstallationIDFlag); err != nil {
			return err
		}
	}
	if fset.NArg() > 1 {
		return fmt.Errorf("init: too many positional arguments (expected at most one project URL)")
	}
	if *createBoard && fset.NArg() == 1 {
		return fmt.Errorf("init: --create-board creates a new project board and cannot be combined with a <project-url> argument, which links to an existing one")
	}
	if *githubApp && fset.NArg() == 1 {
		return fmt.Errorf("init: --github-app sets owner/project from --owner (and, with --create-board, from the board it creates) and cannot be combined with a <project-url> argument — " +
			"the two could name different owners, silently writing a .fabrik/config.yaml whose owner and project belong to different accounts; run `fabrik init --github-app --owner <org> ...` " +
			"first, then `fabrik init <project-url>` separately (with --force) to link an existing board under the same owner")
	}
	if *createBoard && *ownerFlag == "" {
		return fmt.Errorf("init: --create-board requires --owner")
	}
	if *createBoard && *repoFlag == "" {
		return fmt.Errorf("init: --create-board requires --repo")
	}
	if *createBoard && !*force {
		// R1 review finding (PR #1718): without this guard, a repo that's
		// already onboarded (owner/project already set) would still create a
		// brand-new GitHub Project — and then either silently skip writing it
		// into .fabrik/config.yaml (writeConfigTemplate's own no-op-without
		// --force below leaves the new board dangling, pointed at only by a
		// stdout line) or, on a second run, create a second, separate board.
		// Refuse before any network call fires; --force opts into overwriting
		// the existing config with the new board's details.
		existing, err := config.LoadProjectConfig()
		if err != nil {
			return err
		}
		if existing.Owner != "" || existing.ProjectNum != nil {
			projectDesc := "unset"
			if existing.ProjectNum != nil {
				projectDesc = strconv.Itoa(*existing.ProjectNum)
			}
			return fmt.Errorf("init: --create-board refused — .fabrik/config.yaml already configures owner=%q project=%s; "+
				"creating a new board here would either be silently discarded (config left pointing at the old board) or, "+
				"on a repeat run, create yet another duplicate board; pass --force to create the new board and overwrite "+
				"the existing config with it, or drop --create-board and edit .fabrik/config.yaml by hand to link an "+
				"existing board instead", existing.Owner, projectDesc)
		}
	}
	if *githubApp && *ownerFlag == "" {
		return fmt.Errorf("init: --github-app requires --owner (the org to register/adopt the App on and discover its installation for)")
	}
	adoptID := *githubAppIDFlag != 0
	adoptKey := *githubAppKeyPathFlag != ""
	if adoptID != adoptKey {
		return fmt.Errorf("init: --github-app-id and --github-app-private-key-path must be given together (adopting an " +
			"existing App) or not at all (creating a new one via the manifest flow) — giving only one risks a fresh " +
			"manifest bootstrap silently overwriting whatever file already sits at the given key path")
	}
	if (adoptID || adoptKey) && !*githubApp {
		return fmt.Errorf("init: --github-app-id/--github-app-private-key-path require --github-app")
	}
	if *githubAppInstallationIDFlag != 0 && !*githubApp {
		return fmt.Errorf("init: --github-app-installation-id requires --github-app")
	}
	if *githubApp && !*force {
		// Review finding (PR #1731): without this guard, runGitHubAppSetup
		// below still runs to completion — registering/adopting a real App
		// on GitHub, minting an installation token, writing the private key
		// to disk — but writeConfigTemplate's own no-op-when-file-exists gate
		// (mirrored here exactly, since that's the actual condition that bites)
		// then silently discards the resolved github_app_id/
		// github_app_private_key_path/github_app_installation_id instead of
		// persisting them. The operator is left believing setup succeeded —
		// and, on the create path, with a live App now registered on GitHub —
		// while .fabrik/config.yaml never gains what's needed to use it.
		// Refuse before any network call fires, mirroring --create-board's
		// analogous guard above.
		if _, err := os.Stat(".fabrik/config.yaml"); err == nil {
			return fmt.Errorf("init: --github-app refused — .fabrik/config.yaml already exists; completing App " +
				"setup now would register/adopt a real GitHub App and mint credentials, then silently discard the " +
				"resolved github_app_id/github_app_private_key_path/github_app_installation_id (writeConfigTemplate " +
				"skips writing when the file already exists and --force is not given); pass --force to overwrite " +
				"the existing config with the new values, or run --github-app once to see the resolved values and " +
				"add the three github_app_* fields to the existing file by hand")
		}
	}

	// Resolve GHES host from flag > FABRIK_GHES_HOST env var. No config.yaml
	// fallback — it doesn't exist yet at init time — so a zero-value
	// ProjectConfig is passed deliberately, not loaded from disk.
	ghesHost := resolveGHESHost(*ghesHostFlag, config.ProjectConfig{})
	if *githubApp {
		// Review finding (PR #1731): the engine refuses GHES host +
		// GitHub-App-auth unconditionally at startup (RefuseGHESWithGitHubApp,
		// engine/github_app_auth.go) because internal/githubauth's client
		// construction doesn't yet derive correct GHES endpoints. Without this
		// check, setup would happily register/adopt an App against production
		// github.com (its BaseURL is never derived from ghesHost) and write a
		// ghes_host + github_app_* combination the engine then refuses
		// unconditionally on its very next startup — a confusing failure to
		// discover only after setup already reported success.
		if err := engine.RefuseGHESWithGitHubApp(ghesHost); err != nil {
			return fmt.Errorf("--github-app: %w", err)
		}
	}

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

	// --github-app (#1715): register or adopt a GitHub App installation and
	// verify its granted permissions, before --create-board below so a
	// combined `--github-app --create-board` invocation creates the board
	// with the App's own freshly-minted client instead of demanding a
	// second --token — the App auth this flow just set up already carries
	// organization_projects:write, exactly what board creation needs.
	var appSetup *githubAppSetupResult
	if *githubApp {
		noBrowser := *noBrowserFlag || !(isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd()))
		res, err := runGitHubAppSetup(context.Background(), githubAppSetupOptions{
			Owner:          *ownerFlag,
			AppID:          *githubAppIDFlag,
			PrivateKeyPath: *githubAppKeyPathFlag,
			InstallationID: *githubAppInstallationIDFlag,
			Webhooks:       *webhooksFlag,
			NoBrowser:      noBrowser,
		})
		if err != nil {
			return fmt.Errorf("--github-app: %w", err)
		}
		appSetup = res
		owner = *ownerFlag
	}

	// R1: create a fully-configured board from the stage configs just
	// written above, before .fabrik/config.yaml is generated so its
	// owner/project/owner_type can be pre-filled from the result — the same
	// role the URL-provided flow's owner/project/ownerType play below.
	if *createBoard {
		var number, resolvedOwnerType string
		var err error
		if appSetup != nil {
			number, resolvedOwnerType, err = createBoardCore(appSetup.Client, *ownerFlag, *repoFlag, *titleFlag, stagesDir)
		} else {
			number, resolvedOwnerType, err = runCreateBoard(*ownerFlag, *repoFlag, *titleFlag, ghesHost, *tokenFlag, stagesDir)
		}
		if err != nil {
			return fmt.Errorf("--create-board: %w", err)
		}
		owner = *ownerFlag
		repo = *repoFlag
		project = number
		ownerType = resolvedOwnerType
	}

	// Generate .fabrik/config.yaml template
	cv := configValues{Owner: owner, Repo: repo, Project: project, OwnerType: ownerType, User: *userFlag, GHESHost: ghesHost}
	if appSetup != nil {
		cv.GitHubAppID = appSetup.AppID
		cv.GitHubAppPrivateKeyPath = appSetup.PrivateKeyPath
		cv.GitHubAppInstallationID = appSetup.InstallationID
	}
	if err := writeConfigTemplate(cv, *force); err != nil {
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

	// Load stage configs and compute the required column set BEFORE creating
	// anything on GitHub. Both checks below are purely local (no network) —
	// deferring them until after CreateProjectV2, as originally written,
	// meant a missing/empty stages directory left a freshly created project
	// orphaned on GitHub: no .fabrik/config.yaml entry pointing at it (that's
	// only written by the caller after this function returns successfully)
	// and no idempotency guard against a retry creating a second, separate
	// project (review finding on PR #1718).
	allStages, err := stages.LoadAll(stagesDir)
	if err != nil {
		return "", "", fmt.Errorf("loading stage configs from %s: %w", stagesDir, err)
	}
	names := requiredStageColumnNames(allStages)
	if len(names) == 0 {
		return "", "", fmt.Errorf("no stage configs with board columns found in %s — nothing to create Status columns for", stagesDir)
	}

	if title == "" {
		title = repo + " Fabrik Pipeline"
	}

	projectID, number, err := client.CreateProjectV2(ownerID, title, repoID)
	if err != nil {
		return "", "", fmt.Errorf("creating project board: %w", err)
	}
	// refuseIfUserOwnedBoard above already rejected "user", so ownerType is
	// always "organization" here — the /orgs/ URL form is always correct.
	boardURL := fmt.Sprintf("https://github.com/orgs/%s/projects/%d", owner, number)
	fmt.Printf("  board: created %q (#%d) — %s\n", title, number, boardURL)

	// From this point on, the project already exists on GitHub. A failure
	// below must not be reported as if nothing happened: wrap it with the
	// board's own URL so the operator has a durable pointer to it (not just
	// the stdout line above, which may have scrolled past), and steer them
	// at the existing link-to-an-existing-board flow (`fabrik init
	// <project-url>`) to finish setup — rather than a bare retry of
	// --create-board, which would create a second, separate project.
	wrapPostCreateErr := func(step string, causeErr error) error {
		return fmt.Errorf("%s: %w — board %q (#%d) was already created at %s; fix the error, then run "+
			"`fabrik init %s` to link .fabrik/config.yaml to it (do not re-run --create-board, which would create a duplicate)",
			step, causeErr, title, number, boardURL, boardURL)
	}

	desc := fmt.Sprintf("Managed by Fabrik — https://github.com/%s/%s (see .fabrik/stages/)", owner, repo)
	if err := client.SetProjectDescription(projectID, desc); err != nil {
		// Non-fatal: the board is usable without a description. Fail loud
		// but continue — R1 does not require a description to succeed.
		fmt.Fprintf(os.Stderr, "  warning: could not set project description: %v\n", err)
	}

	sf, err := client.FetchStatusField(projectID)
	if err != nil {
		return "", "", wrapPostCreateErr("fetching Status field of newly created board", err)
	}

	options := make([]gh.StatusOptionInput, 0, len(names))
	for _, name := range names {
		options = append(options, gh.StatusOptionInput{Name: name, Color: "GRAY", Description: ""})
	}
	if err := client.SetStatusFieldOptions(sf.FieldID, options); err != nil {
		return "", "", wrapPostCreateErr("setting Status columns on newly created board", err)
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
		// Bot review finding (PR #1731): a fresh `--github-app` manifest run
		// writes the App's private key to defaultGitHubAppPrivateKeyPath and
		// its non-key metadata (App ID, slug, webhook secret, client
		// ID/secret) to engine.GitHubAppStatePath's default — unlike
		// .fabrik/config.yaml, which is deliberately committed, both of these
		// are per-operator secrets that must never enter the repo's history.
		// Added unconditionally, like every other entry above, regardless of
		// whether --github-app was used this run — cheap now, and protects
		// a later run that adds them without needing to re-run this step.
		// Referencing the same constant/func init_github_app.go itself uses
		// (rather than a second copy of the literal path) so the two can
		// never silently drift apart.
		defaultGitHubAppPrivateKeyPath,
		engine.GitHubAppStatePath("."),
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
