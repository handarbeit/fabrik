package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/handarbeit/fabrik/warnings"
)

// configFallbacks collects the "[warn] … is invalid; using default N" lines
// that Execute and its resolve helpers emit while resolving configuration
// (#2135). Each still prints its own line to stderr, as before; the collected
// set is then reported as ONE warnings-panel entry so a badly broken config
// cannot flood the panel (it has a 3-row window), and the entry clears on the
// next start once every setting is valid.
var configFallbacks struct {
	mu    sync.Mutex
	lines []string
}

// resetConfigFallbacks starts a fresh resolution pass (Execute may run more
// than once in a process under test).
func resetConfigFallbacks() {
	configFallbacks.mu.Lock()
	configFallbacks.lines = nil
	configFallbacks.mu.Unlock()
}

// warnConfigFallback prints the "[warn] …" line to stderr (unchanged operator
// output) and remembers it for reportConfigFallbacks.
func warnConfigFallback(format string, args ...any) {
	msg := strings.TrimSuffix(fmt.Sprintf(format, args...), "\n")
	fmt.Fprintf(os.Stderr, "[warn] %s\n", msg)
	configFallbacks.mu.Lock()
	configFallbacks.lines = append(configFallbacks.lines, msg)
	configFallbacks.mu.Unlock()
}

// configFallbackConditions turns the collected lines into the "config_value"
// panel conditions: none when every setting resolved cleanly.
func configFallbackConditions() []warnings.Condition {
	configFallbacks.mu.Lock()
	lines := append([]string(nil), configFallbacks.lines...)
	configFallbacks.mu.Unlock()
	if len(lines) == 0 {
		return nil
	}
	return []warnings.Condition{{
		Subject: "settings",
		Title:   fmt.Sprintf("%d setting(s) invalid — using defaults", len(lines)),
		Detail:  strings.Join(lines, "\n") + "\n\nFix: correct these values in the environment, flags or .fabrik/config.yaml and restart.",
	}}
}

// reportConfigFallbacks records (or, when there are none, clears) the single
// aggregated config_value entry. The lines were already printed one by one, so
// no log line is written here. The resolution pass always ran to completion by
// the time this is called, which is what makes the clearing sweep safe.
func reportConfigFallbacks() {
	if err := warnings.Reconcile(nil, "config_value", configFallbackConditions()); err != nil {
		fmt.Fprintf(os.Stderr, "[startup] warning: recording config_value warning: %v\n", err)
	}
}

// pluginStateReport is the outcome of startup's plugin-skill evaluation.
type pluginStateReport struct {
	// EvalErr is a failed CheckPluginState; RefreshErr a failed auto-refresh.
	EvalErr, RefreshErr error
	// RefreshSkipped means the auto-refresh was not attempted on this start
	// (TUI mode, or customizations skip it), so RefreshErr says nothing about
	// whether a previously recorded refresh failure still holds.
	RefreshSkipped bool
	Customized     bool
	// CustomizedMsg is the operator-facing customization text (already built by
	// pluginCustomizationWarning); empty when not Customized.
	CustomizedMsg string
	// MigrationPending means `fabrik upgrade` has a workflow migration to apply
	// (#2139); MigrationDetail says what.
	MigrationPending bool
	MigrationDetail  string
}

// reportPluginState records the plugin conditions that block or degrade work —
// a failed state check, a failed auto-refresh, and local customizations that
// skip auto-refresh — and clears the ones that no longer hold. w is nil under
// the TUI (stderr output would corrupt the screen); the panel entry is written
// either way. The state evaluation always completed (a failure is itself a
// condition), so the sweep is safe for check_failed and customized; the
// refresh_failed subject is swept only when the refresh was actually attempted
// — otherwise a failure recorded by an earlier start is left in place.
func reportPluginState(w io.Writer, r pluginStateReport) {
	var conds []warnings.Condition
	if r.EvalErr != nil {
		conds = append(conds, warnings.Condition{
			Subject: "check_failed",
			Title:   "Plugin state check failed",
			Detail:  fmt.Sprintf("Could not evaluate .fabrik/plugin: %v\n\nFix: run 'fabrik upgrade' and inspect the error.", r.EvalErr),
		})
	}
	if r.RefreshErr != nil {
		conds = append(conds, warnings.Condition{
			Subject: "refresh_failed",
			Title:   "Plugin skill refresh failed",
			Detail:  fmt.Sprintf("Could not refresh the stale plugin skills: %v\n\nFix: run 'fabrik upgrade' and inspect the error.", r.RefreshErr),
		})
	}
	if r.Customized {
		msg := strings.TrimSpace(strings.TrimPrefix(r.CustomizedMsg, "[upgrade] warning: "))
		conds = append(conds, warnings.Condition{
			Subject: "customized",
			Title:   "Plugin skills have local customizations — auto-refresh skipped",
			Detail:  msg + "\n\nLocal edits are not discarded. They block plugin updates until `fabrik upgrade` moves them into .fabrik/plugins/local-workflows.",
			LogLine: msg,
		})
	}
	if r.MigrationPending {
		conds = append(conds, warnings.Condition{
			Subject: "migration_pending",
			Title:   "Workflow migration pending — run 'fabrik upgrade'",
			Detail:  r.MigrationDetail + "\n\nFix: run 'fabrik upgrade', then review and commit the result. The daemon keeps working on the current layout until you do.",
		})
	}
	var err error
	if r.RefreshSkipped && r.RefreshErr == nil {
		present := map[string]bool{"refresh_failed": true}
		for i := range conds {
			conds[i].Type = "plugin_state"
			present[conds[i].Subject] = true
			if rerr := warnings.Report(w, conds[i]); rerr != nil && err == nil {
				err = rerr
			}
		}
		if err == nil {
			_, err = warnings.ClearMissing("plugin_state", present)
		}
	} else {
		err = warnings.Reconcile(w, "plugin_state", conds)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[startup] warning: recording plugin_state warning: %v\n", err)
	}
}
