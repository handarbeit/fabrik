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

// deletionInvolvingStatus reports whether a git status --porcelain conflict code
// involves a deletion on at least one side (DD: both deleted, UD: deleted by them,
// DU: deleted by us). A generated path conflicted this way carries deletion intent
// from at least one contributor — regenerating it would silently recreate a file one
// or both sides meant to remove, which is a different question than "these two
// versions of the content disagree" and must not be resolved by rerunning the
// generator. AU/UA (add/add-style) and AA/UU conflicts carry no deletion intent and
// are eligible for regeneration as before.
//
// This code list is a deliberate subset of unmergedPaths's own seven-code unmerged
// list (merge_train.go, the "UU"/"AA"/"DD"/"AU"/"UD"/"UA"/"DU" check) — if that list
// ever grows (e.g. a future git version introduces a new unmerged status), review
// whether the new code involves a deletion and, if so, add it here too. A code that
// should be deletion-involving but is missing from this function would silently fall
// through to regeneration instead of Claude.
func deletionInvolvingStatus(status string) bool {
	return status == "DD" || status == "UD" || status == "DU"
}

// classifyConflictedPaths splits a set of conflicted paths against the declared
// generated-file specs. matched holds the subset of specs whose Path appears in paths
// with a non-deletion-involving status (deduplicated by spec, order-stable per specs),
// nonGenerated holds every conflicted path that is not covered by any spec, plus any
// generated path whose conflict involves a deletion (order-stable per paths) — see
// deletionInvolvingStatus — and deletionExcluded holds just that latter subset: declared
// generated paths excluded from matched specifically because of a deletion-involving
// status (order-stable per paths, no duplicates).
//
// Routing a deletion-involving generated-path conflict to nonGenerated sends it to
// Claude for textual resolution instead of regeneration, preserving whichever side's
// deletion intent Claude judges correct rather than silently recreating a file a
// contributor meant to remove. deletionExcluded exists as a separate, narrower signal
// for regenerateAndCommit's shared-command staging: a command that regenerates matched
// path A as a side effect may also regenerate a declared sibling path B on disk even
// though B isn't in matched — if B is in deletionExcluded, that side effect must be
// discarded rather than staged, or regeneration would silently overwrite Claude's
// deletion-aware resolution of B by way of a command it happens to share with A.
//
// If specs ever declares the same Path twice with different Commands (a static
// config mistake, not user input — see TestDeclaredGeneratedFiles), only the first
// declaration for that Path is included in matched: running every command sharing a
// Path against the same file would make the final content depend on declaration
// order, which is worse than picking one deterministically.
func classifyConflictedPaths(specs []generatedFileSpec, paths []conflictedPath) (matched []generatedFileSpec, nonGenerated []string, deletionExcluded []string) {
	generatedPathSet := make(map[string]bool, len(specs))
	for _, spec := range specs {
		generatedPathSet[spec.Path] = true
	}

	matchedPathSet := make(map[string]bool, len(paths))
	seenDeletionExcluded := make(map[string]bool, len(paths))
	for _, p := range paths {
		if !generatedPathSet[p.Path] {
			nonGenerated = append(nonGenerated, p.Path)
			continue
		}
		if deletionInvolvingStatus(p.Status) {
			nonGenerated = append(nonGenerated, p.Path)
			if !seenDeletionExcluded[p.Path] {
				seenDeletionExcluded[p.Path] = true
				deletionExcluded = append(deletionExcluded, p.Path)
			}
			continue
		}
		matchedPathSet[p.Path] = true
	}

	seenPaths := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if matchedPathSet[spec.Path] && !seenPaths[spec.Path] {
			seenPaths[spec.Path] = true
			matched = append(matched, spec)
		}
	}

	return matched, nonGenerated, deletionExcluded
}
