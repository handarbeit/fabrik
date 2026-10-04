// Package workerenv is the audited classification of every FABRIK_* variable
// the engine reads, and of the names a Claude stage worker's environment must
// never lose or gain (#2027, ADR-2027).
//
// Root cause it guards against: cmd/root.go loads .env into the daemon's own
// process environment (config.LoadDotenv), and the worker environment is built
// from os.Environ() — so every .env entry (the App private-key path, the
// webhook secret, the PAT, a reviewer token …) reached every worker unless it
// was explicitly removed. Anything not listed here as forwarded is scrubbed:
// the default is scrubbed, a variable is forwarded only with a stated reason.
//
// The package is stdlib-only and imports nothing from the engine so that
// engine, cmd and every package's tests can share it without an import cycle.
package workerenv

import (
	"sort"
	"strings"
)

// Group tags a scrubbed variable with why it is daemon-only.
type Group string

const (
	GroupCredential Group = "credential"
	GroupHookdeck   Group = "hookdeck"
	GroupIdentity   Group = "identity"
	GroupTuning     Group = "tuning"
	GroupReexec     Group = "reexec"
)

// Var is one classified variable.
type Var struct {
	Name   string
	Group  Group
	Reason string
}

// OptInVar is the explicit, named opt-in (R1b): a comma-separated list of exact
// scrubbed names a worker is allowed to inherit. It is itself scrubbed.
const OptInVar = "FABRIK_WORKER_ENV_PASSTHROUGH"

// DefaultHookdeckAPIKeyEnv / DefaultHookdeckWebhookSecretEnv mirror the
// engine's defaults (engine.DefaultHookdeck*). They are scrubbed
// unconditionally so a stale .env entry never reaches a worker whatever
// event_source is configured.
const (
	DefaultHookdeckAPIKeyEnv        = "HOOKDECK_API_KEY"
	DefaultHookdeckWebhookSecretEnv = "FABRIK_GITHUB_WEBHOOK_SECRET"
)

func cred(name, reason string) Var { return Var{name, GroupCredential, reason} }
func ident(name string) Var {
	return Var{name, GroupIdentity, "daemon identity/control; a worker has no use for it"}
}
func tune(name string) Var {
	return Var{name, GroupTuning, "engine tuning knob; a worker has no use for it"}
}

