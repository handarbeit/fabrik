package stages

import "embed"

// DefaultStages holds the embedded default stage YAML files from examples/.
// These are extracted by `fabrik init` into .fabrik/stages/ for new projects.
//
//go:embed examples/*.yaml
var DefaultStages embed.FS
