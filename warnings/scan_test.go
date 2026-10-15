package warnings_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// FR-005 (#2135): an operator-level failure must reach the warnings panel, not
// only the log. warnings.Report/Reconcile log and record in one call, so a
// condition routed through them leaves no literal behind; this scan fails on
// any remaining operator-level warning literal in non-test source that is not
// on the audited list below, so a new one cannot reach only the log by accident.
//
// Scope — the literals that mean "operator-level warning" at startup or in the
// engine's own wording: "[startup] warn…", "[warn]", "[upgrade] warning" and
// "WARNING:". Out of scope by definition: per-item warn-level lines
// (logf(item.Number, "warn", …) — per-item states, FR-001), runtime poll-time
// lines (poll errors, rate limits — transient, shown by the header's backoff
// status), and the interactive one-shot subcommands in skippedFiles, whose
// output is the operator's whole interface.
//
// To add an entry: it needs a substring that identifies the line and a reason
// that says why the panel is wrong for it (transient, per-item, store failing,
// already surfaced elsewhere). If the condition blocks or degrades work and a
// person must act, route it through warnings.Report/Reconcile instead.

var operatorWarnLiteral = regexp.MustCompile(
	`\[startup\] warn|\[warn\]|\[upgrade\] warning|WARNING:|"startup", "warning:`)

// scannedDirs are the daemon's own source trees (plus cmd's startup path). The
// shared internal packages and Pruefer are separate products with their own
// surfaces.
var scannedDirs = []string{"engine", "cmd", "config", "stages"}

// skippedFiles are interactive one-shot subcommands: the operator is reading
// their stderr as they run, so a panel entry would only linger after the fact.
var skippedFiles = map[string]string{
	"cmd/init.go":         "interactive one-shot subcommand",
	"cmd/upgrade.go":      "interactive one-shot subcommand (its customization notice is re-reported by the daemon's plugin_state entry)",
	"cmd/watch.go":        "read-only client, not the daemon",
	"cmd/repair_board.go": "interactive one-shot subcommand",
}

type logOnly struct {
	file      string // path relative to the repo root
	substring string // identifies the line(s)
	reason    string
}

var logOnlyExceptions = []logOnly{
	// The shared path itself.
	{"cmd/config_fallbacks.go", `"[warn] %s\n"`, "the collector prints each fallback line; reportConfigFallbacks records them as one config_value entry right after resolution"},
	{"cmd/config_fallbacks.go", `[startup] warning: recording config_value`, "the panel store itself is failing"},
	{"cmd/config_fallbacks.go", `[startup] warning: recording plugin_state`, "the panel store itself is failing"},
	{"cmd/config_key_warn.go", `[startup] warning: recording config_key`, "the panel store itself is failing"},
	{"cmd/root.go", `[startup] warning: recording config_token`, "the panel store itself is failing"},
	{"cmd/root.go", `[startup] warning: checking .fabrik/config.yaml for unrecognized keys`, "the check could not run, so nothing is known about the keys; reconciling would wrongly clear real entries"},
	{"config/config.go", `[startup] warning: recording config_ignored`, "the panel store itself is failing"},
	{"engine/poll.go", `[startup] warning: recording log_file`, "the panel store itself is failing"},
	{"engine/startup.go", `[startup] warning: recording worker_env`, "the panel store itself is failing"},
	{"stages/drift.go", `[startup] warning: clearing`, "the panel store itself is failing"},
	{"stages/drift.go", `[startup] warning: recording`, "the panel store itself is failing"},
	{"stages/drift.go", `[startup] warning: stale %s warning sweep failed`, "the panel store itself is failing"},

	// Text builders for the plugin_state:customized entry itself.
	{"cmd/config_fallbacks.go", `TrimPrefix(r.CustomizedMsg`, "strips the prefix from the text recorded as the plugin_state:customized entry"},
	{"cmd/root.go", `msg := "[upgrade] warning: plugin skills have local customizations`, "builds the text of the plugin_state:customized entry, which Execute reports"},

	// Re-exec path: the same facts are re-evaluated and reported to the panel
	// by Execute's reportPluginState right afterwards.
	{"cmd/root.go", `plugin state check failed%s`, "re-exec path; Execute re-evaluates and reports plugin_state"},
	{"cmd/root.go", `RefreshPlugin failed%s`, "re-exec path; a failed refresh leaves the plugin stale, which the next startup reports as plugin_state"},
	{"cmd/root.go", `writing installed version failed%s`, "re-exec path; same as above"},
	{"cmd/root.go", `plugin skill check failed`, "only the reporting-only stale-file count is lost; customization/upgrade gating is unaffected"},

	// Transient or advisory.
	{"engine/engine.go", `could not list GitHub App installation`, "fail-open; the confirmed per-repo result surfaces as repo_access"},
	{"engine/app_identity.go", `could not resolve the user ID`, "cosmetic and fail-soft (commits use the ID-less email)"},
	{"engine/startup.go", `could not fetch project board for startup check`, "transient read failure; the column check is retried at the next start and the poll loop surfaces a persistent failure"},
	{"engine/startup.go", `project board has no ID`, "transient; the column check is retried at the next start"},
	{"engine/startup.go", `could not fetch status field for startup check`, "transient read failure; retried at the next start"},
	{"engine/startup.go", `board has columns with no matching stage`, "extra columns neither block nor degrade work"},
	{"engine/startup.go", `board drift detected`, "per-item state (FR-001), not operator-level"},
	{"engine/startup.go", `has no matching board column`, "holding stage with merge_train off: nothing is queued there, so no work is blocked or degraded"},
	{"engine/webhook.go", `hook-coverage check failed for`, "transient lookup error: logged only and deliberately kept out of webhook_coverage (an unknown repo is not a missing one; a prior entry is left untouched)"},
	{"engine/webhook.go", `orphan webhook cleanup failed`, "transient cleanup; proceeding is safe"},
	{"engine/webhook.go", `projects_v2_item may have caused subprocess crash`, "runtime webhook-subprocess health is surfaced by the webhook ingestion status in the header"},
	{"engine/webhook.go", `webhook subscription permanently failed`, "runtime webhook-subprocess health is surfaced by the webhook ingestion status in the header"},
	{"engine/webhook.go", `webhook subscription for repo`, "runtime webhook-subprocess health is surfaced by the webhook ingestion status in the header"},
	{"engine/webhook.go", `projects_v2_item event not supported`, "informational capability note; the board is polled regardless"},
	{"engine/hookdeck.go", `installation-repo-coverage check failed`, "transient read failure; the prior coverage entry is deliberately left in place"},
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd) // warnings/ -> repo root
}