// Scrubbed is every daemon-only variable, removed from every worker env.
var Scrubbed = []Var{
	cred("FABRIK_TOKEN", "operator PAT (and the upgrade token source)"),
	cred("FABRIK_GITHUB_APP_ID", "App identity; with the key path lets a worker mint tokens"),
	cred("FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "path to the App private key; lets a worker mint installation tokens indefinitely"),
	cred("FABRIK_GITHUB_APP_INSTALLATION_ID", "App installation pin"),
	cred("FABRIK_GITHUB_WEBHOOK_SECRET", "lets a worker forge webhook deliveries"),
	cred("FABRIK_REVIEWER_TOKEN", "second operator credential (e2e); may share the same .env"),
	cred("FABRIK_ANTHROPIC_API_KEY", "ADR-1346 opt-in source; translated by the engine, never forwarded"),
	cred("FABRIK_ANTHROPIC_ENV_PASSTHROUGH", "ADR-1346 control variable"),
	cred(OptInVar, "the #2027 opt-in itself is never forwarded"),
	{"FABRIK_HOOKDECK_API_KEY_ENV", GroupHookdeck, "names the variable holding the Hookdeck API key"},
	{"FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV", GroupHookdeck, "names the variable holding the webhook secret"},
	{DefaultHookdeckAPIKeyEnv, GroupHookdeck, "default Hookdeck API key variable (not FABRIK_-prefixed)"},

	ident("FABRIK_OWNER"), ident("FABRIK_PROJECT_NUMBER"), ident("FABRIK_USER"),
	ident("FABRIK_STAGES"), ident("FABRIK_YOLO"), ident("FABRIK_GIT_SSH"),
	ident("FABRIK_NO_BROWSER"), ident("FABRIK_GHES_HOST"), ident("FABRIK_INSTANCE_ID"),
	ident("FABRIK_PLUGIN_DIR"), ident("FABRIK_AUTO_UPGRADE"), ident("FABRIK_TUI"),
	ident("FABRIK_DEBUG_OUTPUT"), ident("FABRIK_SYMLINK_ENV"),
	ident("FABRIK_WORKTREE_BOUNDARY_AUDIT"), ident("FABRIK_EVENT_SOURCE"),
	ident("FABRIK_WEBHOOKS"), ident("FABRIK_WEBHOOK_PORT"), ident("FABRIK_WEBHOOK_EVENTS"),
	ident("FABRIK_STATUS_POLL"), ident("FABRIK_TEST_POLL_CONTROL"),

	tune("FABRIK_POLL"), tune("FABRIK_RETRY_BACKOFF"), tune("FABRIK_MAX_CONCURRENT"),
	tune("FABRIK_MAX_RETRIES"), tune("FABRIK_MAX_SLICE_RETRIES"),
	tune("FABRIK_MAX_RESUME_FAILURES"), tune("FABRIK_MAX_TOOLS_DENIED_RETRIES"),
	tune("FABRIK_MAX_REVIEW_CYCLES"), tune("FABRIK_MAX_CI_FIX_CYCLES"),
	tune("FABRIK_MAX_REBASE_CYCLES"), tune("FABRIK_MAX_ENQUEUE_CYCLES"),
	tune("FABRIK_MAX_BATCH_SIZE"), tune("FABRIK_MAX_BISECT_VALIDATIONS"),
	tune("FABRIK_MAX_TRAIN_REBASE_CYCLES"), tune("FABRIK_MAX_TRAIN_TRIALS_PER_WINDOW"),
	tune("FABRIK_MAX_COMMENT_CYCLES_PER_WINDOW"), tune("FABRIK_MAX_NO_OP_COMMENT_CYCLES"),
	tune("FABRIK_REVIEW_WAIT_TIMEOUT"), tune("FABRIK_CI_WAIT_TIMEOUT"),
	tune("FABRIK_CI_BACKSTOP_TIMEOUT"), tune("FABRIK_STALL_THRESHOLD"),
	tune("FABRIK_WORKER_STALE_TIMEOUT"), tune("FABRIK_CONVERGENCE_BUDGET"),
	tune("FABRIK_AUTO_MERGE_STRATEGY"), tune("FABRIK_MERGE_QUEUE"), tune("FABRIK_MERGE_TRAIN"),
	tune("FABRIK_TRAIN_TRIAL_WINDOW"), tune("FABRIK_COMMENT_CYCLE_WINDOW"),
	tune("FABRIK_CLAUDE_WAIT_DELAY"), tune("FABRIK_POST_PUSH_DWELL"),
	tune("FABRIK_RECONCILE_INTERVAL"), tune("FABRIK_JANITOR_INTERVAL"),
	tune("FABRIK_LOG_RETENTION_DAYS"), tune("FABRIK_LOG_MAX_BYTES"),
	tune("FABRIK_SESSION_RETENTION_DAYS"), tune("FABRIK_KILL_GRACE_SIGINT"),
	tune("FABRIK_KILL_GRACE_SIGTERM"), tune("FABRIK_DRAIN_DEADLINE"),
	tune("FABRIK_ARCHIVE_AFTER"), tune("FABRIK_ARCHIVE_DONE"),

	{"FABRIK_SIGHUP_RESTART", GroupReexec, "re-exec marker, consumed at startup (defensive)"},
	{"FABRIK_AUTO_UPGRADED", GroupReexec, "re-exec marker, consumed at startup (defensive)"},
}

// EngineSet is the forwarded class: variables the engine itself sets for
// workers on every invocation (ADR-1288). They are never scrubbed — the
// engine's own override always wins — and each carries its reason.
var EngineSet = []Var{
	{"FABRIK_ISSUE", "", "invocation fact: the issue number the worker is acting on"},
	{"FABRIK_REPO", "", "invocation fact; also a daemon config read, but always overridden for the worker"},
	{"FABRIK_WORKTREE", "", "invocation fact: the worker's worktree"},
	{"FABRIK_ROOT", "", "invocation fact: the fabrik directory"},
	{"FABRIK_PR", "", "invocation fact: linked PR number (omitted when none)"},
}

// NotEnv lists FABRIK_* string literals that are output markers or prose, not
// environment variables. A new marker constant must be added here — failing
// safe, since it forces a classification decision.
var NotEnv = []string{
	"FABRIK_STAGE_COMPLETE", "FABRIK_BLOCKED_ON_INPUT", "FABRIK_NO_WORK_NEEDED",
	"FABRIK_ISSUE_UPDATE_BEGIN", "FABRIK_ISSUE_UPDATE_END",
	"FABRIK_SUMMARY_BEGIN", "FABRIK_SUMMARY_END",
	"FABRIK_PR_CREATE_BEGIN", "FABRIK_PR_CREATE_END",
	"FABRIK_SPAWN_CHILD_BEGIN", "FABRIK_SPAWN_CHILD_END",
}

// Protected names may never be scrubbed or admitted by an operator-supplied
// name (Hookdeck indirection, the opt-in): they carry the worker's sanctioned
// GitHub access (R2) or are essential to the process.
func Protected(name string) bool {
	switch name {
	case "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_ENTERPRISE_TOKEN",
		"PATH", "HOME", "USER", "SHELL", "TMPDIR", "PWD", "LANG", "TERM":
		return true
	}
	if strings.HasPrefix(name, "GIT_CONFIG_") || strings.HasPrefix(name, "FABRIK_") && isEngineSet(name) {
		return true
	}
	return false
}

func isEngineSet(name string) bool {
	for _, v := range EngineSet {
		if v.Name == name {
			return true
		}
	}
	return false
}

// IsScrubbed reports whether name is on the fixed scrub list.
func IsScrubbed(name string) bool {
	for _, v := range Scrubbed {
		if v.Name == name {
			return true
		}
	}
	return false
}

// Classified reports whether a FABRIK_* literal is accounted for.
func Classified(name string) bool {
	if IsScrubbed(name) || isEngineSet(name) {
		return true
	}
	for _, n := range NotEnv {
		if n == name {
			return true
		}
	}
	return false
}

// ScrubbedNames returns the fixed scrub list's names, sorted.
func ScrubbedNames() []string {
	names := make([]string, 0, len(Scrubbed))
	for _, v := range Scrubbed {
		names = append(names, v.Name)
	}
	sort.Strings(names)
	return names
}

// ParseNames parses a comma-separated list of exact names: trimmed, empty
// entries and duplicates dropped.
func ParseNames(raw string) []string {
	var names []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// Resolved is the per-engine result of resolving the dynamic parts of the
// scrub: operator-chosen Hookdeck variable names and the opt-in.
type Resolved struct {
	// Extra are additional names to scrub (the Hookdeck variables named by
	// the effective config), already filtered through Protected.
	Extra []string
	// Admitted are scrubbed names the operator explicitly opted back in.
	Admitted []string
	// Ignored are opt-in entries that are not on the scrub list (including
	// Protected names): they are not admitted and have no effect.
	Ignored []string
	// Dropped are operator-chosen names refused by Protected.
	Dropped []string
}

// neverAdmitted names control variables the opt-in can never re-admit: the
// opt-in itself and the ADR-1346 Anthropic control pair (which the engine
// translates and removes separately).
func neverAdmitted(n string) bool {
	return n == OptInVar || n == "FABRIK_ANTHROPIC_API_KEY" || n == "FABRIK_ANTHROPIC_ENV_PASSTHROUGH"
}

// Resolve computes the dynamic scrub inputs. hookdeckNames are the effective
// FABRIK_HOOKDECK_*_ENV values; optIn is the raw OptInVar value.
func Resolve(hookdeckNames []string, optIn string) Resolved {
	var r Resolved
	extraSet := map[string]bool{}
	for _, n := range hookdeckNames {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if Protected(n) {
			r.Dropped = append(r.Dropped, n)
			continue
		}
		if !IsScrubbed(n) && !extraSet[n] {
			extraSet[n] = true
			r.Extra = append(r.Extra, n)
		}
	}
	for _, n := range ParseNames(optIn) {
		if Protected(n) || neverAdmitted(n) || !(IsScrubbed(n) || extraSet[n]) {
			r.Ignored = append(r.Ignored, n)
			continue
		}
		r.Admitted = append(r.Admitted, n)
	}
	return r
}

// Sentinels returns bare mergeEnv removal-sentinel entries (a key with no "=")
// for every scrubbed name — the fixed list, the Hookdeck defaults and r.Extra —
// except those the operator admitted. Emitted unconditionally, regardless of
// presence in the base env: a sentinel for an absent key is a harmless no-op.
func Sentinels(r Resolved) []string {
	admitted := map[string]bool{}
	for _, n := range r.Admitted {
		admitted[n] = true
	}
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if admitted[n] || seen[n] || Protected(n) {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, v := range Scrubbed {
		add(v.Name)
	}
	add(DefaultHookdeckWebhookSecretEnv)
	for _, n := range r.Extra {
		add(n)
	}
	return out
}
