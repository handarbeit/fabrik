package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
	"github.com/handarbeit/fabrik/warnings"
)

// checkStageColumnAlignment validates that every configured non-cleanup stage
// has a matching column in the project board's Status field. It is called once
// from Run() before the first poll.
//
// Non-fatal errors (network failures, missing Status field) are logged as
// warnings and the check is skipped — only a successful fetch with genuine
// name mismatches is fatal. This prevents transient network errors from
// blocking startup.
//
// On success the fetched StatusField is stored in e.statusField so poll()'s
// lazy guard skips the redundant second FetchStatusField call.
func (e *Engine) checkStageColumnAlignment(ctx context.Context) error {
	board, err := e.readClient.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		e.logf(0, "startup", "warning: could not fetch project board for startup check: %v\n", err)
		return nil
	}
	if board.ProjectID == "" {
		e.logf(0, "startup", "warning: project board has no ID — skipping startup check\n")
		return nil
	}

	// Emit project metadata to the TUI so the footer can display the board title.
	if board.Title != "" {
		ownerSegment := "orgs"
		if board.OwnerType == "user" {
			ownerSegment = "users"
		}
		boardURL := fmt.Sprintf("https://github.com/%s/%s/projects/%d", ownerSegment, e.cfg.Owner, e.cfg.ProjectNum)
		e.emitStructural(tui.ProjectMetaEvent{BoardTitle: board.Title, BoardURL: boardURL})
	}

	sf, err := e.readClient.FetchStatusField(board.ProjectID)
	if err != nil {
		e.logf(0, "startup", "warning: could not fetch status field for startup check: %v\n", err)
		return nil
	}

	// Store the StatusField so poll()'s lazy fetch is skipped.
	e.mu.Lock()
	e.statusField = sf
	e.mu.Unlock()

	// Build the required stage set for column validation.
	// Cleanup stages are always excluded (they have no board column requirement).
	// Unmanaged stages (e.g. Backlog) are always excluded — they're parking
	// columns Fabrik recognizes but never requires a matching board column for,
	// regardless of merge_train.
	// Holding stages (e.g. Queued) are part of the required set whenever
	// configured, independent of the current merge_train mode — but their
	// SEVERITY when missing is keyed to reachability, not membership. See the
	// fatal/deferred split below, where this is applied. Every other
	// non-cleanup, non-unmanaged stage is required and fatal unconditionally.
	var checkStages []*stageNameOrder
	for _, s := range e.cfg.Stages {
		if s.CleanupWorktree {
			continue
		}
		if s.Unmanaged {
			continue
		}
		checkStages = append(checkStages, &stageNameOrder{name: s.Name, order: s.Order, holdingStage: s.HoldingStage})
	}
	// Sort by Order for deterministic mismatch report output.
	sort.Slice(checkStages, func(i, j int) bool {
		return checkStages[i].order < checkStages[j].order
	})

	// Find missing stages (stage name not in board options).
	var missing []*stageNameOrder
	for _, s := range checkStages {
		if _, ok := sf.Options[s.name]; !ok {
			missing = append(missing, s)
		}
	}

	// Split missing stages by severity, not by membership in the required set.
	// A holding stage's column is required whenever the stage is configured
	// (R1) — but a missing one is fatal only when merge_train: on, the only
	// mode with a live code path that writes to it (advanceToQueued, gated at
	// its sole call site; stages.NextStage unconditionally skips HoldingStage
	// per ADR-1072, so no other path can reach it). When merge_train is
	// off/unset the column is unreachable *today*, but the flag is an
	// operator-togglable runtime setting that never regenerates board
	// columns — a later `--merge-train on` restart would strand work against
	// an unvalidated board. So a missing holding column while off is not
	// silently ignored (that was the original bug, #1082) and not fatal
	// either (that was this fix's first cut, and it broke fresh installs:
	// `fabrik init` extracts every embedded default stage — including
	// queued.yaml — unconditionally, regardless of merge_train, so every
	// non-merge-train install acquires a holding stage it never asked for and
	// has no reason to have created a board column for). It is deferred to a
	// startup warning instead. See ADR-1421 addendum.
	var fatalMissing, deferredMissing []*stageNameOrder
	for _, s := range missing {
		if s.holdingStage && e.cfg.MergeTrain != "on" {
			deferredMissing = append(deferredMissing, s)
			continue
		}
		fatalMissing = append(fatalMissing, s)
	}

	// Find extra board columns: columns that no configured stage backs and that
	// are not a well-known unmanaged column. Unlike the missing-column check
	// above, this recognizes EVERY configured stage — including cleanup stages
	// (e.g. Done), holding stages (e.g. Queued), and unmanaged stages (e.g.
	// Backlog, via stages/examples/backlog.yaml) — because those columns are
	// legitimately backed by a stage even though they don't require one. The
	// hardcoded "Backlog" fallback below remains as a compat net for installs
	// that predate backlog.yaml and have no local copy of it: without a
	// configured Backlog stage, the loop above never adds "Backlog" to
	// `recognized`, so the hardcode is what keeps those installs silent. Once a
	// stage named Backlog is configured (unmanaged or not), the hardcode is a
	// harmless no-op (the map entry is already true). A genuine typo'd column
	// matches neither and is still surfaced.
	recognized := make(map[string]bool, len(e.cfg.Stages)+1)
	for _, s := range e.cfg.Stages {
		recognized[s.Name] = true
	}
	recognized["Backlog"] = true
	var extra []string
	for colName := range sf.Options {
		if !recognized[colName] {
			extra = append(extra, colName)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		e.logf(0, "startup", "warning: board has columns with no matching stage: %s\n", strings.Join(extra, ", "))
	}

	// Drift scan: warn about items whose board column doesn't match their
	// cleanup-stage complete label. Cleanup stages are terminal — board drift
	// for them cannot self-heal and each mismatch is a regression signal.
	// Non-cleanup stage mismatches are still in-flight and are ignored here.
	cleanupStageNames := make(map[string]bool, len(e.cfg.Stages))
	for _, s := range e.cfg.Stages {
		if s.CleanupWorktree {
			cleanupStageNames[s.Name] = true
		}
	}
	for _, item := range board.Items {
		for _, label := range item.Labels {
			if !strings.HasPrefix(label, "stage:") || !strings.HasSuffix(label, ":complete") {
				continue
			}
			stageName := strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":complete")
			if !cleanupStageNames[stageName] {
				continue
			}
			if item.Status != stageName {
				e.logf(item.Number, "startup", "warning: item #%d has label stage:%s:complete but board column is %q — board drift detected\n", item.Number, stageName, item.Status)
			}
		}
	}

	if len(fatalMissing) == 0 {
		// Only deferred (holding-stage, merge_train off/unset) misses remain,
		// if any. Not fatal — surfaced as a startup warning instead, so an
		// install that has never touched merge_train (e.g. every fresh
		// `fabrik init`, which extracts queued.yaml unconditionally) keeps
		// booting, while an operator who is about to flip merge_train on
		// gets advance notice that the board isn't ready for it.
		for _, s := range deferredMissing {
			e.logf(0, "startup", "warning: holding stage %q has no matching board column. "+
				"Not required to boot while merge_train is off, but its column serves a "+
				"terminal-only landing role for the merge train — never a column to move "+
				"items into manually — and must exist on the board before enabling "+
				"merge_train: on. Add a %q column to your GitHub Project board before "+
				"flipping merge_train on. See docs/state-machine.md for setup steps.\n",
				s.name, s.name)
		}
		return nil
	}

	// Build the list of all board column names for the error report.
	allCols := make([]string, 0, len(sf.Options))
	for colName := range sf.Options {
		allCols = append(allCols, colName)
	}
	sort.Strings(allCols)

	// Build a unified, per-required-stage present/missing report — R3 asks for
	// "every required option and which are present," not just the first
	// missing one. checkStages is already sorted by Order. This report always
	// shows every configured stage's actual present/missing state, including
	// any deferred-severity holding stage, even though it isn't itself the
	// reason startup is failing this time — the report answers "which columns
	// does my board have," not just "which column caused the failure."
	var report strings.Builder
	fmt.Fprintf(&report, "Fabrik startup check failed: stage/board column mismatch\n\n")
	fmt.Fprintf(&report, "Required Status options (derived from configured non-cleanup, non-unmanaged stages):\n")
	for _, s := range checkStages {
		status := "present"
		if _, ok := sf.Options[s.name]; !ok {
			status = "MISSING"
		}
		fmt.Fprintf(&report, "  - %s (order %d): %s\n", s.name, s.order, status)
	}
	fmt.Fprintf(&report, "\nBoard columns found:\n  %s\n\n", strings.Join(allCols, ", "))
	fmt.Fprintf(&report, "Fix: add the missing columns to your GitHub Project board, or update\n")
	fmt.Fprintf(&report, ".fabrik/stages/ to match your board column names (case-sensitive).\n")
	for _, s := range missing {
		if !s.holdingStage {
			continue
		}
		if e.cfg.MergeTrain != "on" {
			// This holding stage is only deferred-missing (merge_train is
			// off/unset) — the fatal report was triggered by a different,
			// co-occurring miss (fatalMissing is non-empty for some other
			// reason). Give it the same "not required to boot, but must
			// exist before enabling merge_train" framing as the deferred-only
			// path above, so the two report paths never disagree about the
			// same holding-stage entry (review finding).
			fmt.Fprintf(&report, "\nNote: %q is a holding stage. Its board column serves a terminal-only\n", s.name)
			fmt.Fprintf(&report, "landing role for the merge train — it is never a column to move items into\n")
			fmt.Fprintf(&report, "manually. Not required to boot while merge_train is off, but must exist on\n")
			fmt.Fprintf(&report, "the board before enabling merge_train: on. Add a `%s` column to your\n", s.name)
			fmt.Fprintf(&report, "GitHub Project board before flipping merge_train on. See\n")
			fmt.Fprintf(&report, "docs/state-machine.md for setup steps.\n")
			continue
		}
		fmt.Fprintf(&report, "\nNote: %q is a holding stage. Its board column serves a terminal-only\n", s.name)
		fmt.Fprintf(&report, "landing role for the merge train — it is never a column to move items into\n")
		fmt.Fprintf(&report, "manually. A Status of Todo/Backlog/null means Fabrik ignores the item, so\n")
		fmt.Fprintf(&report, "guessing wrong here silently stalls it.\n")
		fmt.Fprintf(&report, "Add a `%s` column to your GitHub Project board between `Validate` and `Done`,\n", s.name)
		fmt.Fprintf(&report, "then restart. See docs/state-machine.md for setup steps.\n")
		fmt.Fprintf(&report, "If you copied queued.yaml from a new Fabrik installation, ensure the column\n")
		fmt.Fprintf(&report, "name on the board matches the 'name' field in the YAML (case-sensitive).\n")
	}

	fmt.Fprint(os.Stderr, report.String())

	return fmt.Errorf("%s", report.String())
}

