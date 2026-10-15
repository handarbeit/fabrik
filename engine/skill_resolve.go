package engine

import (
	"slices"

	"github.com/handarbeit/fabrik/internal/skillref"
	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

// skillRefKey names which stage key a skill reference came from.
const (
	skillKeySkill        = "skill"
	skillKeyCommentSkill = "comment_skill"
	skillKeyCIFixSkill   = "ci_fix_skill"
	skillKeyRebaseSkill  = "rebase_skill"
)

// Aliases keep the engine's call sites unchanged over the shared package
// (ADR 2305).
type (
	resolvedSkill        = skillref.Resolved
	skillUnresolvedError = skillref.UnresolvedError
	skillResolver        = skillref.Resolver
	skillCatalogue       = skillref.Catalogue
)

// fabrikSkillOptions is Fabrik's wiring of the shared resolver: a bare name
// prefers the renamed base plugin, then a customised pre-#2140 one, and the
// pre-rename references map through plugin.LegacyRef (ADR 2137, ADR 2140).
var fabrikSkillOptions = skillref.Options{
	PreferredBases: []string{plugin.BasePluginName, plugin.LegacyBasePluginName},
	LegacyRef:      plugin.LegacyRef,
}

func newSkillResolver(dirs []string) *skillResolver {
	return skillref.NewResolver(dirs, fabrikSkillOptions)
}

// stageSkillRefs returns the non-empty skill-valued keys of a stage in a
// stable order, as (key, reference) pairs. A non-empty only restricts the
// result to those keys: each dispatch path resolves just the key it uses, so a
// bad reference in one key never blocks a path that does not read it.
func stageSkillRefs(s *stages.Stage, only []string) [][2]string {
	var refs [][2]string
	for _, kv := range [][2]string{
		{skillKeySkill, s.Skill},
		{skillKeyCommentSkill, s.CommentSkill},
		{skillKeyCIFixSkill, s.CIFixSkill},
		{skillKeyRebaseSkill, s.RebaseSkill},
	} {
		if kv[1] == "" {
			continue
		}
		if len(only) > 0 && !slices.Contains(only, kv[0]) {
			continue
		}
		refs = append(refs, kv)
	}
	return refs
}

// resolveStageSkills returns a copy of stage whose skill-valued keys carry
// fully-qualified names, so the prompt builders emit exactly what Claude Code
// registered. The original stage is never mutated (it is shared by concurrent
// workers). keys limits resolution to the named stage keys (all four when
// none are given). With no resolver (a NewWithDeps engine) the stage is returned
// as-is. Bare references that were qualified by lookup are returned in
// resolved for the caller to log; the first unresolved reference stops the
// resolution and is returned as a *skillUnresolvedError.
func (e *Engine) resolveStageSkills(stage *stages.Stage, keys ...string) (*stages.Stage, []resolvedRef, error) {
	if e.skillResolver == nil || stage == nil {
		return stage, nil, nil
	}
	refs := stageSkillRefs(stage, keys)
	if len(refs) == 0 {
		return stage, nil, nil
	}
	out := *stage
	var resolved []resolvedRef
	for _, kv := range refs {
		res, why, cat, ok := e.skillResolver.Resolve(kv[1])
		if !ok {
			return nil, nil, &skillUnresolvedError{Stage: stage.Name, Key: kv[0], Ref: kv[1], Why: why, Cat: cat}
		}
		switch kv[0] {
		case skillKeySkill:
			out.Skill = res.Name
		case skillKeyCommentSkill:
			out.CommentSkill = res.Name
		case skillKeyCIFixSkill:
			out.CIFixSkill = res.Name
		case skillKeyRebaseSkill:
			out.RebaseSkill = res.Name
		}
		resolved = append(resolved, resolvedRef{Key: kv[0], Ref: kv[1], resolvedSkill: res})
	}
	return &out, resolved, nil
}

// resolvedRef pairs a stage key and its original reference with the outcome.
type resolvedRef struct {
	Key string
	Ref string
	resolvedSkill
}
