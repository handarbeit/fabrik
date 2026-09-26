//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ghapi "github.com/handarbeit/fabrik/github"
)

// App self-recognition scenarios (#1877). Under GitHub App auth the engine's
// own GitHub-facing identity is "<app-slug>[bot]", not the operator login
// (#1754, engine.selfLogin). These helpers decide whether a scenario applies to
// the running auth leg and let the harness post as the App, which is the only
// way to author a plain (marker-free) bot comment: every comment Fabrik posts
// itself carries the "🏭 **Fabrik" prefix that findNewComments excludes before
// filterHuman is ever consulted, so it cannot exercise the author path.
//
// The installation token is a secret. It is passed to gh through the
// environment only (ghOutputWithToken), never in argv, and never handed to
// t.Log/t.Fatalf — every gh output that reaches a failure message goes through
// redactSecret first.

// errAppIdentityUnset means none of the bed-local E2E_APP_* keys is set: the
// bed has no App identity to mint from. A clean skip, not a failure.
var errAppIdentityUnset = errors.New("no E2E_APP_* keys set")

// decideAppLegRun decides whether an App-only scenario applies. mode is the
// normalised E2E_AUTH_MODE, identity is bedAuthIdentity of the bed's stdout
// (the App login, or "" when the last startup ran without App auth).
//
//   - pat: skip. The identity is the harness's own account, which the other
//     scenarios already exercise.
//   - app: run, but a missing identity is an error — the leg was asked to run
//     as the App and the bed says it did not (verifyBedAuthIdentity's rule).
//   - unset: follow the bed's own identity; no App banner means skip.
func decideAppLegRun(mode, identity string) (run bool, reason string, err error) {
	switch mode {
	case "pat":
		return false, "PAT auth leg: Fabrik's identity is the harness account, which the other scenarios already exercise; this scenario needs the GitHub App identity", nil
	case "app":
		if identity == "" {
			return false, "", errors.New("E2E_AUTH_MODE=app but the bed's startup shows no GitHub App identity")
		}
		return true, "", nil
	case "":
		if identity == "" {
			return false, "cannot determine an App identity: E2E_AUTH_MODE is unset and the bed's startup shows no GitHub App banner", nil
		}
		return true, "", nil
	}
	return false, "", fmt.Errorf("unrecognised auth mode %q", mode)
}

// appIdentity is the bed-local App identity read from E2E_APP_*.
type appIdentity struct {
	AppID          int64
	KeyPath        string // absolute
	InstallationID int64
}

// parseAppIdentity validates the raw E2E_APP_* values. keyPath is resolved
// against baseDir (the bed directory) when relative. All three empty returns
// errAppIdentityUnset; a partial set is a hard error, because it can only be a
// misconfigured bed.
func parseAppIdentity(appID, keyPath, installationID, baseDir string) (appIdentity, error) {
	appID, keyPath, installationID = strings.TrimSpace(appID), strings.TrimSpace(keyPath), strings.TrimSpace(installationID)
	if appID == "" && keyPath == "" && installationID == "" {
		return appIdentity{}, errAppIdentityUnset
	}
	var missing []string
	for i, v := range []string{appID, keyPath, installationID} {
		if v == "" {
			missing = append(missing, bedAppIdentityEnvKeys[i])
		}
	}
	if len(missing) > 0 {
		return appIdentity{}, fmt.Errorf("incomplete App identity: %s not set", strings.Join(missing, ", "))
	}
	id, err := strconv.ParseInt(appID, 10, 64)
	if err != nil || id <= 0 {
		return appIdentity{}, fmt.Errorf("%s=%q is not a positive integer", bedAppIdentityEnvKeys[0], appID)
	}
	inst, err := strconv.ParseInt(installationID, 10, 64)
	if err != nil || inst <= 0 {
		return appIdentity{}, fmt.Errorf("%s=%q is not a positive integer", bedAppIdentityEnvKeys[2], installationID)
	}
	if strings.HasPrefix(keyPath, "~") {
		keyPath = expandHome(keyPath)
	}
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(baseDir, keyPath)
	}
	return appIdentity{AppID: id, KeyPath: keyPath, InstallationID: inst}, nil
}

