package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
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
	// Holding stages are excluded when merge_train is off — they only require a
	// board column when merge_train is on (the operator must add the Queued column).
	var checkStages []*stageNameOrder
	for _, s := range e.cfg.Stages {
		if s.CleanupWorktree {
			continue
		}
		if s.HoldingStage && e.cfg.MergeTrain != "on" {
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

	// Find extra board columns: columns that no configured stage backs and that
	// are not a well-known unmanaged column. Unlike the missing-column check
	// above, this recognizes EVERY configured stage — including cleanup stages
	// (e.g. Done) and holding stages (e.g. Queued) — because those columns are
	// legitimately backed by a stage even though they don't require one. The
	// "Backlog" entry column is intentionally unmanaged (Fabrik ignores it), so
	// it is never reported as extra. A genuine typo'd column matches neither and
	// is still surfaced.
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

	if len(missing) == 0 {
		return nil
	}

	// Build the list of all board column names for the error report.
	allCols := make([]string, 0, len(sf.Options))
	for colName := range sf.Options {
		allCols = append(allCols, colName)
	}
	sort.Strings(allCols)

	fmt.Fprintf(os.Stderr, "Fabrik startup check failed: stage/board column mismatch\n\n")
	fmt.Fprintf(os.Stderr, "Configured stages not found on board:\n")
	for _, s := range missing {
		fmt.Fprintf(os.Stderr, "  - %s (order %d)\n", s.name, s.order)
	}
	fmt.Fprintf(os.Stderr, "\nBoard columns found:\n  %s\n\n", strings.Join(allCols, ", "))
	fmt.Fprintf(os.Stderr, "Fix: add the missing columns to your GitHub Project board, or update\n")
	fmt.Fprintf(os.Stderr, ".fabrik/stages/ to match your board column names (case-sensitive).\n")
	for _, s := range missing {
		if s.holdingStage {
			fmt.Fprintf(os.Stderr, "\nNote: %q is a holding stage required by merge_train: on.\n", s.name)
			fmt.Fprintf(os.Stderr, "Add a `%s` column to your GitHub Project board between `Validate` and `Done`,\n", s.name)
			fmt.Fprintf(os.Stderr, "then restart. See docs/state-machine.md for setup steps.\n")
			fmt.Fprintf(os.Stderr, "If you copied queued.yaml from a new Fabrik installation, ensure the column\n")
			fmt.Fprintf(os.Stderr, "name on the board matches the 'name' field in the YAML (case-sensitive).\n")
		}
	}

	return fmt.Errorf("startup check failed: stage/board column mismatch")
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

// checkAllowAutoMerge queries the GitHub API for the allow_auto_merge setting on
// owner/repo and prints a WARNING if it is disabled. Non-fatal: API errors are
// logged at warn level and processing continues. The check fires at most once per
// repo per process run (guarded by checkedAutoMergeRepos).
func (e *Engine) checkAllowAutoMerge(owner, repo string) {
	key := owner + "/" + repo
	e.mu.Lock()
	already := e.checkedAutoMergeRepos[key]
	e.checkedAutoMergeRepos[key] = true
	e.mu.Unlock()
	if already {
		return
	}
	enabled, err := e.client.FetchAllowAutoMerge(owner, repo)
	if err != nil {
		e.logf(0, "warn", "could not check allow_auto_merge for %s: %v\n", key, err)
		return
	}
	if !enabled {
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
// Returns true when such HTTPS→SSH rewriting is active. Prints an
// informational notice when active — git applies the rewriting transparently,
// so Fabrik's HTTPS clone URLs will automatically use SSH.
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
			fmt.Printf("[startup] note: git URL rewriting for github.com is active (HTTPS → SSH); Fabrik's HTTPS clone URLs will be transparently redirected to SSH via your git config.\n")
			return true
		}
	}
	return false
}
