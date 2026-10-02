package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	ghapi "github.com/handarbeit/fabrik/github"
)

// GitHub identities (#1976, ADR-1976). GitHub's GraphQL budget is per
// authenticated identity: every PAT of one user shares that user's 5,000/h, and
// an App installation has a budget of its own (and an App installs once per
// org). So the multi-bed scheduler's unit is not the bed but the IDENTITY, and a
// cell charges a SET of them (D4): its engine identity — the bed's App
// installation on an app leg, the bed token's user on a pat leg — plus the user
// of the harness token it runs with, which is the same bed .env FABRIK_TOKEN.

// Identity is one GitHub identity a cell charges.
type Identity struct {
	// Key is what the scheduler serializes on: "app:<installation id>" (D3),
	// "user:<login>", or — when the login could not be resolved on a single-bed
	// run, or no resolver is configured — "token:<hash>", so two beds with the
	// same token still collide.
	Key string
	// token is the PAT a user identity's budget is read with; app identities mint
	// a token per probe from appBed's .env instead. Never logged.
	token  string
	appBed string
}

// IdentityOps is the seam over every network call identity scheduling makes.
// The real implementation routes each through the bounded Commander path or a
// ctx-bounded in-process mint; tests substitute a fake.
type IdentityOps interface {
	// ResolveLogin is the GitHub login a token authenticates as (GET /user).
	ResolveLogin(ctx context.Context, token string) (string, error)
	// Budget is the token's identity's remaining GraphQL budget and its reset
	// instant (GraphQL's inline rateLimit; see GraphQLBudgetRemaining).
	Budget(ctx context.Context, token string) (remaining int, resetAt string, err error)
	// MintAppToken mints an installation token for the App configured in
	// bedDir/.env (E2E_APP_ID, E2E_APP_PRIVATE_KEY_PATH, E2E_APP_INSTALLATION_ID).
	MintAppToken(ctx context.Context, bedDir string) (string, error)
}

// osIdentityOps is the real IdentityOps.
type osIdentityOps struct{ g *Gate }

// gh runs one bounded, session-reaped `gh` call scoped to token via GH_TOKEN —
// budget.go's required routing point for a network call (#1676).
func (o osIdentityOps) gh(ctx context.Context, token string, args ...string) (string, error) {
	g := o.g
	var so, se syncBuf
	res := g.Exec.Run(ctx, Cmd{
		Name: "gh", Args: args, Env: withEnv(g.Env, "GH_TOKEN="+token),
		Stdout: &so, Stderr: &se,
		Session: true, Timeout: g.Cfg.GHAPITimeout, Grace: g.Cfg.KillGrace,
	})
	switch {
	case res.TimedOut:
		return "", fmt.Errorf("gh %s timed out after %ds", args[0], int(g.Cfg.GHAPITimeout.Seconds()))
	case res.ExitCode != 0 || res.Err != nil:
		return "", fmt.Errorf("gh exited %d: %s", res.ExitCode, strings.TrimSpace(se.String()))
	}
	return strings.TrimSpace(so.String()), nil
}

