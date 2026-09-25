package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// This file gives HTTPS git a deterministic identity under GitHub App auth
// (#1846, superseding ADR-1756's blanket refusal). The engine injects a
// credential helper for https://github.com into its own process environment
// via git's GIT_CONFIG_COUNT/GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n>
// mechanism, so every engine git invocation (exec.Command("git", ...)
// inherits os.Environ) and every stage worker (buildClaudeEnv starts from
// os.Environ) authenticates as the App installation — never as whatever
// helper the host happens to have configured (GCM, `gh auth git-credential`
// resolving the operator's own login, osxkeychain).
//
// The helper reads the installation token from a file rather than carrying
// it in the environment: runAppGitTokenWriter rewrites that file whenever
// the refresh loop rotates the token, so a long-running worker's git picks
// up the fresh token on its next credential lookup. This closes ADR-1713's
// ~1h env-snapshot gap for git (it remains for the worker's GH_TOKEN, which
// `gh` reads from the environment only).

// appGitCredentialMarker tags the helper value this file injects, so a later
// exec of the engine (SIGHUP, self-upgrade — both syscall.Exec with the
// current environment) can find and strip the previous exec's entries rather
// than stacking duplicates, or leaving a bot-identity helper behind after a
// config change back to PAT mode or git_ssh.
const appGitCredentialMarker = "fabrik-app-git-credential"

// appGitCredentialKey is the URL-scoped helper key. Scoped to github.com so
// the helper never answers for any other host the worker's git talks to.
const appGitCredentialKey = "credential.https://github.com.helper"

// appGitTokenRewriteInterval is how often runAppGitTokenWriter compares the
// live installation token against the file. The refresh loop rotates the
// token well before expiry, so a lag of this size never serves an expired
// token.
const appGitTokenRewriteInterval = 30 * time.Second

// AppGitTokenPath is where the installation token for the git credential
// helper lives. .fabrik/state/ is gitignored.
func AppGitTokenPath(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "state", "github-app-git-token")
}

// shellSingleQuote quotes s for POSIX sh.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appGitCredentialHelper returns the `!`-prefixed shell helper git runs for
// credential lookups. git appends the operation ("get", "store", "erase") as
// an argument; only "get" answers. A missing or empty token file answers
// nothing, so git fails the operation rather than sending an empty password.
func appGitCredentialHelper(tokenPath string) string {
	return "!: " + appGitCredentialMarker + "; f() { test \"$1\" = get || exit 0; " +
		"t=$(cat " + shellSingleQuote(tokenPath) + " 2>/dev/null) || exit 0; test -n \"$t\" || exit 0; " +
		"echo username=x-access-token; printf 'password=%s\\n' \"$t\"; }; f"
}

