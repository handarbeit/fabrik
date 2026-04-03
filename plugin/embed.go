package plugin

import "embed"

// FabrikPlugin holds the embedded Fabrik plugin files.
// These are installed by `fabrik init` into ~/.claude/plugins/fabrik/.
//
//go:embed fabrik-plugin/.claude-plugin/plugin.json
//go:embed fabrik-plugin/README.md
//go:embed fabrik-plugin/skills/*/SKILL.md
var FabrikPlugin embed.FS
