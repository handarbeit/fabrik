package plugin

import "embed"

// FabrikPlugin holds the embedded fabrik-workflows plugin files (the worker-side
// plugin loaded into the Claude Code workers the Fabrik engine spawns to execute
// pipeline stages).
//
// `fabrik init` extracts these files to .fabrik/plugin/ in the current project,
// and `fabrik upgrade` refreshes that directory from the embedded copy — after
// first moving any operator-edited copy to .fabrik/plugins/local-workflows
// (#2139, ADR 2139), so the base is always Fabrik's own. The engine then passes
// `--plugin-dir .fabrik/plugin/` (plus local-workflows when it exists, and one
// `--plugin-dir` per configured `plugin_dirs` entry, ADR 2136) to every worker
// invocation.
// This is distinct from the user-facing `fabrik` plugin (plugin/fabrik/) which
// is installed via Claude Code's plugin marketplace, not embedded.
//
//go:embed fabrik-workflows/.claude-plugin/plugin.json
//go:embed fabrik-workflows/README.md
//go:embed fabrik-workflows/LABELS.md
//go:embed fabrik-workflows/skills/*/SKILL.md
var FabrikPlugin embed.FS
