package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResetOptions selects the form of the reset (scripts/e2e/reset.sh's flags).
type ResetOptions struct {
	// Worktrees also removes Fabrik's worktrees and bare clones from the bed
	// (destructive; refuses while a bed engine is running).
	Worktrees bool
}

// ResetConfig names what reset clears. Defaults come from the same env vars
// reset.sh read.
type ResetConfig struct {
	Alpha, Beta   string
	ProjectOwner  string
	ProjectNumber string
}

func (g *Gate) resetConfig() ResetConfig {
	return ResetConfig{
		Alpha:         orDefault(g.Getenv("FABRIK_TEST_REPO_ALPHA"), "handarbeit/fabrik-test-alpha"),
		Beta:          orDefault(g.Getenv("FABRIK_TEST_REPO_BETA"), "handarbeit/fabrik-test-beta"),
		ProjectOwner:  orDefault(g.Getenv("FABRIK_TEST_PROJECT_OWNER"), "handarbeit"),
		ProjectNumber: orDefault(g.Getenv("FABRIK_TEST_PROJECT_NUMBER"), "2"),
	}
}

// Reset is scripts/e2e/reset.sh: clear state from prior e2e runs to a clean
// slate, so the bed starts from a known-empty state (stale board items and
// leftover branches otherwise pollute the next run's merge-train snapshots).
//
// By default it closes every OPEN pull request (deleting its head branch) and
// every OPEN issue in alpha + beta, deletes leftover fabrik/* branches there,
// and removes EVERY item from the "Fabrik Test" project board (closed issues
// linger as board items otherwise). With Worktrees it also deletes
// <bed>/.fabrik/worktrees and .fabrik/repos — refusing if a bed engine is
// running — for when the bed itself is wedged.
//
// All GitHub calls go through `gh` scoped to the bed's own PAT via GH_TOKEN;
// that keeps the existing auth, pagination limits and retry behaviour (the
// `--limit 200/500` bounds and the 12-round board drain are part of the
// contract: a smaller bound would silently leave items behind).
func (g *Gate) Reset(ctx context.Context, opts ResetOptions) error {
	rc := g.resetConfig()
	bed := g.Cfg.TestBed

	if _, err := os.Stat(filepath.Join(bed, ".env")); err != nil {
		return exitErr(1, "test bed not found at %s (expected .env)", bed)
	}
	token := EnvFileValue(filepath.Join(bed, ".env"), "FABRIK_TOKEN")
	if token == "" {
		return exitErr(1, "FABRIK_TOKEN missing from %s/.env", bed)
	}
	gh := func(args ...string) (string, Result) {
		so, _, res := output(ctx, g.Exec, Cmd{Name: "gh", Args: args, Env: withEnv(g.Env, "GH_TOKEN="+token), Session: true, Grace: g.Cfg.KillGrace})
		return so, res
	}
	ghQuiet := func(args ...string) bool {
		var sink syncBuf
		res := g.Exec.Run(ctx, Cmd{Name: "gh", Args: args, Env: withEnv(g.Env, "GH_TOKEN="+token), Stdout: &sink, Stderr: &sink, Session: true, Grace: g.Cfg.KillGrace})
		return res.ExitCode == 0
	}
	lines := func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}

	closePRs := func(repo string) error {
		so, res := gh("pr", "list", "-R", repo, "--state", "open", "--limit", "200", "--json", "number", "--jq", ".[].number")
		if res.ExitCode != 0 {
			return exitErr(res.ExitCode, "reset: listing open PRs in %s failed", repo)
		}
		nums := lines(so)
		if len(nums) == 0 {
			g.outf("  %s: no open PRs\n", repo)
			return nil
		}
		g.outf("  %s: closing PRs: %s \n", repo, strings.Join(nums, " "))
		for _, n := range nums {
			// --delete-branch also removes the head branch; fall back to a plain
			// close if the branch is already gone.
			if !ghQuiet("pr", "close", n, "-R", repo, "--delete-branch") {
				ghQuiet("pr", "close", n, "-R", repo)
			}
		}
		return nil
	}
	closeIssues := func(repo string) error {
		so, res := gh("issue", "list", "-R", repo, "--state", "open", "--limit", "500", "--json", "number", "--jq", ".[].number")
		if res.ExitCode != 0 {
			return exitErr(res.ExitCode, "reset: listing open issues in %s failed", repo)
		}
		nums := lines(so)
		if len(nums) == 0 {
			g.outf("  %s: no open issues\n", repo)
			return nil
		}
		g.outf("  %s: closing issues: %s \n", repo, strings.Join(nums, " "))
		for _, n := range nums {
			ghQuiet("issue", "close", n, "-R", repo, "--reason", "completed", "--comment", "Closed by scripts/e2e/reset.sh")
		}
		return nil
	}
	deleteBranches := func(repo string) {
		so, _ := gh("api", "repos/"+repo+"/git/matching-refs/heads/fabrik/", "--jq", ".[].ref")
		refs := lines(so)
		if len(refs) == 0 {
			g.outf("  %s: no leftover fabrik/* branches\n", repo)
			return
		}
		g.outf("  %s: deleting %d fabrik/* branch(es)\n", repo, len(refs))
		for _, r := range refs {
			// matching-refs returns full refs (refs/heads/fabrik/...). The delete
			// endpoint is DELETE /repos/{repo}/git/refs/{ref} where {ref} is the ref
			// WITHOUT the leading "refs/" — strip only that.
			ghQuiet("api", "-X", "DELETE", "repos/"+repo+"/git/refs/"+strings.TrimPrefix(r, "refs/"))
		}
	}
	resolveProjectID := func() string { return g.resolveProjectNodeID(ctx, token, rc) }
	drainBoard := func() {
		pid := resolveProjectID()
		if pid == "" {
			g.errf("  could not resolve project %s/#%s — skipping board drain\n", rc.ProjectOwner, rc.ProjectNumber)
			return
		}
		// Loop: fetch a page of item IDs and delete them until the board is
		// empty. A few rounds absorb GitHub's eventual-consistency lag on deletes.
		for round := 1; round <= 12; round++ {
			so, _ := gh("api", "graphql", "-f", fmt.Sprintf(`query=query { node(id:"%s"){ ... on ProjectV2 { items(first:50){ nodes { id } } } } }`, pid), "--jq", ".data.node.items.nodes[].id")
			ids := lines(so)
			if len(ids) == 0 {
				break
			}
			for _, item := range ids {
				ghQuiet("api", "graphql", "-f", fmt.Sprintf(`query=mutation { deleteProjectV2Item(input:{projectId:"%s", itemId:"%s"}){ deletedItemId } }`, pid, item))
			}
		}
		so, res := gh("api", "graphql", "-f", fmt.Sprintf(`query=query { node(id:"%s"){ ... on ProjectV2 { items(first:1){ totalCount } } } }`, pid), "--jq", ".data.node.items.totalCount")
		remaining := strings.TrimSpace(so)
		if res.ExitCode != 0 || remaining == "" {
			remaining = "?"
		}
		g.outf("  project %s/#%s drained (remaining items: %s)\n", rc.ProjectOwner, rc.ProjectNumber, remaining)
	}

	g.outln("== closing open PRs (with branch delete) ==")
	for _, repo := range []string{rc.Alpha, rc.Beta} {
		if err := closePRs(repo); err != nil {
			return err
		}
	}
	g.outln("== closing open issues ==")
	for _, repo := range []string{rc.Alpha, rc.Beta} {
		if err := closeIssues(repo); err != nil {
			return err
		}
	}
	g.outln("== deleting leftover fabrik/* branches ==")
	deleteBranches(rc.Alpha)
	deleteBranches(rc.Beta)
	g.outln("== draining project board ==")
	drainBoard()

	if opts.Worktrees {
		g.outf("== removing Fabrik worktrees + bare clones from %s ==\n", bed)
		// Stop the Fabrik instance first if running — otherwise it keeps using
		// the deleted dirs and produces confusing errors.
		ps, _, res := output(ctx, g.Exec, Cmd{Name: "ps", Args: []string{"ax", "-o", "pid,command"}, Env: g.Env})
		if res.ExitCode != 0 {
			return exitErr(1, "reset: ps failed (exit %d)", res.ExitCode)
		}
		for _, line := range strings.Split(ps, "\n") {
			if strings.Contains(line, bed+"/fabrik") && !strings.Contains(line, "grep") {
				if f := strings.Fields(line); len(f) > 0 {
					return exitErr(1, "  fabrik-test pid %s is running — stop it before --worktrees", f[0])
				}
			}
		}
		if err := os.RemoveAll(filepath.Join(bed, ".fabrik", "worktrees")); err != nil {
			return exitErr(1, "reset: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(bed, ".fabrik", "repos")); err != nil {
			return exitErr(1, "reset: %v", err)
		}
		_ = os.Remove(filepath.Join(bed, ".fabrik", "fabrik.lock"))
		g.outln("  worktrees + bare clones removed; restart fabrik-test to re-init")
	}

	g.outln("done.")
	return nil
}

// resolveProjectNodeID is the board's ProjectV2 node ID ("" if it cannot be
// resolved): the org lookup first, then the user lookup. Shared by Reset's board
// drain and the lag probe, so both name the same board. The calls are scoped to
// token (the bed's own PAT) via GH_TOKEN.
func (g *Gate) resolveProjectNodeID(ctx context.Context, token string, rc ResetConfig) string {
	gh := func(args ...string) string {
		so, _, _ := output(ctx, g.Exec, Cmd{Name: "gh", Args: args, Env: withEnv(g.Env, "GH_TOKEN="+token), Session: true, Timeout: g.Cfg.GHAPITimeout, Grace: g.Cfg.KillGrace})
		return so
	}
	q := fmt.Sprintf(`query { organization(login:"%s"){ projectV2(number:%s){ id } } }`, rc.ProjectOwner, rc.ProjectNumber)
	if id := strings.TrimSpace(gh("api", "graphql", "-f", "query="+q, "--jq", ".data.organization.projectV2.id")); id != "" && id != "null" {
		return id
	}
	q = fmt.Sprintf(`query { user(login:"%s"){ projectV2(number:%s){ id } } }`, rc.ProjectOwner, rc.ProjectNumber)
	if id := strings.TrimSpace(gh("api", "graphql", "-f", "query="+q, "--jq", ".data.user.projectV2.id")); id != "null" {
		return id
	}
	return ""
}
