package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// GitHub caps a label name at 50 characters. "fabrik:locked:" takes 14, so a
// lock identity may be at most 36. It is "<slug>-<6 hex>", which leaves 29 for
// the slug (#1893, ADR-1893).
const (
	lockLabelPrefix      = "fabrik:locked:"
	maxLockIdentityLen   = 50 - len(lockLabelPrefix)
	lockIdentitySuffixLn = 6
	maxLockSlugLen       = maxLockIdentityLen - 1 - lockIdentitySuffixLn
	// appLockFallbackSlug stands in when an App-mode Engine has no resolvable
	// bot login (only the sim seam SetGitHubAppModeForTest ever produces
	// that): an empty slug must never yield the label "fabrik:locked:".
	appLockFallbackSlug = "app"
)

// appIdentity is the identity a GitHub-App-authenticated Engine acts under
// instead of an operator's (#1893). It is resolved once in New() and is nil in
// PAT mode, where cfg.User keeps every role it has always had.
type appIdentity struct {
	slug      string // app slug, no "[bot]" suffix
	botLogin  string // "<slug>[bot]"
	botUserID int64  // 0 when the GET /users/<slug>[bot] lookup failed or was not attempted
	lockID    string // "<slug>-<6 hex>", instance-distinct, fits the 50-char label limit
}

// commitIdentity is the git author/committer pair written to a bare clone.
type commitIdentity struct {
	Name  string
	Email string
}

// newAppIdentity builds the App identity for a bot login ("<slug>[bot]") and
// the directory of the instance it runs in. botUserID may be 0.
func newAppIdentity(botLogin string, botUserID int64, hostname, fabrikDir string) *appIdentity {
	slug := stripBotSuffix(botLogin)
	id := &appIdentity{slug: slug, botUserID: botUserID}
	if slug != "" {
		id.botLogin = slug + "[bot]"
	}
	id.lockID = deriveLockID(slug, hostname, fabrikDir)
	return id
}

// deriveLockID returns "<slug>-<6 hex>" where the hex is the leading part of
// sha256(hostname NUL abs(fabrikDir)). The suffix makes two local instances
// sharing one App installation hold distinct lock labels, which keeps the
// skip check, the lexicographic tie-break and the own-label-only cleanup
// correct — with one shared label all three silently misbehave (ADR-1893).
// The slug is truncated deterministically so the whole label stays within
// GitHub's 50-character limit; the suffix keeps truncated slugs distinct.
func deriveLockID(slug, hostname, fabrikDir string) string {
	if slug == "" {
		slug = appLockFallbackSlug
	}
	if len(slug) > maxLockSlugLen {
		slug = strings.TrimRight(slug[:maxLockSlugLen], "-")
	}
	dir := fabrikDir
	if abs, err := filepath.Abs(fabrikDir); err == nil {
		dir = abs
	}
	sum := sha256.Sum256([]byte(hostname + "\x00" + dir))
	return slug + "-" + hex.EncodeToString(sum[:])[:lockIdentitySuffixLn]
}

// userIDFetcher is the slice of *gh.Client resolveAppIdentity needs.
type userIDFetcher interface {
	FetchUserID(login string) (int64, error)
}

// resolveAppIdentity builds the App identity at startup, resolving the bot's
// numeric user ID once. A failed lookup is soft: it logs a loud warning and
// carries on with the ID-less email, since refusing to start over an avatar
// link is disproportionate — the commit *name* is still "<slug>[bot]". The
// clone-marker protocol (setAppCommitterIdentity) upgrades the email on a
// later start where the lookup succeeds.
func resolveAppIdentity(client userIDFetcher, botLogin, fabrikDir string) *appIdentity {
	host, _ := os.Hostname()
	var botUserID int64
	if botLogin != "" {
		id, err := client.FetchUserID(botLogin)
		if err != nil {
			fmt.Printf("[startup] warning: could not resolve the user ID of %s: %v — commits will be authored as %s with an ID-less "+
				"noreply email, which GitHub does not link to the App's avatar; it is upgraded on a later start once the lookup succeeds\n",
				botLogin, err, botLogin)
		} else {
			botUserID = id
		}
	}
	return newAppIdentity(botLogin, botUserID, host, fabrikDir)
}

