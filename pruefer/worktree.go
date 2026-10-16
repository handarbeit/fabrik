package pruefer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// given PR's head commit, for Claude to inspect during review. It fetches
// refs/pull/<N>/head and, only when GitHub never created that ref, falls back
// to fetching headSHA (the commit the review was selected on) from the same
// base-repository URL. Either way the checked-out HEAD must equal headSHA
// (see cloneRefAt). Returns the clone directory; callers must call the
// returned cleanup function when done (safe to call multiple times, including
// on error paths).
//
// No bare-clone cache: each review does a single-use, depth-1 fetch into a
// fresh temp dir. Fabrik's worktrees persist for days across many stage
// runs on the same issue; Pruefer's reviews are single-shot, so a cache
// would add lifecycle-management complexity for comparatively little
// benefit at V1 scale (see adrs/1113-pruefer-v1-architecture.md and
// adrs/2331-pruefer-head-sha-fetch-fallback.md).
func CloneForReview(ctx context.Context, owner, repo, token string, prNumber int, headSHA string) (dir string, cleanup func(), err error) {
	return cloneRefAt(ctx, cloneURL(owner, repo, token), token, fmt.Sprintf("refs/pull/%d/head", prNumber), headSHA, prNumber)
}

// Test seams for the git invocations made by cloneRefAt. Production code
// never reassigns them.
var (
	runGitFn    = runGit
	gitOutputFn = gitOutput
)

// Route names logged once per clone attempt.
const (
	routePullRef     = "pull ref"
	routeHeadSHAFall = "head SHA fallback"
)

// cloneRef is cloneRefAt without a selected head SHA: no fallback and no
// head verification. Kept for callers (and tests) that only have a ref.
func cloneRef(ctx context.Context, remoteURL, token, ref string, prNumber int) (dir string, cleanup func(), err error) {
	return cloneRefAt(ctx, remoteURL, token, ref, "", prNumber)
}

// cloneRefAt does the init/fetch/checkout sequence against an explicit
// remoteURL and ref. Split out from CloneForReview so tests can exercise it
// against a local file-based git repo instead of github.com.
//
// The ref fetch is always tried first, exactly as before. Only when it fails
// with git's "couldn't find remote ref" and headSHA is a full commit SHA is
// headSHA fetched directly from the same remoteURL (GitHub does not always
// create refs/pull/<N>/head). A fork PR therefore never contacts the fork:
// the fallback succeeds only if the base repository serves the commit.
//
// When headSHA is a full SHA the checked-out HEAD must equal it, on either
// route, or the attempt fails: a review is never produced from a commit other
// than the one selected. An empty or malformed headSHA skips both the
// fallback and the verification.
func cloneRefAt(ctx context.Context, remoteURL, token, ref, headSHA string, prNumber int) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("", fmt.Sprintf("pruefer-pr-%d-*", prNumber))
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp clone dir: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }

	if err := runGitFn(ctx, dir, token, "init", "-q"); err != nil {
		cleanup()
		return "", func() {}, err
	}
	route := routePullRef
	if err := runGitFn(ctx, dir, token, "fetch", "--depth", "1", "-q", remoteURL, ref); err != nil {
		primaryErr := fmt.Errorf("fetching %s: %w", ref, err)
		if !isFullSHA(headSHA) || !isMissingRefErr(err) {
			cleanup()
			return "", func() {}, primaryErr
		}
		route = routeHeadSHAFall
		if err := runGitFn(ctx, dir, token, "fetch", "--depth", "1", "-q", remoteURL, headSHA); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("head SHA fallback fetch of %s failed: %w (pull ref fetch: %v)", headSHA, err, primaryErr)
		}
	}
	logf(prNumber, "clone", "route=%s\n", route)
	if err := runGitFn(ctx, dir, token, "checkout", "-q", "FETCH_HEAD"); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("checking out FETCH_HEAD: %w", err)
	}
	// FETCH_HEAD records the fetched URL, and the clone is readable by the
	// reviewer (#2192): drop it so a credentialed remote URL can never be read
	// from inside the review scope. Best effort; checkout no longer needs it.
	_ = os.Remove(filepath.Join(dir, ".git", "FETCH_HEAD"))
	if isFullSHA(headSHA) {
		got, err := gitOutputFn(ctx, dir, token, "rev-parse", "HEAD")
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("verifying checked-out HEAD: %w", err)
		}
		if !strings.EqualFold(got, headSHA) {
			cleanup()
			return "", func() {}, fmt.Errorf("checked-out HEAD %s (via %s) is not the selected head %s", got, route, headSHA)
		}
	}
	return dir, cleanup, nil
}

// isMissingRefErr reports whether a git fetch error is git's "couldn't find
// remote ref" condition. runGit has no typed errors (exit 128 covers many
// fatal conditions), so this is a narrow substring match; a localized git
// simply does not trigger the fallback, which is today's behaviour.
func isMissingRefErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "couldn't find remote ref")
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

// gitOutput runs git like runGit but returns its trimmed stdout. Errors are
// redacted exactly as runGit's are.
func gitOutput(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stdout.String() + stderr.String()
		displayArgs := args
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
			displayArgs = redactArgs(args, token)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(displayArgs, " "), err, strings.TrimSpace(msg))
	}
	return strings.TrimSpace(stdout.String()), nil
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