// stageNameOrder is a helper for sorting and reporting stage names.
type stageNameOrder struct {
	name         string
	order        int
	holdingStage bool
}

// runStartupTransientLabelScan is a one-shot recovery pass that runs after the
// first successful poll. It scans the Store for closed issues that still carry
// transient lifecycle labels — a condition that can occur when an issue closes
// mid-stage during a prior crash.
//
// Label availability note: when the cache was bootstrapped via BootstrapFromProbe
// (the default cold-start path), labels are absent from the Store and this scan
// is a no-op. The accepted gap: stale transient labels on closed terminal items
// will not be cleaned up at startup (very low probability — requires a crash
// between issue close and label removal in the Done stage). Active items are
// deep-fetched on the first probe cycle, which populates their labels normally,
// so those items are covered by the steady-state cleanup path.
// When bootstrapped via the full FetchProjectBoard path (webhook-paused / reconcile),
// the Store contains full label data and this scan is fully effective.
func (e *Engine) runStartupTransientLabelScan() {
	snaps := e.store.All()
	if len(snaps) == 0 {
		return
	}

	// Build a synthetic board containing only closed items that carry at least
	// one transient lifecycle label or a lock label. The cleanup helpers operate
	// on *gh.ProjectBoard items so we pass only the relevant subset.
	transientSet := make(map[string]bool, len(transientLifecycleLabels))
	for _, l := range transientLifecycleLabels {
		transientSet[l] = true
	}
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)

	var items []gh.ProjectItem
	for _, snap := range snaps {
		if !snap.IsClosed() {
			continue
		}
		labels := snap.Labels()
		hasStale := false
		for _, l := range labels {
			if transientSet[l] || l == lockLabel {
				hasStale = true
				break
			}
		}
		if !hasStale {
			continue
		}
		items = append(items, gh.ProjectItem{
			Number:   snap.Number(),
			Repo:     snap.Repo(),
			IsClosed: true,
			Labels:   labels,
		})
	}
	if len(items) == 0 {
		return
	}
	e.logf(0, "startup", "transient-label scan: %d closed item(s) with stale labels\n", len(items))
	board := &gh.ProjectBoard{Items: items}
	e.cleanupClosedIssueLocks(board)
	e.cleanupClosedIssueTransientLabels(board)
}