// gitConfigEnvEdit computes the environment changes that replace any
// previously injected app-git-credential entries in environ's
// GIT_CONFIG_COUNT list with add (which may be empty, meaning strip only).
// Entries belonging to anything else are preserved in order. A stripped
// helper entry also takes its immediately preceding reset entry (same key,
// empty value) with it, since installAppGitCredentialEnv always writes the
// two adjacently.
//
// Returns the keys to unset and the KEY=VALUE pairs to set, and changed=false
// when environ needs no change at all (the common PAT/SSH case with nothing
// to strip). A GIT_CONFIG_COUNT git itself would reject (non-numeric,
// negative) is left untouched and reported as an error.
func gitConfigEnvEdit(environ []string, add [][2]string) (unset []string, set []string, changed bool, err error) {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	count := 0
	if raw, ok := env["GIT_CONFIG_COUNT"]; ok && raw != "" {
		count, err = strconv.Atoi(raw)
		if err != nil || count < 0 {
			return nil, nil, false, fmt.Errorf("GIT_CONFIG_COUNT=%q in the environment is not a non-negative integer", raw)
		}
	}
	existing := make([][2]string, 0, count)
	for i := 0; i < count; i++ {
		existing = append(existing, [2]string{env["GIT_CONFIG_KEY_"+strconv.Itoa(i)], env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]})
	}
	kept := make([][2]string, 0, len(existing))
	for _, p := range existing {
		if strings.Contains(p[1], appGitCredentialMarker) {
			if n := len(kept); n > 0 && kept[n-1][0] == p[0] && kept[n-1][1] == "" {
				kept = kept[:n-1]
			}
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == len(existing) && len(add) == 0 {
		return nil, nil, false, nil
	}
	final := append(kept, add...)
	for i := len(final); i < count; i++ {
		unset = append(unset, "GIT_CONFIG_KEY_"+strconv.Itoa(i), "GIT_CONFIG_VALUE_"+strconv.Itoa(i))
	}
	if len(final) == 0 {
		unset = append(unset, "GIT_CONFIG_COUNT")
		return unset, nil, true, nil
	}
	for i, p := range final {
		set = append(set, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+p[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+p[1])
	}
	set = append(set, "GIT_CONFIG_COUNT="+strconv.Itoa(len(final)))
	return unset, set, true, nil
}

// applyGitConfigEnv strips any previous app-git-credential entries from this
// process's environment and, when tokenPath is non-empty, injects a fresh
// pair: a reset (empty helper, which clears every helper configured for
// github.com by the host's own git config) followed by the file-reading
// helper.
func applyGitConfigEnv(tokenPath string) error {
	var add [][2]string
	if tokenPath != "" {
		add = [][2]string{{appGitCredentialKey, ""}, {appGitCredentialKey, appGitCredentialHelper(tokenPath)}}
	}
	unset, set, changed, err := gitConfigEnvEdit(os.Environ(), add)
	if err != nil || !changed {
		return err
	}
	for _, k := range unset {
		if err := os.Unsetenv(k); err != nil {
			return fmt.Errorf("unsetting %s: %w", k, err)
		}
	}
	for _, kv := range set {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("setting %s: %w", k, err)
		}
	}
	return nil
}

// writeAppGitToken atomically writes token to path with 0600 permissions.
func writeAppGitToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".github-app-git-token-*")
	if err != nil {
		return fmt.Errorf("creating temp token file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp token file: %w", err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming token file into place: %w", err)
	}
	return nil
}

// runAppGitTokenWriter keeps path in step with tokenFn until ctx is done,
// then removes the file. The caller has already written the initial token.
// A failed rewrite is logged and retried on the next tick: the previous
// token stays valid until its own expiry, well past one interval.
func (e *Engine) runAppGitTokenWriter(ctx context.Context, tokenFn func() string, path, written string) {
	ticker := time.NewTicker(appGitTokenRewriteInterval)
	defer ticker.Stop()
	defer func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			e.logf(0, "github-app", "warning: removing git token file %s: %v", path, err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tok := tokenFn()
			if tok == "" || tok == written {
				continue
			}
			if err := writeAppGitToken(path, tok); err != nil {
				e.logf(0, "github-app", "warning: refreshing git token file: %v", err)
				continue
			}
			written = tok
		}
	}
}

// setUpAppGitCredential runs at startup (Run()), after the refresh loops
// have started. It always strips a previous exec's injected entries; it
// injects fresh ones only under App auth with HTTPS git (httpsGit — neither
// git_ssh nor a global HTTPS→SSH insteadOf rewrite). Under that combination
// setUpGitHubAppAuth has already failed startup unless the installation
// grants contents:write, so the token the helper serves can push.
func (e *Engine) setUpAppGitCredential(ctx context.Context, httpsGit bool) error {
	if e.ghAppAuth == nil || !httpsGit || e.hostClient == nil {
		// hostClient is nil only for NewWithDeps-built test engines.
		return applyGitConfigEnv("")
	}
	tokenFn := e.hostClient.Token
	tok := tokenFn()
	if tok == "" {
		return fmt.Errorf("GitHub App auth: no installation token available for HTTPS git credentials")
	}
	path := AppGitTokenPath(e.fabrikDir)
	if err := writeAppGitToken(path, tok); err != nil {
		return fmt.Errorf("GitHub App auth: writing HTTPS git token file: %w", err)
	}
	if err := applyGitConfigEnv(path); err != nil {
		return fmt.Errorf("GitHub App auth: injecting HTTPS git credential helper: %w", err)
	}
	go e.runAppGitTokenWriter(ctx, tokenFn, path, tok)
	fmt.Printf("[startup] HTTPS git authenticates as the GitHub App installation (credential helper for https://github.com)\n")
	return nil
}
