package engine

// generatedFileSpec declares a single generated artefact: a path that must never be
// textually merged, and the command that regenerates it from its (already-merged)
// sources. The Command slice is passed directly to exec.Command(Command[0], Command[1:]...).
type generatedFileSpec struct {
	Path    string
	Command []string
}

// generatedFiles is the single declared mapping of generated path -> regeneration command
// (FR-3). Adding a new generated file means adding an entry here — nothing in the
// conflict-resolution dispatch logic needs to change.
var generatedFiles = []generatedFileSpec{
	{
		Path:    "docs/llms-full.txt",
		Command: []string{"bash", "scripts/generate-llms-full.sh"},
	},
}

// generatedFileSet returns the effective generated-file mapping: the test override
// when set, otherwise the package-level generatedFiles used in production.
func (e *Engine) generatedFileSet() []generatedFileSpec {
	if e.generatedFilesOverride != nil {
		return e.generatedFilesOverride
	}
	return generatedFiles
}

// classifyConflictedPaths splits a set of conflicted paths against the declared
// generated-file specs. matched holds the subset of specs whose Path appears in paths
// (deduplicated by spec, order-stable per specs), and nonGenerated holds every
// conflicted path that is not covered by any spec (order-stable per paths).
func classifyConflictedPaths(specs []generatedFileSpec, paths []string) (matched []generatedFileSpec, nonGenerated []string) {
	generatedPathSet := make(map[string]bool, len(specs))
	for _, spec := range specs {
		generatedPathSet[spec.Path] = true
	}

	pathSet := make(map[string]bool, len(paths))
	for _, path := range paths {
		pathSet[path] = true
		if !generatedPathSet[path] {
			nonGenerated = append(nonGenerated, path)
		}
	}

	for _, spec := range specs {
		if pathSet[spec.Path] {
			matched = append(matched, spec)
		}
	}

	return matched, nonGenerated
}
