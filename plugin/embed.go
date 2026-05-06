package plugin

import "embed"

// FabrikPlugin holds the embedded fabrik-workflows plugin files (the worker-side
// plugin loaded into the Claude Code workers the Fabrik engine spawns to execute
// pipeline stages).
//
// `fabrik init` extracts these files to .fabrik/plugin/ in the current project,
// and `fabrik upgrade` overwrites that directory from the embedded copy. The
// engine then passes `--plugin-dir .fabrik/plugin/` to every worker invocation.
// This is distinct from the user-facing `fabrik` plugin (plugin/fabrik/) which
// is installed via Claude Code's plugin marketplace, not embedded.
//
//go:embed fabrik-workflows/.claude-plugin/plugin.json
//go:embed fabrik-workflows/README.md
//go:embed fabrik-workflows/skills/*/SKILL.md
var FabrikPlugin embed.FS
