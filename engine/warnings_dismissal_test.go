package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/selfupgrade"
	"github.com/handarbeit/fabrik/warnings"
)

func dismissedState(t *testing.T, key string) (found, dismissed bool) {
	t.Helper()
	es, err := warnings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range es {
		if e.Key == key {
			return true, e.Dismissed
		}
	}
	return false, false
}

// A dismissed source_staleness warning stays dismissed while its detail's
// volatile commit count moves (#2135: constant fingerprint).
func TestSourceStaleness_DismissalSurvivesDetailChurn(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "dev(8a5ef0f1234567890abcdef1234567890abcdef)"
	behind := 75
	needs := true
	eng.stalenessCompareFn = func(selfupgrade.DevBuildConfig) (selfupgrade.DevBuildStatus, error) {
		return selfupgrade.DevBuildStatus{
			Applicable: true, LocalRef: "8a5ef0f1234567890abcdef1234567890abcdef",
			LocalCommitTime: time.Now().Add(-time.Hour), RemoteAhead: true,
			CommitsBehind: behind, NeedsRebuild: needs,
		}, nil
	}

	eng.checkSourceStaleness()
	if found, _ := dismissedState(t, sourceStalenessWarningKey); !found {
		t.Fatal("expected a recorded staleness warning")
	}
	if err := warnings.Dismiss(sourceStalenessWarningKey); err != nil {
		t.Fatal(err)
	}
	behind = 80
	eng.checkSourceStaleness()
	if _, dismissed := dismissedState(t, sourceStalenessWarningKey); !dismissed {
		t.Error("dismissal lost when only the commit count changed")
	}

	needs = false
	eng.checkSourceStaleness()
	if found, _ := dismissedState(t, sourceStalenessWarningKey); found {
		t.Error("staleness warning not cleared once no rebuild is needed")
	}
}
