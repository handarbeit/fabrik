package simgh

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// This file is the git-backed half of the model. Everything git can genuinely
// answer is computed here by running the real git binary against a real bare
// repository: mergeability comes from an actual trial merge, MergePR writes an
// actual merge commit, commits-behind comes from rev-list. Nothing in this
// file consults a declared boolean.
//
// Every exported entry point below acquires the repo's gitMu for the duration
// of its subprocess calls. Per the package doc's invariant, no function here
// may acquire Sim.mu.

// simGitAuthor is the identity every commit the model writes is attributed to.
const (
	simGitName  = "simgh"
	simGitEmail = "simgh@example.invalid"
)

// errNothingToMerge reports that a merge would produce no commit because the
// head is already contained in the base. Callers wrap it in the sentinel the
// engine tests against; see tryMerge and MergePR.
var errNothingToMerge = errors.New("head is already contained in base, so there is no merge commit to write")

// gitEnv returns a non-interactive environment for git subprocesses, mirroring
// engine/worktree.go's nonInteractiveGitEnv. The sim never talks to a network
// remote, but a stray credential or SSH prompt would hang a test rather than
// fail it.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME="+simGitName,
		"GIT_AUTHOR_EMAIL="+simGitEmail,
		"GIT_COMMITTER_NAME="+simGitName,
		"GIT_COMMITTER_EMAIL="+simGitEmail,
	)
}

// runGit executes git in dir and returns trimmed stdout. Caller must hold the
// repo's gitMu.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// runGitAllowFail executes git and reports the exit status without treating a
// non-zero exit as an error. Used where a non-zero exit is a meaningful answer
// rather than a failure — a conflicting merge, most importantly.
func runGitAllowFail(dir string, args ...string) (out string, ok bool, err error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	combined, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(combined)), true, nil
	}
	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		return strings.TrimSpace(string(combined)), false, nil
	}
	return strings.TrimSpace(string(combined)), false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// asExitError reports whether err is an *exec.ExitError, assigning it to target.
func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// initBare creates the backing bare repository and gives it a root commit on
// defaultBranch.
//
// The root commit is written with plumbing (mktree + commit-tree + update-ref)
// rather than through a worktree, because `git worktree add` needs a commit to
// check out and a fresh bare repo has none. Every later operation goes through
// a real worktree.
func initBare(dir, defaultBranch string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("simgh: creating repo dir: %w", err)
	}
	if _, err := runGit(filepath.Dir(dir), "init", "--bare", "-b", defaultBranch, filepath.Base(dir)); err != nil {
		return err
	}
	// Empty tree, then a root commit pointing at it.
	tree, err := runGitStdin(dir, "", "mktree")
	if err != nil {
		return err
	}
	commit, err := runGitStdin(dir, "root commit\n", "commit-tree", tree)
	if err != nil {
		return err
	}
	if _, err := runGit(dir, "update-ref", "refs/heads/"+defaultBranch, commit); err != nil {
		return err
	}
	return nil
}