func TestOperatorWarningsReachThePanel(t *testing.T) {
	root := repoRoot(t)
	used := make([]bool, len(logOnlyExceptions))

	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			p := filepath.Join(dir, name)
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if e.IsDir() {
				walk(p)
				continue
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if _, skip := skippedFiles[rel]; skip {
				continue
			}
			if rel == "engine/warn_writer.go" { // the Report log adapter, not a warning source
				continue
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				trim := strings.TrimSpace(line)
				if strings.HasPrefix(trim, "//") || !operatorWarnLiteral.MatchString(line) {
					continue
				}
				ok := false
				for j, ex := range logOnlyExceptions {
					if ex.file == rel && strings.Contains(line, ex.substring) {
						used[j] = true
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s:%d: operator-level warning literal reaches only the log:\n\t%s\n"+
						"Route it through warnings.Report/Reconcile (docs/state-machine.md §7.11), or add it to "+
						"logOnlyExceptions with a reason.", rel, i+1, trim)
				}
			}
		}
	}
	for _, d := range scannedDirs {
		walk(filepath.Join(root, d))
	}

	for j, ex := range logOnlyExceptions {
		if !used[j] {
			t.Errorf("stale log-only exception (no matching line): %s %q — remove it", ex.file, ex.substring)
		}
		if strings.TrimSpace(ex.reason) == "" {
			t.Errorf("log-only exception %s %q has no reason", ex.file, ex.substring)
		}
	}
}

// FR-004 (#2218): the scan must catch a warning written through the engine's
// logf as well as the bracketed literal. These are the shapes it has to flag
// (and one it must leave alone), so a regex edit cannot silently narrow it.
func TestOperatorWarnLiteralCatchesLogfShape(t *testing.T) {
	for line, want := range map[string]bool{
		`e.logf(0, "startup", "warning: could not look up the %q field: %v\n", n, err)`: true,
		`e.logf(0, "startup", "warning: project board has no ID\n")`:                    true,
		`fmt.Fprintf(w, "[startup] warning: x\n")`:                                      true,
		`e.logf(0, "startup", "WARNING: x\n")`:                                          true,
		`e.logf(0, "startup", "status-line field unavailable — %s\n", why)`:             false,
		`e.logf(item.Number, "warn", "per-item state\n")`:                               false,
	} {
		if got := operatorWarnLiteral.MatchString(line); got != want {
			t.Errorf("operatorWarnLiteral.MatchString(%q) = %v, want %v", line, got, want)
		}
	}
}