// captureGitMeta captures the current branch name, short commit SHA,
// origin/{baseBranch} SHA, and a human-readable UTC timestamp from the given
// worktree directory. Returns "unknown" values gracefully if git commands fail.
// mainSHA is empty (not "unknown") when it cannot be resolved — callers treat
// empty as "no data" rather than an error sentinel.
func captureGitMeta(workDir, baseBranch string) (branch, commit, mainSHA, timestamp string) {
	timestamp = time.Now().UTC().Format("2006-01-02 15:04 UTC")

	if workDir == "" {
		return "unknown", "unknown", "", timestamp
	}

	sha, err := gitRevParse(workDir, "HEAD")
	if err != nil || sha == "" {
		commit = "unknown"
	} else if len(sha) >= 8 {
		commit = sha[:8]
	} else {
		commit = sha
	}

	branchCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = workDir
	out, err := branchCmd.Output()
	if err != nil {
		branch = "unknown"
	} else {
		branch = strings.TrimSpace(string(out))
	}

	// Capture origin/{baseBranch} SHA for staleness tracking.
	// Store full SHA — it is used as a git revision in writeCodebaseChanges;
	// abbreviated SHAs can become ambiguous in larger repos.
	if baseBranch != "" {
		if mSHA, err := gitRevParse(workDir, "origin/"+baseBranch); err == nil {
			mainSHA = mSHA
		}
	}

	return branch, commit, mainSHA, timestamp
}