// runGitStdin executes git with the given stdin, returning trimmed stdout only
// (not stderr), since callers here consume object IDs.
func runGitStdin(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// withWorktree checks ref out into a throwaway detached worktree, runs fn
// against it, and removes it. Trial merges must not share a working tree —
// git's index and worktree administrative files are not concurrency-safe, and
// a shared checkout would also let one operation's leftover conflict state
// leak into the next.
//
// The throwaway lives under the repo's worktreeRoot, inside Sim.baseDir, not in
// the OS temp dir. The deferred cleanup below removes it on every ordinary
// path, but a process killed mid-merge (OOM, SIGKILL, a CI timeout) cannot run
// deferred code — and an orphan under the OS temp dir would outlive both the
// sandbox this package's doc comment promises and t.TempDir()'s cleanup.
// Keeping it inside baseDir makes the test framework the backstop.
//
// Caller must hold the repo's gitMu.
func (r *repoState) withWorktree(ref string, fn func(wt string) error) (err error) {
	if err := os.MkdirAll(r.worktreeRoot, 0o755); err != nil {
		return fmt.Errorf("simgh: creating worktree root: %w", err)
	}
	tmp, err := os.MkdirTemp(r.worktreeRoot, "wt-")
	if err != nil {
		return fmt.Errorf("simgh: creating worktree temp dir: %w", err)
	}
	// git worktree add insists on creating the directory itself.
	wt := filepath.Join(tmp, "wt")

	if _, err := runGit(r.bareDir, "worktree", "add", "--detach", wt, ref); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	defer func() {
		// Prune unconditionally: the worktree removal can legitimately fail if
		// fn left the tree in a conflicted state, and a stale administrative
		// entry would break the next operation on this repo.
		_, _, _ = runGitAllowFail(r.bareDir, "worktree", "remove", "--force", wt)
		os.RemoveAll(tmp)
		_, _, _ = runGitAllowFail(r.bareDir, "worktree", "prune")
	}()

	return fn(wt)
}

// resolveRef returns the commit SHA a ref points at. Caller must hold gitMu.
func (r *repoState) resolveRef(ref string) (string, error) {
	return runGit(r.bareDir, "rev-parse", "--verify", ref+"^{commit}")
}

// branchExists reports whether a branch is present in the backing repo.
// Caller must hold gitMu.
func (r *repoState) branchExists(branch string) bool {
	_, ok, err := runGitAllowFail(r.bareDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil && ok
}

// tryMerge performs a real merge of head into base in a throwaway worktree.
//
// It is the single entry point for both uses: a read-only mergeability probe
// (commit=false, result discarded) and an actual merge (commit=true, which
// fast-forwards base's ref to the new merge commit). Sharing one
// implementation is deliberate — if the probe and the merge could disagree,
// the model would be able to report a PR mergeable and then fail to merge it,
// which is precisely the class of bug this layer exists to catch.
//
// Returns conflict=true when the merge genuinely cannot be performed. Caller
// must hold gitMu.
func (r *repoState) tryMerge(base, head string, commit bool, msg string) (sha string, conflict bool, err error) {
	if !r.branchExists(base) {
		return "", false, fmt.Errorf("simgh: base branch %q does not exist in %s/%s", base, r.owner, r.repo)
	}
	if !r.branchExists(head) {
		return "", false, fmt.Errorf("simgh: head branch %q does not exist in %s/%s", head, r.owner, r.repo)
	}

	headSHA, err := r.resolveRef("refs/heads/" + head)
	if err != nil {
		return "", false, err
	}
	baseSHA, err := r.resolveRef("refs/heads/" + base)
	if err != nil {
		return "", false, err
	}

	err = r.withWorktree("refs/heads/"+base, func(wt string) error {
		// --no-ff so a fast-forwardable head still produces a real merge
		// commit, matching GitHub's default "Create a merge commit" strategy.
		out, ok, runErr := runGitAllowFail(wt, "merge", "--no-ff", "-m", msg, headSHA)
		if runErr != nil {
			return runErr
		}
		if !ok {
			// Distinguish a genuine content conflict from any other failure.
			// "Already up to date" exits zero, so reaching here with no
			// conflicted paths means something else went wrong.
			conflicted, _, lsErr := runGitAllowFail(wt, "diff", "--name-only", "--diff-filter=U")
			if lsErr == nil && strings.TrimSpace(conflicted) != "" {
				conflict = true
				return nil
			}
			return fmt.Errorf("simgh: merging %s into %s: %s", head, base, out)
		}
		newSHA, err := runGit(wt, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		sha = newSHA
		return nil
	})
	if err != nil {
		return "", false, err
	}
	if conflict {
		return "", true, nil
	}

	// `git merge --no-ff` prints "Already up to date." and exits *zero* without
	// creating a commit when head is already contained in base's history — the
	// state a second merge of the same branch leaves behind. The tip is then
	// unchanged, so publishing it would flip merged=true while writing no merge
	// commit at all, silently breaking the invariant this package and
	// FIDELITY.md both state unconditionally.
	//
	// The probe (commit=false) is untouched: there is genuinely no conflict, so
	// it still reports mergeable. Only the merge itself refuses, which is also
	// how GitHub behaves — a PR with nothing left to merge is not a conflict,
	// but its merge endpoint will not manufacture an empty merge commit.
	if sha == baseSHA {
		if commit {
			return "", false, fmt.Errorf("simgh: merging %s into %s in %s/%s: %w",
				head, base, r.owner, r.repo, errNothingToMerge)
		}
		return sha, false, nil
	}

	if commit {
		// The merge commit already exists in the shared object store; publish
		// it by moving the base branch.
		if _, err := runGit(r.bareDir, "update-ref", "refs/heads/"+base, sha); err != nil {
			return "", false, err
		}
	}
	return sha, false, nil
}

// commitsBehind returns how many commits are on base but not on head — the
// same quantity GitHub's compare endpoint reports as behind_by, which is what
// production's FetchCommitsBehind reads. Caller must hold gitMu.
func (r *repoState) commitsBehind(base, head string) (int, error) {
	baseRef, headRef := "refs/heads/"+base, "refs/heads/"+head
	if !r.branchExists(base) {
		return 0, fmt.Errorf("simgh: base branch %q does not exist in %s/%s", base, r.owner, r.repo)
	}
	if !r.branchExists(head) {
		return 0, fmt.Errorf("simgh: head branch %q does not exist in %s/%s", head, r.owner, r.repo)
	}
	out, err := runGit(r.bareDir, "rev-list", "--count", headRef+".."+baseRef)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("simgh: parsing rev-list count %q: %w", out, err)
	}
	return n, nil
}

// commitFiles writes files into branch and commits them, creating the branch
// from fromBranch if it does not yet exist. Returns the new commit SHA.
// Caller must hold gitMu.
func (r *repoState) commitFiles(branch, fromBranch string, files map[string]string, msg string) (string, error) {
	// Validated up front, before any git work, so a bad name refuses the whole
	// commit rather than leaving some files already written. Sorted so the
	// error names the same key on every run despite map ordering.
	names := sortedKeys(files)
	for _, name := range names {
		if err := validateRepoRelPath(name); err != nil {
			return "", err
		}
	}

	startRef := "refs/heads/" + branch
	if !r.branchExists(branch) {
		if fromBranch == "" {
			fromBranch = r.defaultBranch
		}
		if !r.branchExists(fromBranch) {
			return "", fmt.Errorf("simgh: cannot branch %q from missing %q", branch, fromBranch)
		}
		startRef = "refs/heads/" + fromBranch
	}

	var sha string
	err := r.withWorktree(startRef, func(wt string) error {
		for _, name := range names {
			content := files[name]
			p := filepath.Join(wt, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return fmt.Errorf("simgh: creating dir for %s: %w", name, err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return fmt.Errorf("simgh: writing %s: %w", name, err)
			}
		}
		if _, err := runGit(wt, "add", "-A"); err != nil {
			return err
		}
		if _, err := runGit(wt, "commit", "--allow-empty", "-m", msg); err != nil {
			return err
		}
		out, err := runGit(wt, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		sha = out
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := runGit(r.bareDir, "update-ref", "refs/heads/"+branch, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// createBranch points a *new* branch at fromBranch's tip. Caller must hold
// gitMu.
//
// An existing branch is refused rather than repointed. GitHub's create-ref
// endpoint behaves the same way (422 "Reference already exists") — moving a
// branch is a separate, explicit operation there, not something ref creation
// does silently. The refusal matters more here than the API parity: the raw
// `update-ref` this wraps would happily discard whatever the branch already
// pointed at, so SeedBranch after a SeedCommit on the same branch would throw
// the seeded commits away and still report success. Growing a branch is
// commitFiles' job, and it already forks-or-appends deliberately.
func (r *repoState) createBranch(branch, fromBranch string) error {
	if r.branchExists(branch) {
		return fmt.Errorf("simgh: branch %q already exists in %s/%s; "+
			"use SeedCommit to add commits to an existing branch", branch, r.owner, r.repo)
	}
	if fromBranch == "" {
		fromBranch = r.defaultBranch
	}
	sha, err := r.resolveRef("refs/heads/" + fromBranch)
	if err != nil {
		return err
	}
	_, err = runGit(r.bareDir, "update-ref", "refs/heads/"+branch, sha)
	return err
}

// headSHA resolves a branch tip, returning "" when the branch does not exist —
// a PR whose head branch was deleted is a real state, not an error.
//
// That empty string models exactly one condition, so a resolveRef failure is
// *not* folded into it: branchExists confirmed the ref a moment earlier, so a
// failure here is a genuine git error (a corrupt object, a ref pointing at a
// non-commit) and must surface as one rather than be miscategorised as
// legitimate simulated GitHub state. Caller must hold gitMu.
func (r *repoState) headSHA(branch string) (string, error) {
	if !r.branchExists(branch) {
		return "", nil
	}
	return r.resolveRef("refs/heads/" + branch)
}
