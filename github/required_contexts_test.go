package github

import (
	"reflect"
	"testing"
)

func TestClassifyRequiredContexts_Unconfigured_Satisfied(t *testing.T) {
	status, missing, pending, failed := ClassifyRequiredContexts(nil, nil, nil)
	if status != RequiredContextsSatisfied {
		t.Errorf("expected RequiredContextsSatisfied for unconfigured repo, got %v", status)
	}
	if len(missing) != 0 || len(pending) != 0 || len(failed) != 0 {
		t.Errorf("expected no missing/pending/failed, got missing=%v pending=%v failed=%v", missing, pending, failed)
	}
}

func TestClassifyRequiredContexts_CheckRunSuccess_Satisfied(t *testing.T) {
	status, _, _, _ := ClassifyRequiredContexts(
		[]string{"ci/test"},
		[]CheckRun{{ID: 1, Name: "ci/test", Status: "completed", Conclusion: "success"}},
		nil,
	)
	if status != RequiredContextsSatisfied {
		t.Errorf("expected RequiredContextsSatisfied, got %v", status)
	}
}

// The core #933 regression: an all-skipped/all-neutral check-run set must
// NOT satisfy a required context, even though ClassifyCheckRuns alone would
// call this "ready".
func TestClassifyRequiredContexts_CheckRunSkippedOrNeutral_Pending(t *testing.T) {
	status, _, pending, _ := ClassifyRequiredContexts(
		[]string{"ci/test", "ci/lint"},
		[]CheckRun{
			{ID: 1, Name: "ci/test", Status: "completed", Conclusion: "skipped"},
			{ID: 2, Name: "ci/lint", Status: "completed", Conclusion: "neutral"},
		},
		nil,
	)
	if status != RequiredContextsPending {
		t.Errorf("expected RequiredContextsPending for skipped/neutral required checks, got %v", status)
	}
	if len(pending) != 2 {
		t.Errorf("expected both required names pending, got %v", pending)
	}
}

func TestClassifyRequiredContexts_MissingEntirely_Pending(t *testing.T) {
	status, missing, _, _ := ClassifyRequiredContexts(
		[]string{"fantasy/local-test"},
		nil,
		nil,
	)
	if status != RequiredContextsPending {
		t.Errorf("expected RequiredContextsPending for a required context with no producer at all, got %v", status)
	}
	if !reflect.DeepEqual(missing, []string{"fantasy/local-test"}) {
		t.Errorf("expected missing=[fantasy/local-test], got %v", missing)
	}
}

func TestClassifyRequiredContexts_CommitStatusSuccess_Satisfied(t *testing.T) {
	status, _, _, _ := ClassifyRequiredContexts(
		[]string{"fantasy/local-test"},
		nil,
		[]CommitStatus{{Context: "fantasy/local-test", State: "success"}},
	)
	if status != RequiredContextsSatisfied {
		t.Errorf("expected RequiredContextsSatisfied via commit status, got %v", status)
	}
}

func TestClassifyRequiredContexts_CommitStatusPending_Pending(t *testing.T) {
	status, _, pending, _ := ClassifyRequiredContexts(
		[]string{"fantasy/local-test"},
		nil,
		[]CommitStatus{{Context: "fantasy/local-test", State: "pending"}},
	)
	if status != RequiredContextsPending {
		t.Errorf("expected RequiredContextsPending, got %v", status)
	}
	if !reflect.DeepEqual(pending, []string{"fantasy/local-test"}) {
		t.Errorf("expected pending=[fantasy/local-test], got %v", pending)
	}
}

func TestClassifyRequiredContexts_CommitStatusFailure_Failed(t *testing.T) {
	status, _, _, failed := ClassifyRequiredContexts(
		[]string{"fantasy/local-test"},
		nil,
		[]CommitStatus{{Context: "fantasy/local-test", State: "failure"}},
	)
	if status != RequiredContextsFailed {
		t.Errorf("expected RequiredContextsFailed, got %v", status)
	}
	if !reflect.DeepEqual(failed, []string{"fantasy/local-test"}) {
		t.Errorf("expected failed=[fantasy/local-test], got %v", failed)
	}
}

func TestClassifyRequiredContexts_CommitStatusError_Failed(t *testing.T) {
	status, _, _, _ := ClassifyRequiredContexts(
		[]string{"fantasy/local-test"},
		nil,
		[]CommitStatus{{Context: "fantasy/local-test", State: "error"}},
	)
	if status != RequiredContextsFailed {
		t.Errorf("expected RequiredContextsFailed for state=error, got %v", status)
	}
}

func TestClassifyRequiredContexts_CheckRunFailure_Failed(t *testing.T) {
	status, _, _, failed := ClassifyRequiredContexts(
		[]string{"ci/test"},
		[]CheckRun{{ID: 1, Name: "ci/test", Status: "completed", Conclusion: "failure"}},
		nil,
	)
	if status != RequiredContextsFailed {
		t.Errorf("expected RequiredContextsFailed, got %v", status)
	}
	if !reflect.DeepEqual(failed, []string{"ci/test"}) {
		t.Errorf("expected failed=[ci/test], got %v", failed)
	}
}

// A stale failed run superseded by a fresh (higher-ID) success rerun of the
// same check name must resolve via the latest run, mirroring ClassifyCheckRuns.
func TestClassifyRequiredContexts_LatestCheckRunByID_Wins(t *testing.T) {
	status, _, _, _ := ClassifyRequiredContexts(
		[]string{"ci/test"},
		[]CheckRun{
			{ID: 1, Name: "ci/test", Status: "completed", Conclusion: "failure"},
			{ID: 2, Name: "ci/test", Status: "completed", Conclusion: "success"},
		},
		nil,
	)
	if status != RequiredContextsSatisfied {
		t.Errorf("expected RequiredContextsSatisfied (latest rerun succeeded), got %v", status)
	}
}

// Failure takes precedence over missing/pending when both classes are present.
func TestClassifyRequiredContexts_FailedTakesPrecedenceOverMissing(t *testing.T) {
	status, missing, _, failed := ClassifyRequiredContexts(
		[]string{"ci/test", "ci/never-runs"},
		[]CheckRun{{ID: 1, Name: "ci/test", Status: "completed", Conclusion: "failure"}},
		nil,
	)
	if status != RequiredContextsFailed {
		t.Errorf("expected RequiredContextsFailed, got %v", status)
	}
	if !reflect.DeepEqual(failed, []string{"ci/test"}) {
		t.Errorf("expected failed=[ci/test], got %v", failed)
	}
	if !reflect.DeepEqual(missing, []string{"ci/never-runs"}) {
		t.Errorf("expected missing=[ci/never-runs], got %v", missing)
	}
}

func TestClassifyRequiredContexts_AllSatisfied_MixedSources(t *testing.T) {
	status, _, _, _ := ClassifyRequiredContexts(
		[]string{"ci/test", "fantasy/local-test"},
		[]CheckRun{{ID: 1, Name: "ci/test", Status: "completed", Conclusion: "success"}},
		[]CommitStatus{{Context: "fantasy/local-test", State: "success"}},
	)
	if status != RequiredContextsSatisfied {
		t.Errorf("expected RequiredContextsSatisfied across mixed check-run + commit-status sources, got %v", status)
	}
}