func (o osIdentityOps) ResolveLogin(ctx context.Context, token string) (string, error) {
	login, err := o.gh(ctx, token, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	if login == "" || login == "null" {
		return "", fmt.Errorf("GET /user returned no login")
	}
	return login, nil
}

func (o osIdentityOps) Budget(ctx context.Context, token string) (int, string, error) {
	out, err := o.gh(ctx, token, "api", "graphql", "-f", "query=query { rateLimit { remaining resetAt } }",
		"--jq", `"\(.data.rateLimit.remaining) \(.data.rateLimit.resetAt)"`)
	if err != nil {
		return 0, "", err
	}
	rem, reset, _ := strings.Cut(out, " ")
	n, perr := strconv.Atoi(rem)
	if perr != nil {
		return 0, "", fmt.Errorf("unparseable budget %q", out)
	}
	return n, reset, nil
}

func (o osIdentityOps) MintAppToken(ctx context.Context, bedDir string) (string, error) {
	envFile := filepath.Join(bedDir, ".env")
	appID := envFileLastValue(envFile, "E2E_APP_ID")
	keyPath := envFileLastValue(envFile, "E2E_APP_PRIVATE_KEY_PATH")
	instStr := envFileLastValue(envFile, "E2E_APP_INSTALLATION_ID")
	if appID == "" || keyPath == "" || instStr == "" {
		return "", fmt.Errorf("E2E_APP_ID / E2E_APP_PRIVATE_KEY_PATH / E2E_APP_INSTALLATION_ID not all set in %s", envFile)
	}
	inst, err := strconv.ParseInt(instStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("E2E_APP_INSTALLATION_ID=%q is not an integer", instStr)
	}
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(bedDir, keyPath)
	}
	pem, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("reading the App private key: %v", err)
	}
	key, err := ghapi.ParseAppPrivateKey(pem)
	if err != nil {
		return "", err
	}
	var issuer any = appID // a Client ID
	if n, err := strconv.ParseInt(appID, 10, 64); err == nil {
		issuer = n
	}
	jwt, err := ghapi.BuildAppJWT(issuer, key)
	if err != nil {
		return "", err
	}
	// The mint's own HTTP client is bounded (30s); running it beside ctx keeps a
	// cancelled gate from waiting on it.
	type minted struct {
		tok string
		err error
	}
	ch := make(chan minted, 1)
	go func() {
		tok, _, err := ghapi.MintInstallationToken("", jwt, inst)
		ch <- minted{tok, err}
	}()
	select {
	case m := <-ch:
		return m.tok, m.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// resolveLogin is ResolveLogin memoised per token across every bed (root only).
func (g *Gate) resolveLogin(ctx context.Context, token string) (string, error) {
	r := g.root()
	r.idMu.Lock()
	if login, ok := r.loginCache[token]; ok {
		r.idMu.Unlock()
		return login, nil
	}
	r.idMu.Unlock()
	login, err := r.Identity.ResolveLogin(ctx, token)
	if err != nil {
		return "", err
	}
	r.idMu.Lock()
	if r.loginCache == nil {
		r.loginCache = map[string]string{}
	}
	r.loginCache[token] = login
	r.idMu.Unlock()
	return login, nil
}

// ResolveBedIdentities is the identity live preflight: each bed token's login,
// resolved once per distinct token. It makes a REST call, so it runs after the
// pre-gate (ADR-1454). On a multi-bed run an unresolvable login refuses
// (ExitPreconditionFailed): a leg whose identity is unknown would run
// unscheduled. With one bed it only warns — identity there feeds the budget
// lines, never a schedule.
func (g *Gate) ResolveBedIdentities(ctx context.Context) error {
	if g.Identity == nil {
		return nil
	}
	for _, b := range g.beds() {
		if b.Cfg.BedToken == "" {
			continue // single bed: already warned; multi-bed: CheckBedTopology refused
		}
		login, err := g.resolveLogin(ctx, b.Cfg.BedToken)
		if err != nil {
			if g.multiBed() {
				return exitErr(ExitPreconditionFailed, "PRECONDITION FAILED: cannot resolve the GitHub login of %s's FABRIK_TOKEN (%v) — a leg whose identity is unknown cannot be scheduled safely against the other beds (#1976)", b.bedLabel(), err)
			}
			g.errf("warning: cannot resolve the GitHub login of the bed's FABRIK_TOKEN (%v) — the identity budget lines will call it \"bed FABRIK_TOKEN\"\n", err)
			continue
		}
		b.login = login
		if g.multiBed() {
			b.outf("== bed identity: FABRIK_TOKEN authenticates as %s ==\n", login)
		}
	}
	return nil
}

// appInstallationID is the bed's configured App installation ("" when none).
func (g *Gate) appInstallationID() string {
	if g.bed != nil {
		return g.bed.AppInstallationID
	}
	return envFileLastValue(filepath.Join(g.Cfg.TestBed, ".env"), "E2E_APP_INSTALLATION_ID")
}

// tokenIdentity is the bed token's identity: its user when resolved, else a key
// derived from the token itself (never the token). ok=false: no token.
func (g *Gate) tokenIdentity() (Identity, bool) {
	tok := g.Cfg.BedToken
	if tok == "" {
		return Identity{}, false
	}
	if g.login != "" {
		return Identity{Key: "user:" + g.login, token: tok}, true
	}
	sum := sha256.Sum256([]byte(tok))
	return Identity{Key: "token:" + hex.EncodeToString(sum[:4]), token: tok}, true
}

// engineIdentity is the identity the bed ENGINE spends on a leg of this auth
// mode — the one the engine's own rate-limit backoff (RUN INVALID) reports on.
func (g *Gate) engineIdentity(auth string) (Identity, bool) {
	if auth == "app" {
		if id := g.appInstallationID(); id != "" {
			return Identity{Key: "app:" + id, appBed: g.Cfg.TestBed}, true
		}
	}
	return g.tokenIdentity()
}

// identitySet is every identity a cell of this auth mode charges on this bed:
// the engine identity plus the harness token's (D4), deduplicated and sorted —
// the order the scheduler acquires them in.
func (g *Gate) identitySet(auth string) []Identity {
	var set []Identity
	seen := map[string]bool{}
	add := func(id Identity, ok bool) {
		if ok && !seen[id.Key] {
			seen[id.Key] = true
			set = append(set, id)
		}
	}
	add(g.engineIdentity(auth))
	add(g.tokenIdentity())
	sort.Slice(set, func(i, j int) bool { return set[i].Key < set[j].Key })
	return set
}

// identityLabel is how a budget line names an identity.
func (g *Gate) identityLabel(id Identity) string {
	switch {
	case id.appBed != "" && g.appSlug != "":
		return id.Key + " " + g.appSlug
	case strings.HasPrefix(id.Key, "token:"):
		return "bed FABRIK_TOKEN"
	}
	return id.Key
}

// logIdentityBudget is R5: one line per identity the cell charges, with its
// remaining GraphQL budget and reset instant, at leg start and end — so budget
// pressure on every identity shows up in the output. App identities mint an
// installation token per probe. A failure warns and never gates; no token text
// ever reaches the output.
func (g *Gate) logIdentityBudget(ctx context.Context, cell Cell, which string) {
	if g.Identity == nil {
		return
	}
	label := cell.Label()
	for _, id := range g.identitySet(cell.Auth) {
		if ctx.Err() != nil {
			return
		}
		token := id.token
		if id.appBed != "" {
			t, err := g.Identity.MintAppToken(ctx, id.appBed)
			if err != nil {
				g.errf("warning: identity budget (leg: %s, %s): cannot mint a token for %s: %v\n", label, which, id.Key, err)
				continue
			}
			token = t
		}
		rem, reset, err := g.Identity.Budget(ctx, token)
		if err != nil {
			g.errf("warning: identity budget (leg: %s, %s): reading %s's budget failed: %v\n", label, which, g.identityLabel(id), err)
			continue
		}
		g.outf("== identity budget (leg: %s, %s): %s — %d remaining, resets %s ==\n", label, which, g.identityLabel(id), rem, reset)
	}
}

// bedStartMarker is the first line every Fabrik startup prints (cmd/root.go).
const bedStartMarker = "Fabrik starting "

// The App-auth startup lines (cmd/root.go and engine/github_app_auth.go):
//
//	  identity: GitHub App installation <N>
//	[startup] authenticated as <slug>[bot] (GitHub App installation, organization "<org>")
const (
	bannerInstallationPrefix = "identity: GitHub App installation "
	bannerAuthPrefix         = "[startup] authenticated as "
	bannerAuthSuffix         = "(GitHub App installation"
)

// parseBedAppBanner reads the App installation and bot login from the MOST
// RECENT startup in a bed stdout log (bed-run.log accumulates across harness
// restarts, so only the lines after the last start marker count). "" for
// whatever the startup did not print.
func parseBedAppBanner(stdout string) (installation, slug string) {
	if i := strings.LastIndex(stdout, bedStartMarker); i >= 0 {
		stdout = stdout[i:]
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, bannerInstallationPrefix); ok && installation == "" {
			installation = strings.TrimSpace(rest)
		}
		if rest, ok := strings.CutPrefix(line, bannerAuthPrefix); ok && slug == "" && strings.Contains(rest, bannerAuthSuffix) {
			slug, _, _ = strings.Cut(rest, " ")
		}
	}
	return installation, slug
}

