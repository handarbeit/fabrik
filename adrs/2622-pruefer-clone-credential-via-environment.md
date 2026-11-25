# ADR 2622: Pruefer's review clone authenticates through the environment, not the URL

## Status

Accepted (#2622). Amends ADR 1113 decision 3, whose sentence "The clone URL
embeds the installation token as HTTP basic auth" no longer holds. Cites
ADR 2192 decision 9 (`FETCH_HEAD` removal, retained), ADR 2331 (head-SHA
fallback, same credential) and ADR 1846 (the engine's App git credential,
reference only).

## Context

`cloneURL` built `https://x-access-token:<token>@github.com/<owner>/<repo>.git`
and `cloneRefAt` passed it as an argument to `git fetch`, including the
head-SHA fallback. For the duration of each fetch any local process that can
list processes could read the installation token from the command line, as
could every git subprocess spawned from it. The token is short-lived (~1 h)
and not a PAT, and API calls already use an `Authorization: Bearer` header, so
the only gap was argv exposure on a shared host.

## Decision

1. **The remote URL carries no userinfo.** `cloneURL(owner, repo)` returns
   `https://github.com/<owner>/<repo>.git`.
2. **The credential travels in the environment of the `git fetch` process
   only.** `gitCredentialEnv` (`pruefer/worktree.go`) appends to the
   `exec.Cmd`'s environment `GIT_CONFIG_COUNT=n+1`,
   `GIT_CONFIG_KEY_n=http.https://<host>/.extraHeader` and
   `GIT_CONFIG_VALUE_n=Authorization: Basic base64("x-access-token:<token>")`.
   It is Basic, not Bearer: Bearer is for the REST API, Basic with the
   documented `x-access-token` user is what git's smart-HTTP transport
   accepts from an App token (the same form `actions/checkout` uses). The key
   is scoped to the remote's origin (derived from the URL, so the port is part
   of it), so the header is sent nowhere else.
3. **Fetch only, https only.** `init`, `checkout` and `rev-parse` never use the
   network and get nothing. A local-path or `file://` remote, a cleartext
   `http://` remote and an empty token get nothing either, so the credential
   never goes over cleartext HTTP and the existing local-fixture tests are
   unaffected.
4. **Existing config is preserved.** If the daemon's environment already has
   `GIT_CONFIG_COUNT`, the new entry takes the next index and the single
   `GIT_CONFIG_COUNT` is rewritten (Go's `exec` keeps the last duplicate key,
   so a blind append would drop either the operator's config or our header). A
   missing, negative or malformed count counts as zero. The input slice is not
   mutated.
5. **Nothing is written to disk, and the daemon environment is untouched.** The
   environment is set on the `exec.Cmd` only; there is no `os.Setenv`, so the
   reviewer environment, which `claude.go` builds from `os.Environ()` and then
   allowlists (`reviewProcessEnv`, `GIT_CONFIG_*` is not on it), is unchanged.
   No `.git/config` entry, no file under the clone.
6. **Redaction covers every form git could echo.** `redactSecrets` masks, in
   `runGit` and `gitOutput` for both the output text and the echoed argv, the
   whole `Authorization: Basic <b64>` line, the base64 of
   `x-access-token:<token>` in standard, unpadded, URL-safe and unpadded
   URL-safe encodings, and the raw token. git also masks `Authorization` in its
   curl trace unless `GIT_TRACE_REDACT=0`, which the daemon does not set.
7. **Unchanged:** `FETCH_HEAD` removal (now defence in depth, as the URL it
   records holds no credential), the single-use temp clone, the head-SHA
   fallback (identical credential), error wording and `isMissingRefErr`
   classification, and the scrubbed review environment.

## Alternatives rejected

- **Credential helper.** An inline helper needs the token in a second
  environment variable, which is no better; a file-backed helper (the engine's
  ADR 1846 shape) writes a secret to disk and is process-global.
- **`GIT_CONFIG_*` on the daemon environment** (as the engine does). It would
  violate R3 and reach the reviewer, because the reviewer environment starts
  from `os.Environ()`.
- **`git -c http.extraHeader=…`.** Puts the secret back in argv.
- **`GIT_ASKPASS` with a temp script.** A disk write.
- **A shared `internal/` helper.** Engine git credentials are out of scope, the
  engine's equivalent is unexported and marker-specific, and Pruefer cannot
  import `engine` (ADR 1113). The merge logic is about fifteen lines, so it
  stays private to `pruefer/worktree.go`.
- **Pinning an empty `credential.helper` for the origin.** Not adopted: the
  userinfo form never prevented helper consultation either, so this is not a
  regression. With a header and no credential struct git does not consult
  helpers on a successful fetch, and `GIT_TERMINAL_PROMPT=0` fails a 401 fast.
  It remains an optional hardening.

## Consequences

- No git command line Pruefer runs contains the token or a userinfo URL.
- Residual exposure: the process environment is readable by the same OS user
  and root (and via `/proc/<pid>/environ` on Linux). That is no worse than the
  token held in the daemon's memory, and it removes the world-readable `ps`
  exposure. git's own helper processes (`git-remote-https`) inherit the
  environment, which is expected and carries no argv secret.
- Requires git >= 2.31 for `GIT_CONFIG_COUNT`; the engine already relies on it
  (ADR 1846).
- Tests: `TestCloneForReview_NoCredentialInArgv` (non-vacuity: restoring the
  userinfo URL fails it), `TestGitCredentialEnv`, `TestRedactSecrets`,
  `TestRunGit_RealExecCredentialEnvAndRedaction` (real exec, fake `git` on
  `PATH`) and `TestCloneRefAt_CredentialDeliveredOverHTTPSAndLeftNowhere` (a
  real fetch over TLS through `git http-backend` that only answers when the
  exact `Authorization` header arrives, on the pull-ref and fallback routes,
  then scans every file under the clone for the credential).
