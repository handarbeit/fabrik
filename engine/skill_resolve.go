package engine

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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

// resolvedSkill is the outcome of resolving one reference.
type resolvedSkill struct {
	Name       string   // fully-qualified name the prompt carries
	Deprecated bool     // the reference was bare and was qualified by lookup
	Others     []string // other qualified providers of an ambiguous bare name
}

// skillUnresolvedError reports a reference no loaded plugin provides. It is
// returned before any worker is spawned (ADR 2137, FR-005).
type skillUnresolvedError struct {
	Stage string // stage name
	Key   string // stage key the reference came from
	Ref   string
	Why   string
	Cat   *skillCatalogue
}

func (e *skillUnresolvedError) Error() string {
	return fmt.Sprintf("stage %q: %s %q is unresolved: %s; plugins searched: %s",
		e.Stage, e.Key, e.Ref, e.Why, e.Cat.describe())
}

// describe lists the searched plugins and their paths.
func (c *skillCatalogue) describe() string {
	if c.empty() {
		return "(none)"
	}
	parts := make([]string, 0, len(c.plugins))
	for _, p := range c.plugins {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.Name, p.Dir))
	}
	return strings.Join(parts, ", ")
}

// resolve resolves ref against the snapshot. ok=false means unresolved and why
// explains it. An empty catalogue (no plugin directories) passes the reference
// through unchanged: the plugin then comes from Claude Code's own install,
// which the engine cannot see (ADR 2137).
func (c *skillCatalogue) resolve(ref string) (res resolvedSkill, why string, ok bool) {
	if c.empty() {
		return resolvedSkill{Name: ref}, "", true
	}
	if pn, skill, qualified := plugin.Split(ref); qualified {
		p, found := c.plugin(pn)
		if !found {
			return resolvedSkill{}, fmt.Sprintf("plugin %q is not loaded", pn), false
		}
		if _, has := p.Skills[skill]; !has {
			return resolvedSkill{}, fmt.Sprintf("plugin %q has no skill %q", pn, skill), false
		}
		return resolvedSkill{Name: ref}, "", true
	}

	// Bare: the base plugin wins, otherwise load order.
	var providers []loadedPlugin
	for _, p := range c.plugins {
		if _, has := p.Skills[ref]; has {
			providers = append(providers, p)
		}
	}
	if len(providers) == 0 {
		return resolvedSkill{}, fmt.Sprintf("no loaded plugin provides a skill named %q", ref), false
	}
	chosen := providers[0]
	for _, p := range providers {
		if p.Name == plugin.BasePluginName {
			chosen = p
			break
		}
	}
	res = resolvedSkill{Name: plugin.Qualify(chosen.Name, ref), Deprecated: true}
	for _, p := range providers {
		if p.Dir != chosen.Dir {
			res.Others = append(res.Others, plugin.Qualify(p.Name, ref))
		}
	}
	return res, "", true
}

// skillResolver resolves stage skill references against the plugins loaded for
// the workers. The snapshot is immutable; on an unresolved lookup it is rebuilt
// once from disk (under a mutex) and the lookup retried, so a plugin edited or
// upgraded while the daemon runs is picked up without a scan per dispatch.
type skillResolver struct {
	dirs []string
	mu   sync.Mutex // serialises rebuilds
	cat  atomic.Pointer[skillCatalogue]
}

func newSkillResolver(dirs []string) *skillResolver {
	r := &skillResolver{dirs: append([]string(nil), dirs...)}
	r.cat.Store(buildSkillCatalogue(r.dirs))
	return r
}

// Resolve resolves ref. On failure the returned catalogue is the one searched.
func (r *skillResolver) Resolve(ref string) (resolvedSkill, string, *skillCatalogue, bool) {
	cat := r.cat.Load()
	if res, _, ok := cat.resolve(ref); ok {
		return res, "", cat, true
	}
	fresh := r.rebuildIfStale(cat)
	res, why, ok := fresh.resolve(ref)
	return res, why, fresh, ok
}

// rebuildIfStale rescans the plugin directories unless another goroutine has
// already replaced the snapshot the caller missed on, in which case that newer
// snapshot is returned as is. Concurrent misses therefore share one rescan
// instead of queueing a redundant one each behind the mutex.
func (r *skillResolver) rebuildIfStale(seen *skillCatalogue) *skillCatalogue {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.cat.Load(); cur != seen {
		return cur
	}
	fresh := buildSkillCatalogue(r.dirs)
	r.cat.Store(fresh)
	return fresh
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
