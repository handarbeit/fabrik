package pruefer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// cloneURL returns the HTTPS clone URL Pruefer uses to fetch a PR's head
// commit, with the installation token embedded as HTTP basic auth.
// "x-access-token" is GitHub's documented username convention for App
// installation tokens. Safe here because each clone is single-use and
// discarded (see CloneForReview) — there is no persisted git config for the
// token to leak from afterward, and runGit redacts the token from any
// returned error text.
func cloneURL(owner, repo, token string) string {
	return fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", token, owner, repo)
}

// CloneForReview creates an ephemeral shallow clone of owner/repo at the
// given PR's head commit (refs/pull/<N>/head), for Claude to inspect during
// review. Returns the clone directory; callers must call the returned
// cleanup function when done (safe to call multiple times, including on
// error paths).
//
// No bare-clone cache: each review does a single-use, depth-1 fetch into a
// fresh temp dir. Fabrik's worktrees persist for days across many stage
// runs on the same issue; Pruefer's reviews are single-shot, so a cache
// would add lifecycle-management complexity for comparatively little
// benefit at V1 scale (see adrs/1113-pruefer-v1-architecture.md).
func CloneForReview(ctx context.Context, owner, repo, token string, prNumber int) (dir string, cleanup func(), err error) {
	return cloneRef(ctx, cloneURL(owner, repo, token), token, fmt.Sprintf("refs/pull/%d/head", prNumber), prNumber)
}

// cloneRef does the actual init/fetch/checkout sequence against an explicit
// remoteURL and ref. Split out from CloneForReview so tests can exercise it
// against a local file-based git repo instead of github.com.
func cloneRef(ctx context.Context, remoteURL, token, ref string, prNumber int) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("", fmt.Sprintf("pruefer-pr-%d-*", prNumber))
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp clone dir: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }

	if err := runGit(ctx, dir, token, "init", "-q"); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := runGit(ctx, dir, token, "fetch", "--depth", "1", "-q", remoteURL, ref); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("fetching %s: %w", ref, err)
	}
	if err := runGit(ctx, dir, token, "checkout", "-q", "FETCH_HEAD"); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("checking out FETCH_HEAD: %w", err)
	}
	return dir, cleanup, nil
}

// runGit runs git in dir with a clean, non-interactive environment
// (GIT_TERMINAL_PROMPT=0, so a misconfigured/expired token fails fast
// instead of hanging on a credential prompt), redacting token from any
// error output so it never reaches logs.
func runGit(ctx context.Context, dir, token string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		displayArgs := args
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
			displayArgs = redactArgs(args, token)
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(displayArgs, " "), err, strings.TrimSpace(msg))
	}
	return nil
}

// redactArgs returns a copy of args with any argument containing token
// replaced by a masked version, so an error message built from the argv
// never echoes the installation token.
func redactArgs(args []string, token string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if token != "" && strings.Contains(a, token) {
			out[i] = strings.ReplaceAll(a, token, "***")
		} else {
			out[i] = a
		}
	}
	return out
}