// appID returns the App identity, or nil in PAT mode. New() and the sim seams
// set e.appIdent once, before any goroutine runs; an Engine whose ghAppAuth
// was assigned directly (unit tests) gets an identity computed on the fly, so
// every accessor below is total in App mode without a racy lazy write.
func (e *Engine) appID() *appIdentity {
	if e.ghAppAuth == nil {
		return nil
	}
	if e.appIdent != nil {
		return e.appIdent
	}
	host, _ := os.Hostname()
	return newAppIdentity(e.ghAppAuth.BotLogin(), 0, host, e.fabrikDir)
}

// appBotLogin returns the App's bot login ("<slug>[bot]") under App auth and
// "" otherwise.
func (e *Engine) appBotLogin() string {
	if id := e.appID(); id != nil {
		return id.botLogin
	}
	return ""
}

// lockIdentity is the name Fabrik holds a fabrik:locked:<name> label under:
// cfg.User in PAT mode (unchanged), the derived App lock ID under App auth —
// where a configured user: is deliberately ignored (R8).
func (e *Engine) lockIdentity() string {
	if id := e.appID(); id != nil {
		return id.lockID
	}
	return e.cfg.User
}

// lockLabel is this instance's own lock label.
func (e *Engine) lockLabel() string {
	return lockLabelPrefix + e.lockIdentity()
}

// commitIdentity returns the git identity commits are authored under in App
// mode, or nil in PAT mode (where ensureBareClone keeps its cfg.User path).
// The email is GitHub's documented App-attribution form
// "<bot-user-id>+<slug>[bot]@users.noreply.github.com"; when the ID lookup
// failed it falls back to the ID-less form, which still names the App but
// does not link to its avatar.
func (e *Engine) commitIdentity() *commitIdentity {
	id := e.appID()
	if id == nil || id.botLogin == "" {
		return nil
	}
	email := id.botLogin + "@users.noreply.github.com"
	if id.botUserID > 0 {
		email = strconv.FormatInt(id.botUserID, 10) + "+" + email
	}
	return &commitIdentity{Name: id.botLogin, Email: email}
}

// operatorNote names the human a TUI-originated stop should be attributed to:
// cfg.User in PAT mode, "" under App auth (there is no operator identity).
func (e *Engine) operatorNote() string {
	if e.ghAppAuth != nil {
		return ""
	}
	return e.cfg.User
}

// isAppSelf reports whether login is the Fabrik App itself, in either the
// REST ("<slug>[bot]") or GraphQL ("<slug>") shape.
func (e *Engine) isAppSelf(login string) bool {
	id := e.appID()
	if id == nil || id.slug == "" {
		return false
	}
	return strings.EqualFold(stripBotSuffix(login), id.slug)
}

// humanLogins filters out empty logins, bot logins and the Fabrik App itself,
// preserving order and dropping duplicates.
func (e *Engine) humanLogins(logins []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range logins {
		if l == "" || gh.IsBotLogin(l) || e.isAppSelf(l) || seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	return out
}

// mentionTargets returns the logins an "awaiting input" notification should
// @mention. PAT mode: the configured operator, or nobody when unset —
// byte-identical to the pre-#1893 behavior. App mode (R4): the item's human
// assignees, else its human author, else nobody.
func (e *Engine) mentionTargets(item gh.ProjectItem) []string {
	if e.ghAppAuth == nil {
		if e.cfg.User == "" {
			return nil
		}
		return []string{e.cfg.User}
	}
	if assignees := e.humanLogins(item.Assignees); len(assignees) > 0 {
		return assignees
	}
	return e.humanLogins([]string{item.Author})
}

// spawnAssignees returns the assignees for a spawned child. PAT mode:
// []string{cfg.User}, exactly as before. App mode (R4): the parent's human
// assignees (possibly none) — never the operator, never the author.
func (e *Engine) spawnAssignees(parent gh.ProjectItem) []string {
	if e.ghAppAuth == nil {
		return []string{e.cfg.User}
	}
	return e.humanLogins(parent.Assignees)
}

// legacyLockLabel returns the fabrik:locked:<user> label a pre-App-auth run of
// this instance would have held — non-empty only under App auth with a still-
// configured user: — for the one-shot startup sweep in runStartupCleanup.
func (e *Engine) legacyLockLabel() string {
	if e.ghAppAuth == nil || e.cfg.User == "" {
		return ""
	}
	if label := lockLabelPrefix + e.cfg.User; label != e.lockLabel() {
		return label
	}
	return ""
}
