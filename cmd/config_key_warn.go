package cmd

import (
	"flag"
	"fmt"
	"io"
	"strings"
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
	for _, key := range keys {
		fmt.Fprintf(w, "[startup] warning: .fabrik/config.yaml: unrecognised key %q\n", key)

		flagName := strings.ReplaceAll(key, "_", "-")
		if flag.CommandLine.Lookup(flagName) == nil {
			continue
		}
		envName := "FABRIK_" + strings.ToUpper(key)
		fmt.Fprintf(w, "          (this knob is CLI/env-only — set %s or -%s)\n", envName, flagName)
	}
}