// checkHTTPSCredentials probes whether a git credential helper is configured
// when using HTTPS clone mode. Prints an advisory warning if none is found.
// Skip entirely when SSH mode is active or when HTTPS→SSH URL rewriting is in
// effect for github.com — no credential helper is needed in either case.
// This check is non-interactive and never prompts; it only reads git config.
func (e *Engine) checkHTTPSCredentials(hasSSHRewrite bool) {
	if e.cfg.GitSSH || hasSSHRewrite {
		return
	}
	cmd := exec.Command("git", "config", "credential.helper")
	out, err := cmd.Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		fmt.Printf("[startup] warn: no git credential helper configured; HTTPS cloning may prompt for credentials.\n")
		fmt.Printf("[startup] warn: configure one (e.g. git-credential-osxkeychain) or use --ssh / git_ssh: true in .fabrik/config.yaml.\n")
		fmt.Printf("[startup] note: existing bare clones retain their original remote URL and are not affected by this setting.\n")
	}
}

// resolveRepoAccess returns the cached write-access determination for
// owner/repo, probing and caching it on first call (at most once per repo per
// process run, per R3/AC4). It is the single shared resolver consulted by
// label seeding, checkAllowAutoMerge, and itemMayNeedWork's dispatch gate
// (ADR-1347) — whichever of those calls it first for a given repo is the one
// that fetches and logs.
//
// Fails open (CanPush: true) on a probe error (network failure, transient
// 5xx), matching this codebase's prior error posture for the same GET
// request: a genuinely managed repo should not lose dispatch/seeding for the
// rest of the process lifetime over a transient blip.
func (e *Engine) resolveRepoAccess(owner, repo string) gh.RepoAccess {
	key := owner + "/" + repo
	e.mu.Lock()
	access, cached := e.repoAccess[key]
	e.mu.Unlock()
	if cached {
		return access
	}

	access, err := e.client.FetchRepoAccess(owner, repo)
	if err != nil {
		e.logf(0, "warn", "could not determine repo access for %s: %v (assuming writable)\n", key, err)
		access = gh.RepoAccess{AllowAutoMerge: true, CanPush: true}
	}

	e.mu.Lock()
	existing, already := e.repoAccess[key]
	if already {
		access = existing
	} else {
		e.repoAccess[key] = access
	}
	e.mu.Unlock()

	if !already && !access.CanPush {
		e.logf(0, "startup", "%s: no write access — skipping label seeding and allow_auto_merge check; items from this repo will not be processed\n", key)
	}
	return access
}

