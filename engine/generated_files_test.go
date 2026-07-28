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
		name             string
		paths            []string
		wantMatched      []generatedFileSpec
		wantNonGenerated []string
	}{
		{
			name:             "all generated",
			paths:            []string{"docs/llms-full.txt"},
			wantMatched:      []generatedFileSpec{specs[0]},
			wantNonGenerated: nil,
		},
		{
			name:             "all non-generated",
			paths:            []string{"engine/merge_train.go", "engine/item.go"},
			wantMatched:      nil,
			wantNonGenerated: []string{"engine/merge_train.go", "engine/item.go"},
		},
		{
			name:             "mixed",
			paths:            []string{"docs/llms-full.txt", "docs/state-machine.md"},
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
			paths:            []string{"docs/llms-full.txt", "docs/other-generated.txt"},
			wantMatched:      []generatedFileSpec{specs[0], specs[1]},
			wantNonGenerated: nil,
		},
		{
			name:             "duplicate matched path only counted once",
			paths:            []string{"docs/llms-full.txt", "docs/llms-full.txt"},
			wantMatched:      []generatedFileSpec{specs[0]},
			wantNonGenerated: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMatched, gotNonGenerated := classifyConflictedPaths(specs, tt.paths)
			if !reflect.DeepEqual(gotMatched, tt.wantMatched) {
				t.Errorf("matched = %+v, want %+v", gotMatched, tt.wantMatched)
			}
			if !reflect.DeepEqual(gotNonGenerated, tt.wantNonGenerated) {
				t.Errorf("nonGenerated = %+v, want %+v", gotNonGenerated, tt.wantNonGenerated)
			}
		})
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