// verifyBedAppIdentity is D3's post-restart cross-check on an app leg: the bed's
// startup banner must name the App installation its .env configures, the key
// the scheduler serialized this leg on. Missing or different fails the leg with
// ExitPreconditionFailed — loudly, rather than a leg running on an identity it
// was not scheduled for. Skipped when the bed's .env names no installation (the
// auth-mode preflight guarantees one on every real app leg).
func (g *Gate) verifyBedAppIdentity(label string) error {
	want := g.appInstallationID()
	if want == "" {
		return nil
	}
	path := filepath.Join(g.Cfg.TestBed, "bed-run.log")
	data, err := os.ReadFile(path)
	if err != nil {
		return exitErr(ExitPreconditionFailed, "PRECONDITION FAILED (leg: %s): cannot read %s to verify the bed's App identity: %v", label, path, err)
	}
	inst, slug := parseBedAppBanner(string(data))
	switch {
	case inst == "":
		return exitErr(ExitPreconditionFailed, "PRECONDITION FAILED (leg: %s): the bed restarted for an app leg but its startup banner in %s names no GitHub App installation (expected %s, from E2E_APP_INSTALLATION_ID in %s/.env)", label, path, want, g.Cfg.TestBed)
	case inst != want:
		return exitErr(ExitPreconditionFailed, "PRECONDITION FAILED (leg: %s): the bed started as GitHub App installation %s, but its .env configures %s — the leg was scheduled on app:%s and would spend another identity's budget", label, inst, want, want)
	}
	g.appSlug = slug
	return nil
}