// checkAllowAutoMerge queries the GitHub API for the allow_auto_merge setting on
// owner/repo and prints a WARNING if it is disabled. Non-fatal: API errors are
// logged at warn level and processing continues. The check fires at most once per
// repo per process run (guarded by checkedAutoMergeRepos). Skips the warning
// entirely — no API call beyond the shared resolveRepoAccess probe — when the
// token has no write access to the repo (R2): a repo Fabrik can't push to also
// can't have allow_auto_merge administered on it, and resolveRepoAccess has
// already logged the "no write access" notice once. Also clears any
// allow_auto_merge warning already recorded for the repo from a prior run when
// access was still present: a repo whose access is revoked between runs would
// otherwise keep an unfixable, now-moot warning immortal in
// .fabrik/warnings.json forever, since !CanPush never reaches this function's
// own Clear branch below again.
func (e *Engine) checkAllowAutoMerge(owner, repo string) {
	key := owner + "/" + repo
	e.mu.Lock()
	already := e.checkedAutoMergeRepos[key]
	e.checkedAutoMergeRepos[key] = true
	e.mu.Unlock()
	if already {
		return
	}
	access := e.resolveRepoAccess(owner, repo)
	if !access.CanPush {
		_ = warnings.Clear("allow_auto_merge:" + key)
		return
	}
	if !access.AllowAutoMerge {
		e.logf(0, "startup", "WARNING: %s has allow_auto_merge disabled.\n", key)
		e.logf(0, "startup", "WARNING: yolo issues on this repo will reach Validate complete but their PRs will not merge.\n")
		e.logf(0, "startup", "WARNING: Fix: gh api -X PATCH repos/%s -f allow_auto_merge=true\n", key)
		_ = warnings.Record(warnings.Entry{
			Key:       "allow_auto_merge:" + key,
			Type:      "allow_auto_merge",
			Title:     "allow_auto_merge disabled on " + key,
			Detail:    fmt.Sprintf("yolo issues on this repo will reach Validate complete but their PRs will not merge.\n\nFix: gh api -X PATCH repos/%s -f allow_auto_merge=true", key),
			FixAction: "shell_command",
			FixParams: map[string]string{"cmd": fmt.Sprintf("gh api -X PATCH repos/%s -f allow_auto_merge=true", key)},
		})
	} else {
		_ = warnings.Clear("allow_auto_merge:" + key)
	}
}

// checkURLRewrite detects whether git has URL rewriting configured that
// transparently redirects github.com HTTPS URLs to SSH (e.g. via
// url.git@github.com:.insteadOf = https://github.com/ in ~/.gitconfig).
// Returns true when such HTTPS→SSH rewriting is active. The result is used
// solely to suppress the checkHTTPSCredentials advisory (no credential helper
// is needed when HTTPS is rewritten to SSH); this is a normal, expected config
// state, so detecting it is silent — nothing is printed.
func (e *Engine) checkURLRewrite() bool {
	cmd := exec.Command("git", "config", "--get-regexp", `url\..*\.insteadOf`)
	out, _ := cmd.Output() // exit code 1 = no matches, not an error
	// Parse each line: "url.<base>.insteadof <value>"
	// Look specifically for entries that rewrite https://github.com URLs to an
	// SSH base (key contains git@github.com), avoiding false positives from
	// same-protocol or reverse (SSH→HTTPS) rewrites.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(parts[0])     // url.<base>.insteadof
		value := strings.TrimSpace(parts[1]) // the insteadOf value (URL prefix to match)
		if strings.Contains(value, "https://github.com") && strings.Contains(key, "git@github.com") {
			return true
		}
	}
	return false
}

// claudeSettingsFile is the subset of a Claude Code settings.json this
// preflight cares about — just apiKeyHelper (R10-R12). Other keys are
// ignored, so a settings file with unrelated content still parses fine.
type claudeSettingsFile struct {
	APIKeyHelper string `json:"apiKeyHelper"`
}

// settingsLayer names one file in the Claude Code settings-resolution chain
// checked by findAPIKeyHelper/checkAPIKeyHelper (R11): a human-readable layer
// label plus the file path.
type settingsLayer struct {
	layer string
	path  string
}