// redactSecret removes every occurrence of secret from s. An empty secret is a
// no-op (strings.ReplaceAll would otherwise interleave the replacement between
// every rune).
func redactSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}

// readAppIdentity reads E2E_APP_* from the bed's .env.
func readAppIdentity(env *Env) (appIdentity, error) {
	envFile := filepath.Join(env.FabrikTestDir, ".env")
	vals := make([]string, len(bedAppIdentityEnvKeys))
	for i, key := range bedAppIdentityEnvKeys {
		// A missing key is reported as unset by parseAppIdentity, not as a read error.
		vals[i], _ = readEnvFileValue(envFile, key)
	}
	return parseAppIdentity(vals[0], vals[1], vals[2], env.FabrikTestDir)
}

// mintAppInstallationToken mints an installation token from the identity,
// reusing the github package's App helpers (the same ones the engine uses)
// rather than a second JWT implementation. Errors never contain the token.
func mintAppInstallationToken(id appIdentity) (string, error) {
	pemBytes, err := os.ReadFile(id.KeyPath)
	if err != nil {
		return "", fmt.Errorf("reading App private key %s: %w", id.KeyPath, err)
	}
	key, err := ghapi.ParseAppPrivateKey(pemBytes)
	if err != nil {
		return "", fmt.Errorf("parsing App private key %s: %w", id.KeyPath, err)
	}
	jwt, err := ghapi.BuildAppJWT(id.AppID, key)
	if err != nil {
		return "", fmt.Errorf("building App JWT: %w", err)
	}
	token, _, err := ghapi.MintInstallationToken("", jwt, id.InstallationID)
	if err != nil {
		return "", redactedErr(err, jwt)
	}
	if token == "" {
		return "", errors.New("minting installation token: empty token in response")
	}
	return token, nil
}

// redactedErr returns err with secret scrubbed from its message.
func redactedErr(err error, secret string) error {
	return errors.New(redactSecret(err.Error(), secret))
}

// appLeg is what requireAppLeg hands the scenario.
type appLeg struct {
	BotLogin string // "<slug>[bot]" as printed in the bed's startup banner
	Token    string // installation token; a secret — never log it
}

// requireAppLeg skips unless the running leg is the App leg, and mints an
// installation token from the bed-local E2E_APP_* keys. It fails (does not
// skip) when the leg is App but the identity is unusable, so a broken bed is
// never mistaken for "not applicable".
func requireAppLeg(t *testing.T, env *Env) appLeg {
	t.Helper()
	mode, err := normalizeAuthMode(os.Getenv("E2E_AUTH_MODE"))
	if err != nil {
		t.Fatalf("%v", err)
	}
	identity := ""
	if data, rerr := os.ReadFile(bedRunLogPath(env)); rerr == nil {
		identity = bedAuthIdentity(string(data))
	} else if mode == "app" {
		t.Fatalf("reading bed stdout %s to confirm the App identity: %v", bedRunLogPath(env), rerr)
	}
	run, reason, err := decideAppLegRun(mode, identity)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !run {
		t.Skip(reason)
	}
	appID, err := readAppIdentity(env)
	switch {
	case errors.Is(err, errAppIdentityUnset):
		t.Skipf("bed .env has no %s — cannot post as the App", strings.Join(bedAppIdentityEnvKeys, "/"))
	case err != nil:
		t.Fatalf("bed App identity: %v", err)
	}
	token, err := mintAppInstallationToken(appID)
	if err != nil {
		t.Fatalf("minting App installation token: %v", err)
	}
	return appLeg{BotLogin: identity, Token: token}
}

