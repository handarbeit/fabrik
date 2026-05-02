package plugin

import "embed"

// FabrikPlugin holds the embedded Fabrik plugin files.
// These are installed by `fabrik init` into ~/.claude/plugins/fabrik/.
//
//go:embed fabrik-workflows/.claude-plugin/plugin.json
//go:embed fabrik-workflows/README.md
//go:embed fabrik-workflows/skills/*/SKILL.md
var FabrikPlugin embed.FS