// managedPolicySettingsPath returns the OS-specific managed-policy settings
// file path Claude Code itself resolves (R11), or "" on an unsupported
// runtime.GOOS. Fabrik has no supported Windows host today (darwin/linux
// only, per CLAUDE.md/the Plan stage's Constraints), so no third path is
// defined here.
func managedPolicySettingsPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "linux":
		return "/etc/claude-code/managed-settings.json"
	default:
		return ""
	}
}

// claudeUserConfigDir returns the directory Claude Code resolves user-level
// settings from: $CLAUDE_CONFIG_DIR if set (the same "Alternate Claude
// Profile" variable Fabrik already documents and pins subprocess invocations
// to), else ~/.claude. Empty if the home directory cannot be resolved.
func claudeUserConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// findAPIKeyHelper checks each layer in order and returns the first one
// whose settings file sets a non-empty apiKeyHelper (R10), plus any
// warnings collected along the way. A missing file is not an error —
// absence means the layer contributes nothing (R12). A present but
// unreadable or malformed file is collected into warnings and the scan
// continues rather than failing outright (R12) — only a present and
// parseable apiKeyHelper key is fatal.
func findAPIKeyHelper(layers []settingsLayer) (found *settingsLayer, warns []string) {
	for i := range layers {
		layer := layers[i]
		data, err := os.ReadFile(layer.path)
		if err != nil {
			if !os.IsNotExist(err) {
				warns = append(warns, fmt.Sprintf("could not read %s (%s layer): %v", layer.path, layer.layer, err))
			}
			continue
		}
		var settings claudeSettingsFile
		if err := json.Unmarshal(data, &settings); err != nil {
			warns = append(warns, fmt.Sprintf("could not parse %s (%s layer) as JSON: %v", layer.path, layer.layer, err))
			continue
		}
		if settings.APIKeyHelper != "" {
			return &layer, warns
		}
	}
	return nil, warns
}

// checkAPIKeyHelper is the R10-R12 startup preflight: it resolves the Claude
// Code settings chain (managed-policy, user, fabrikDir-project layers) and
// fails startup with a non-zero exit if apiKeyHelper is set anywhere in it.
// apiKeyHelper is a settings.json key, not an environment variable — a
// command Claude Code shells out to for credentials — so no amount of
// environment scrubbing (buildClaudeEnv, R2-R9) can prevent it from supplying
// an API key to a Fabrik-spawned invocation. It is refused outright rather
// than silently tolerated (see the issue's Non-goals and ADR-1346). This is
// the startup-time half of the check; runInvocationWithExtension performs the
// structurally identical per-invocation check against the worktree's own
// .claude/settings.json and settings.local.json (R13), which don't exist
// until a worktree is materialized.
func (e *Engine) checkAPIKeyHelper() error {
	var layers []settingsLayer
	if p := managedPolicySettingsPath(); p != "" {
		layers = append(layers, settingsLayer{layer: "managed-policy", path: p})
	}
	if dir := claudeUserConfigDir(); dir != "" {
		layers = append(layers,
			settingsLayer{layer: "user", path: filepath.Join(dir, "settings.json")},
			settingsLayer{layer: "user", path: filepath.Join(dir, "settings.local.json")},
		)
	}
	layers = append(layers,
		settingsLayer{layer: "project", path: filepath.Join(e.fabrikDir, ".claude", "settings.json")},
		settingsLayer{layer: "project", path: filepath.Join(e.fabrikDir, ".claude", "settings.local.json")},
	)

	found, warns := findAPIKeyHelper(layers)
	for _, w := range warns {
		e.logf(0, "startup", "warning: %s\n", w)
	}
	if found != nil {
		return fmt.Errorf("apiKeyHelper is set in %s (%s layer) — Fabrik refuses to run with apiKeyHelper configured, "+
			"since it can supply API credentials outside Fabrik's control regardless of environment scrubbing; "+
			"remove it and restart. See docs/USER_GUIDE.md", found.path, found.layer)
	}
	return nil
}

// ghesVersionFloorMajor/ghesVersionFloorMinor are the minimum supported GHES
// major.minor version (FR-7). 3.19 is the version Fabrik's capability probe
// (scripts/ghes-probe.sh) was run against and confirmed full API parity for
// everything Fabrik uses — see adrs/1391-ghes-support.md. Patch version is
// deliberately ignored: the floor is stated as "3.19", not a specific patch.
const (
	ghesVersionFloorMajor = 3
	ghesVersionFloorMinor = 19
)

