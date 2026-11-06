package engine

import (
	"context"
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// columnProbeClient is the minimal GitHub client behind
// StartupRequiredColumnsForTest. Embedding the nil interface means any call
// other than the two overridden ones panics, which is the point: the probe
// must not depend on anything else.
type columnProbeClient struct {
	GitHubClient
	options map[string]string
}

func (c *columnProbeClient) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	return &gh.ProjectBoard{ProjectID: "PVT_probe"}, nil
}

func (c *columnProbeClient) FetchStatusField(projectID string) (*gh.StatusField, error) {
	return &gh.StatusField{Options: c.options}, nil
}

// StartupRequiredColumnsForTest returns the names of the given stages whose
// board column the real startup column-alignment check (checkStageColumnAlignment)
// refuses to start without, with merge_train on (so a missing holding-stage
// column is fatal too). It runs the real check once per stage name against a
// board that has every column except that one, and reports the names for which
// the check fails. startup.go is not consulted or duplicated, so a change to
// the check's rule changes this answer. Test-only seam (#2496): `fabrik init`
// lists columns from cmd, which cannot call the unexported check.
func StartupRequiredColumnsForTest(allStages []*stages.Stage) ([]string, error) {
	var required []string
	seen := map[string]bool{}
	for _, s := range allStages {
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		options := map[string]string{}
		for _, o := range allStages {
			if o.Name != s.Name {
				options[o.Name] = "OPT_" + o.Name
			}
		}
		eng := NewWithDeps(
			Config{Owner: "owner", Repo: "repo", ProjectNum: 1, MaxConcurrent: 1, MergeTrain: "on", Stages: allStages},
			&columnProbeClient{options: options}, nil, NewWorktreeManager(""),
		)
		eng.SetLogSinkForTest(func(int, string, string) {})
		if err := eng.checkStageColumnAlignment(context.Background()); err != nil {
			required = append(required, s.Name)
		}
	}
	if len(required) == 0 && len(allStages) > 0 {
		// A probe that never fails would silently make the cmd test vacuous.
		return nil, fmt.Errorf("startup check required no columns for %d stages", len(allStages))
	}
	return required, nil
}
