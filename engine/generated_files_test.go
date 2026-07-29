package engine

import (
	"reflect"
	"testing"
)

func TestClassifyConflictedPaths(t *testing.T) {
	specs := []generatedFileSpec{
		{Path: "docs/llms-full.txt", Command: []string{"bash", "scripts/generate-llms-full.sh"}},
		{Path: "docs/other-generated.txt", Command: []string{"bash", "scripts/generate-other.sh"}},
	}

	tests := []struct {
		name                 string
		paths                []conflictedPath
		wantMatched          []generatedFileSpec
		wantNonGenerated     []string
		wantDeletionExcluded []string
	}{
		{
			name:             "all generated",
			paths:            []conflictedPath{{Path: "docs/llms-full.txt", Status: "UU"}},
			wantMatched:      []generatedFileSpec{specs[0]},
			wantNonGenerated: nil,
		},
		{
			name:             "all non-generated",
			paths:            []conflictedPath{{Path: "engine/merge_train.go", Status: "UU"}, {Path: "engine/item.go", Status: "UU"}},
			wantMatched:      nil,
			wantNonGenerated: []string{"engine/merge_train.go", "engine/item.go"},
		},
		{
			name:             "mixed",
			paths:            []conflictedPath{{Path: "docs/llms-full.txt", Status: "UU"}, {Path: "docs/state-machine.md", Status: "UU"}},
			wantMatched:      []generatedFileSpec{specs[0]},
			wantNonGenerated: []string{"docs/state-machine.md"},
		},
		{
			name:             "empty input",
			paths:            nil,
			wantMatched:      nil,
			wantNonGenerated: nil,
		},
		{
			name:             "both generated specs matched",
			paths:            []conflictedPath{{Path: "docs/llms-full.txt", Status: "UU"}, {Path: "docs/other-generated.txt", Status: "AA"}},
			wantMatched:      []generatedFileSpec{specs[0], specs[1]},
			wantNonGenerated: nil,
		},
		{
			name:             "duplicate matched path only counted once",
			paths:            []conflictedPath{{Path: "docs/llms-full.txt", Status: "UU"}, {Path: "docs/llms-full.txt", Status: "UU"}},
			wantMatched:      []generatedFileSpec{specs[0]},
			wantNonGenerated: nil,
		},
		{
			name:                 "both-deleted generated path routes to Claude instead of regenerating",
			paths:                []conflictedPath{{Path: "docs/llms-full.txt", Status: "DD"}},
			wantMatched:          nil,
			wantNonGenerated:     []string{"docs/llms-full.txt"},
			wantDeletionExcluded: []string{"docs/llms-full.txt"},
		},
		{
			name:                 "deleted-by-them generated path routes to Claude instead of regenerating",
			paths:                []conflictedPath{{Path: "docs/llms-full.txt", Status: "UD"}},
			wantMatched:          nil,
			wantNonGenerated:     []string{"docs/llms-full.txt"},
			wantDeletionExcluded: []string{"docs/llms-full.txt"},
		},
		{
			name:                 "deleted-by-us generated path routes to Claude instead of regenerating",
			paths:                []conflictedPath{{Path: "docs/llms-full.txt", Status: "DU"}},
			wantMatched:          nil,
			wantNonGenerated:     []string{"docs/llms-full.txt"},
			wantDeletionExcluded: []string{"docs/llms-full.txt"},
		},
		{
			name:                 "deletion-involving generated path alongside a clean generated match",
			paths:                []conflictedPath{{Path: "docs/llms-full.txt", Status: "DD"}, {Path: "docs/other-generated.txt", Status: "UU"}},
			wantMatched:          []generatedFileSpec{specs[1]},
			wantNonGenerated:     []string{"docs/llms-full.txt"},
			wantDeletionExcluded: []string{"docs/llms-full.txt"},
		},
		{
			name:                 "duplicate deletion-involving path only counted once in deletionExcluded",
			paths:                []conflictedPath{{Path: "docs/llms-full.txt", Status: "DD"}, {Path: "docs/llms-full.txt", Status: "DD"}},
			wantMatched:          nil,
			wantNonGenerated:     []string{"docs/llms-full.txt", "docs/llms-full.txt"},
			wantDeletionExcluded: []string{"docs/llms-full.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMatched, gotNonGenerated, gotDeletionExcluded := classifyConflictedPaths(specs, tt.paths)
			if !reflect.DeepEqual(gotMatched, tt.wantMatched) {
				t.Errorf("matched = %+v, want %+v", gotMatched, tt.wantMatched)
			}
			if !reflect.DeepEqual(gotNonGenerated, tt.wantNonGenerated) {
				t.Errorf("nonGenerated = %+v, want %+v", gotNonGenerated, tt.wantNonGenerated)
			}
			if !reflect.DeepEqual(gotDeletionExcluded, tt.wantDeletionExcluded) {
				t.Errorf("deletionExcluded = %+v, want %+v", gotDeletionExcluded, tt.wantDeletionExcluded)
			}
		})
	}
}

// TestClassifyConflictedPaths_DuplicatePathDifferentCommand guards against a static
// config mistake: if specs ever declared the same Path twice with different Commands,
// running both commands against the same file would make the final content silently
// depend on declaration order. classifyConflictedPaths must pick the first declaration
// deterministically rather than returning both.
func TestClassifyConflictedPaths_DuplicatePathDifferentCommand(t *testing.T) {
	first := generatedFileSpec{Path: "docs/llms-full.txt", Command: []string{"bash", "scripts/generate-llms-full.sh"}}
	second := generatedFileSpec{Path: "docs/llms-full.txt", Command: []string{"bash", "scripts/some-other-generator.sh"}}
	specs := []generatedFileSpec{first, second}

	matched, nonGenerated, _ := classifyConflictedPaths(specs, []conflictedPath{{Path: "docs/llms-full.txt", Status: "UU"}})
	if nonGenerated != nil {
		t.Errorf("nonGenerated = %+v, want nil", nonGenerated)
	}
	want := []generatedFileSpec{first}
	if !reflect.DeepEqual(matched, want) {
		t.Errorf("matched = %+v, want only the first declared spec %+v", matched, want)
	}
}

func TestDeletionInvolvingStatus(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"DD", true},
		{"UD", true},
		{"DU", true},
		{"UU", false},
		{"AA", false},
		{"AU", false},
		{"UA", false},
	}
	for _, tt := range tests {
		if got := deletionInvolvingStatus(tt.status); got != tt.want {
			t.Errorf("deletionInvolvingStatus(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestDeclaredGeneratedFiles(t *testing.T) {
	if len(generatedFiles) != 1 {
		t.Fatalf("expected exactly one declared generated file today, got %d", len(generatedFiles))
	}
	spec := generatedFiles[0]
	if spec.Path != "docs/llms-full.txt" {
		t.Errorf("Path = %q, want docs/llms-full.txt", spec.Path)
	}
	wantCmd := []string{"bash", "scripts/generate-llms-full.sh"}
	if !reflect.DeepEqual(spec.Command, wantCmd) {
		t.Errorf("Command = %v, want %v", spec.Command, wantCmd)
	}
}