// checkGHESVersionFloor is the FR-7 startup preflight: when a GHES host is
// configured, it fetches /meta's installed_version and fails startup loudly,
// naming both the detected and required versions, if the instance is below
// the verified-supported floor. A no-op when no GHES host is configured —
// github.com's version is not in question (FR-2's no-regression
// requirement). Delegates to checkGHESVersionFloorAgainst so the comparison
// logic unit-tests directly against a *gh.Client + httptest, without needing
// an Engine or the engine-level GitHubClient mocks.
func (e *Engine) checkGHESVersionFloor() error {
	if e.cfg.GHESHost == "" {
		return nil
	}
	if e.hostClient == nil {
		// Defensive: New() always sets hostClient when GHESHost is configured.
		// A nil hostClient here (e.g. an unusual test construction) has no safe
		// version to check against, so skip rather than panic.
		return nil
	}
	return checkGHESVersionFloorAgainst(e.hostClient, e.cfg.GHESHost, func(format string, args ...any) {
		e.logf(0, "startup", format, args...)
	})
}

// checkGHESVersionFloorAgainst is the testable core of checkGHESVersionFloor.
// A /meta fetch error (network, auth) is a non-fatal warning, mirroring
// checkStageColumnAlignment's existing "log and skip" precedent for fetch
// failures — connectivity/auth problems already surface loudly elsewhere
// (the board fetch immediately after), and conflating them here would
// misreport a network blip as "your GHES is too old." An unparseable
// installed_version is likewise a non-fatal warning, not a hard failure.
func checkGHESVersionFloorAgainst(client *gh.Client, host string, logf func(format string, args ...any)) error {
	version, err := client.FetchInstalledVersion()
	if err != nil {
		logf("warning: could not verify GHES version for %s: %v\n", host, err)
		return nil
	}
	major, minor, ok := parseMajorMinor(version)
	if !ok {
		logf("warning: could not parse GHES installed_version %q from %s — skipping version floor check\n", version, host)
		return nil
	}
	if major < ghesVersionFloorMajor || (major == ghesVersionFloorMajor && minor < ghesVersionFloorMinor) {
		return fmt.Errorf("GHES instance %s reports version %s, below Fabrik's minimum supported version %d.%d — "+
			"upgrade the GHES instance or remove the ghes_host configuration. See docs/USER_GUIDE.md",
			host, version, ghesVersionFloorMajor, ghesVersionFloorMinor)
	}
	return nil
}

// parseMajorMinor extracts the major and minor integers from a version
// string like "3.19.8", ignoring any patch/suffix component. Returns
// ok=false if the string doesn't start with "<int>.<int>".
func parseMajorMinor(version string) (major, minor int, ok bool) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// logAnthropicAPIKeyOptIn is the testable core of the R9 startup notice: when
// FABRIK_ANTHROPIC_API_KEY is set, invocations are billed to the Anthropic
// API rather than a subscription. A no-op when inactive, so the unset case
// stays byte-for-byte silent. The value itself is never logged, in whole or
// in part — only the fact that the opt-in is active.
func logAnthropicAPIKeyOptIn(active bool, w io.Writer) {
	if !active {
		return
	}
	fmt.Fprintf(w, "[startup] notice: FABRIK_ANTHROPIC_API_KEY is set — Claude invocations will be billed to the "+
		"Anthropic API rather than a subscription. See docs/USER_GUIDE.md.\n")
}

// logAnthropicEnvPassthrough is the testable core of the R18 startup notice:
// when the FABRIK_ANTHROPIC_ENV_PASSTHROUGH allow-list is non-empty, it names
// which variables were passed through and warns that invocations may not be
// subscription-billed as a result. A no-op when names is empty, so the
// unset/empty case stays byte-for-byte silent. Values are never logged, in
// whole or in part — only the variable names.
func logAnthropicEnvPassthrough(names []string, w io.Writer) {
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "[startup] notice: FABRIK_ANTHROPIC_ENV_PASSTHROUGH passes through %s — Claude invocations may not "+
		"be subscription-billed as a result. See docs/USER_GUIDE.md.\n", strings.Join(names, ", "))
}