// postedComment is the slice of a created-comment response the scenarios use.
type postedComment struct {
	ID       int64
	Login    string
	UserType string
}

// postCommentAs posts a comment on an issue or PR with the given token and
// returns the created comment's id and author as GitHub reports them.
func postCommentAs(token, repo string, number int, body string) (postedComment, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return postedComment{}, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutputWithToken(token, "api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, number),
		"-f", "body="+body)
	if err != nil {
		return postedComment{}, fmt.Errorf("posting comment on %s#%d: %v\n%s", repo, number, err, redactSecret(out, token))
	}
	var resp struct {
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return postedComment{}, fmt.Errorf("parsing created-comment response for %s#%d: %v", repo, number, err)
	}
	if resp.ID == 0 {
		return postedComment{}, fmt.Errorf("created-comment response for %s#%d carried no id", repo, number)
	}
	return postedComment{ID: resp.ID, Login: resp.User.Login, UserType: resp.User.Type}, nil
}

// postBotComment posts as the App and fails unless GitHub attributes the
// comment to the bed's bot login (and type Bot): a comment posted as anything
// else would make the scenario's "bot-authored" premise false.
func postBotComment(t *testing.T, leg appLeg, repo string, number int, body string) postedComment {
	t.Helper()
	c, err := postCommentAs(leg.Token, repo, number, body)
	if err != nil {
		t.Fatalf("posting bot comment: %v", err)
	}
	if c.Login != leg.BotLogin || c.UserType != "Bot" {
		t.Fatalf("comment %d on %s#%d was attributed to %q (type %q); want %q (type Bot)",
			c.ID, repo, number, c.Login, c.UserType, leg.BotLogin)
	}
	return c
}

// assertHarnessAccountIsHuman fails unless the harness account (env.GHToken)
// reads as human to Fabrik. In the App leg that account (not the bot) is the
// "human" of every positive control; if it classified as a bot the control
// could never resume anything and the scenario would be meaningless.
func assertHarnessAccountIsHuman(t *testing.T, env *Env) string {
	t.Helper()
	login := TokenLogin(t, env.GHToken)
	if ghapi.IsBotLogin(login) {
		t.Fatalf("harness account %q classifies as a bot (github.IsBotLogin) — it cannot serve as the human in this scenario", login)
	}
	return login
}

// logCommentAuthorShapes logs, for one comment, the author as REST reports it
// and as the GraphQL path the engine reads issue comments through reports it.
// The two can differ ("<slug>[bot]" vs the bare "<slug>"), which decides
// whether Fabrik's own self-recognition comparisons match — pure diagnostics,
// never asserted, so a failure elsewhere in the scenario can be attributed.
func logCommentAuthorShapes(t *testing.T, env *Env, repo string, issueNumber int, commentID int64) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		return
	}
	restOut, restErr := ghOutput(env, "api", fmt.Sprintf("repos/%s/%s/issues/comments/%d", owner, name, commentID),
		"--jq", `.user.login + " (" + .user.type + ")"`)
	q := fmt.Sprintf(`query { repository(owner: "%s", name: "%s") { issue(number: %d) { comments(last: 50) { nodes { databaseId author { __typename login } } } } } }`,
		owner, name, issueNumber)
	gqlOut, gqlErr := ghOutput(env, "api", "graphql", "-f", "query="+q,
		"--jq", fmt.Sprintf(`.data.repository.issue.comments.nodes[] | select(.databaseId == %d) | .author.login + " (" + .author.__typename + ")"`, commentID))
	t.Logf("comment %d author shapes on %s#%d: REST=%q (err %v) GraphQL=%q (err %v) — a bare GraphQL login here means selfLogin() "+
		"comparisons against GraphQL-sourced comments cannot match", commentID, repo, issueNumber,
		strings.TrimSpace(restOut), restErr, strings.TrimSpace(gqlOut), gqlErr)
}
