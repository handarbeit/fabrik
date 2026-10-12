package engine

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

// skillLoadType is the warnings-panel type of a skill that passed pre-flight
// resolution but failed to load in the worker (#2138); the subject is
// "<stage>.<key>", like the pre-flight skillUnresolvedType.
const skillLoadType = "skill_load"

// describe lists the plugins the resolver loaded. Nil-safe: NewWithDeps engines
// have no resolver, and an empty catalogue reads "(none)".
func (r *skillResolver) describe() string {
	if r == nil {
		return "(none)"
	}
	return r.cat.Load().describe()
}

// skillLoadCondition builds the warnings-panel entry for a runtime skill-load
// failure. The fingerprint is stable (skill + sanitised detail) so a dismissal
// follows the cause, not the stage.
func skillLoadCondition(stageName, key string, sle *claudeSkillLoadError, plugins string) warnings.Condition {
	return warnings.Condition{
		Type:    skillLoadType,
		Subject: stageName + "." + key,
		Level:   warnings.LevelWarning,
		Title:   fmt.Sprintf("%s: %s %q failed to load", stageName, key, sle.Skill),
		Detail: fmt.Sprintf("Stage %q ran, but its %s %q failed to load in the worker, so the stage was not completed and the item is paused.\n\nPlugins loaded: %s.\n\nError: %s\n\n"+
			"Fix: check that the plugin providing the skill is registered with Claude Code (plugin_dirs, the plugin's manifest and the skill's SKILL.md), then remove fabrik:paused and fabrik:awaiting-input from the item (see docs/USER_GUIDE.md).",
			stageName, key, sle.Skill, plugins, skillLoadDetailOrNone(sle.Detail)),
		Fingerprint: sle.Skill + "|" + sle.Detail,
	}
}

func skillLoadDetailOrNone(detail string) string {
	if detail == "" {
		return "(the tool result carried no text)"
	}
	return detail
}

// handleSkillLoadFailed is the shared handler for a worker whose own configured
// skill demonstrably failed to load (#2138, ADR-2138). Unlike the pre-flight
// skillUnresolvedError (ADR 2137, self-healing, never paused) this condition
// cannot self-heal — resolution already passed — so a cooldown would re-spawn a
// worker every cycle. The item is held for a human instead: one comment through
// pauseIssue (fabrik:paused + fabrik:awaiting-input), the warnings-panel entry,
// and EnginePaused so the engine knows it paused the item. Never
// StageRetryIncremented, never stage:<name>:failed. key is the stage key the
// skill came from ("skill" for stage dispatch, "comment_skill" for the comment,
// CI-fix and rebase paths, which all run through the stage's CommentSkill).
func (e *Engine) handleSkillLoadFailed(item gh.ProjectItem, stage *stages.Stage, key string, sle *claudeSkillLoadError) {
	e.logf(item.Number, "skill", "stage %q: %s %q failed to load — pausing (not a stage failure): %v\n", stage.Name, key, sle.Skill, sle)
	plugins := e.skillResolver.describe()
	if err := warnings.Report(e.warnWriter("skill"), skillLoadCondition(stage.Name, key, sle, plugins)); err != nil {
		e.logf(item.Number, "skill", "recording %s: %v\n", skillLoadType, err)
	}

	called := ""
	if sle.Called != "" {
		called = fmt.Sprintf(" (the worker called it as `%s`)", sle.Called)
	}
	comment := fmt.Sprintf(
		"🏭 **Fabrik — skill failed to load**\n\nStage **%s** did not complete: its `%s` `%s`%s failed to load in the worker, so the worker ran without the stage's instructions. The completion marker, if any, was ignored.\n\n"+
			"Plugins loaded: %s.\n\nError: `%s`\n\n"+
			"This is a configuration fault, not a stage failure — it does not count against `max_retries`. Check that the plugin providing the skill is registered with Claude Code (`plugin_dirs`, the plugin manifest and the skill's `SKILL.md`). "+
			"Fabrik has paused this issue. Once resolved, remove the `fabrik:paused` and `fabrik:awaiting-input` labels to resume. See docs/USER_GUIDE.md.",
		stage.Name, key, sle.Skill, called, plugins, skillLoadDetailOrNone(sle.Detail),
	)
	e.pauseIssue(item, comment, pauseOpts{awaitingInput: true})

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
}

// clearSkillLoadFailed clears the panel entry once an invocation of the stage
// that used the skill reached Claude without a skill-load failure.
func (e *Engine) clearSkillLoadFailed(stageName, key string) {
	if err := warnings.Resolve(skillLoadType + ":" + stageName + "." + key); err != nil {
		e.logf(0, "skill", "clearing %s: %v\n", skillLoadType, err)
	}
}
