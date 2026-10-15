package engine

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

const skillUnresolvedLabel = "fabrik:skill-unresolved"

// handleSkillUnresolved is the shared "did not run" handler for a stage skill
// reference that no loaded plugin provides (ADR 2137, FR-005). It follows the
// apiKeyHelper shape (handleAPIKeyHelperDetected): when recordAttempt is set
// StageAttempted is recorded so the dispatch cooldown applies, but
// StageRetryIncremented never is, and neither stage:<name>:failed nor
// fabrik:paused is applied — fixing the configuration or the plugin is picked
// up by the next dispatch, so the condition is self-healing. The explanatory
// comment and fabrik:skill-unresolved label are applied only on the absent ->
// applied transition. The operator warnings panel entry is written every time
// (unchanged repeats are deduplicated by the panel).
func (e *Engine) handleSkillUnresolved(item gh.ProjectItem, stage *stages.Stage, repoStr string, ue *skillUnresolvedError, recordAttempt bool) {
	e.logf(item.Number, "warn", "stage %q did not run — %v\n", stage.Name, ue)
	e.reportSkillUnresolved(ue)

	if recordAttempt {
		e.store.Apply(itemstate.StageAttempted{
			Repo:      repoStr,
			Number:    item.Number,
			StageName: stage.Name,
			At:        time.Now(),
		})
	}

	if hasLabel(item.Labels, skillUnresolvedLabel) {
		return
	}
	comment := fmt.Sprintf(
		"🏭 **Fabrik — skill reference unresolved**\n\nStage **%s** did not run because its `%s` reference `%s` does not resolve: %s.\n\nPlugins searched: %s.\n\nThis is not a stage failure — it does not count against `max_retries`. Name a skill a loaded plugin provides as `<plugin>:<skill>` in the stage YAML, or load the plugin (`plugin_dirs`); the next dispatch re-resolves and the `%s` label clears automatically once it does. See docs/USER_GUIDE.md.",
		stage.Name, ue.Key, ue.Ref, ue.Why, ue.Cat.Describe(), skillUnresolvedLabel,
	)
	e.postItemComment(item, comment, false)
	e.addLabel(item, skillUnresolvedLabel)
}
