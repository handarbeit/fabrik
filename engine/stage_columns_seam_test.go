package engine

import (
	"reflect"
	"testing"

	"github.com/handarbeit/fabrik/stages"
)

func TestStartupRequiredColumnsForTest(t *testing.T) {
	set := []*stages.Stage{
		{Name: "Backlog", Order: 0, Unmanaged: true},
		{Name: "Specify", Order: 1},
		{Name: "Validate", Order: 2},
		{Name: "Queued", Order: 3, HoldingStage: true},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
	got, err := StartupRequiredColumnsForTest(set)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Specify", "Validate", "Queued"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
}
