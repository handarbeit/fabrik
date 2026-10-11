package cmd

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/handarbeit/fabrik/warnings"
)

// warnUnrecognizedConfigKeys prints one [startup] warning line per key in
// keys, naming the offending .fabrik/config.yaml key. When a key
// mechanically resolves to a real, currently-registered CLI flag — via
// Fabrik's consistent snake_case (config.yaml) <-> kebab-case (flag) <->
// FABRIK_SCREAMING_SNAKE_CASE (env var) naming convention — the warning also
// names the flag/env var to use instead.
//
// The flag/env suggestion is derived mechanically and verified live against
// flag.CommandLine.Lookup rather than a hand-maintained table: cmd/root.go
// always calls flag.CommandLine.Parse before config.LoadProjectConfig runs,
// so every real CLI flag is already registered by the time this is called.
// A key with no matching registered flag (a pure typo, or a genuinely
// env-only knob such as FABRIK_ANTHROPIC_API_KEY) gets only the base line.
//
// keys is expected to already be sorted (UnrecognizedTopLevelKeys returns a
// sorted slice); this function reports them in the order given.
func warnUnrecognizedConfigKeys(keys []string, w io.Writer) {
	conds := make([]warnings.Condition, 0, len(keys))
	for _, key := range keys {
		detail := fmt.Sprintf("%q in .fabrik/config.yaml is not a recognised key and is silently ignored.", key)
		logLine := fmt.Sprintf(".fabrik/config.yaml: unrecognised key %q", key)

		flagName := strings.ReplaceAll(key, "_", "-")
		if flag.CommandLine.Lookup(flagName) != nil {
			envName := "FABRIK_" + strings.ToUpper(key)
			hint := fmt.Sprintf("this knob is CLI/env-only — set %s or -%s", envName, flagName)
			detail += "\n\nFix: " + hint + "."
			logLine += fmt.Sprintf("\n          (%s)", hint)
		} else {
			detail += "\n\nFix: remove the key or correct its spelling."
		}
		conds = append(conds, warnings.Condition{
			Subject: key,
			Title:   fmt.Sprintf("Unrecognised key %q in .fabrik/config.yaml", key),
			Detail:  detail,
			LogLine: logLine,
		})
	}
	// keys is the complete result of a check that ran, so Reconcile also clears
	// the entry for a key the operator has since fixed — across a restart.
	if err := warnings.Reconcile(w, "config_key", conds); err != nil {
		fmt.Fprintf(w, "[startup] warning: recording config_key warnings: %v\n", err)
	}
}
