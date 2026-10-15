package skillref

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/handarbeit/fabrik/plugin"
)

// Options carries what differs between consumers of the resolver. The zero
// value is valid: bare names resolve by load order and no legacy mapping runs.
type Options struct {
	// PreferredBases lists, in order, the plugin names that win a bare
	// reference provided by several plugins. The first name with a provider
	// wins; when none has one, load order decides. The engine passes
	// plugin.BasePluginName then plugin.LegacyBasePluginName.
	PreferredBases []string
	// LegacyRef, when non-nil, maps a pre-rename reference to its renamed
	// form. It is consulted only after the as-is lookup misses, and a result
	// found through it is flagged Deprecated. The engine passes plugin.LegacyRef.
	LegacyRef func(ref string) (mapped string, legacy bool)
}

// Resolved is the outcome of resolving one reference.
type Resolved struct {
	Name       string   // fully-qualified name the prompt carries
	Deprecated bool     // the reference was bare, or a legacy form, and was resolved by lookup
	Others     []string // other qualified providers of an ambiguous bare name
}

// UnresolvedError reports a reference no loaded plugin provides. It is returned
// before any worker is spawned (ADR 2137, FR-005). Stage and Key are labels the
// caller supplies; this package never interprets them.
type UnresolvedError struct {
	Stage string // stage name
	Key   string // stage key the reference came from
	Ref   string
	Why   string
	Cat   *Catalogue
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("stage %q: %s %q is unresolved: %s; plugins searched: %s",
		e.Stage, e.Key, e.Ref, e.Why, e.Cat.Describe())
}

// Resolve resolves ref against the snapshot. ok=false means unresolved and why
// explains it. An empty catalogue (no plugin directories) passes the reference
// through unchanged: the plugin then comes from Claude Code's own install,
// which the caller cannot see (ADR 2137).
//
// The as-is lookup always runs first, so an operator-customised copy of a
// pre-rename base plugin keeps winning. Only on a miss does opts.LegacyRef turn
// a pre-rename reference into its renamed skill, flagged Deprecated (#2140,
// FR-004).
func (c *Catalogue) Resolve(ref string, opts Options) (res Resolved, why string, ok bool) {
	res, why, ok = c.ResolveAsIs(ref, opts)
	if ok || c.Empty() || opts.LegacyRef == nil {
		return res, why, ok
	}
	if mapped, legacy := opts.LegacyRef(ref); legacy {
		if mres, _, mok := c.ResolveAsIs(mapped, opts); mok {
			mres.Deprecated = true
			return mres, "", true
		}
	}
	return res, why, ok
}

// ResolveAsIs is Resolve without the legacy mapping.
func (c *Catalogue) ResolveAsIs(ref string, opts Options) (res Resolved, why string, ok bool) {
	if c.Empty() {
		return Resolved{Name: ref}, "", true
	}
	if pn, skill, qualified := plugin.Split(ref); qualified {
		p, found := c.Plugin(pn)
		if !found {
			return Resolved{}, fmt.Sprintf("plugin %q is not loaded", pn), false
		}
		if _, has := p.Skills[skill]; !has {
			return Resolved{}, fmt.Sprintf("plugin %q has no skill %q", pn, skill), false
		}
		return Resolved{Name: ref}, "", true
	}

	// Bare: a preferred base plugin wins, otherwise load order.
	var providers []Plugin
	for _, p := range c.plugins {
		if _, has := p.Skills[ref]; has {
			providers = append(providers, p)
		}
	}
	if len(providers) == 0 {
		return Resolved{}, fmt.Sprintf("no loaded plugin provides a skill named %q", ref), false
	}
	chosen := providers[0]
	for _, name := range opts.PreferredBases {
		found := false
		for _, p := range providers {
			if p.Name == name {
				chosen, found = p, true
				break
			}
		}
		if found {
			break
		}
	}
	res = Resolved{Name: plugin.Qualify(chosen.Name, ref), Deprecated: true}
	for _, p := range providers {
		if p.Dir != chosen.Dir {
			res.Others = append(res.Others, plugin.Qualify(p.Name, ref))
		}
	}
	return res, "", true
}

// Resolver resolves skill references against the plugins loaded for the
// workers. The snapshot is immutable; on an unresolved lookup it is rebuilt
// once from disk (under a mutex) and the lookup retried, so a plugin edited or
// upgraded while the process runs is picked up without a scan per dispatch.
type Resolver struct {
	dirs []string
	opts Options
	mu   sync.Mutex // serialises rebuilds
	cat  atomic.Pointer[Catalogue]
}

// NewResolver builds the initial snapshot from dirs.
func NewResolver(dirs []string, opts Options) *Resolver {
	opts.PreferredBases = slices.Clone(opts.PreferredBases)
	r := &Resolver{dirs: slices.Clone(dirs), opts: opts}
	r.cat.Store(Build(r.dirs))
	return r
}

// Resolve resolves ref. On failure the returned catalogue is the one searched.
func (r *Resolver) Resolve(ref string) (Resolved, string, *Catalogue, bool) {
	cat := r.cat.Load()
	if res, _, ok := cat.Resolve(ref, r.opts); ok {
		return res, "", cat, true
	}
	fresh := r.rebuildIfStale(cat)
	res, why, ok := fresh.Resolve(ref, r.opts)
	return res, why, fresh, ok
}

// Describe lists the plugins currently loaded. Nil-safe.
func (r *Resolver) Describe() string {
	if r == nil {
		return "(none)"
	}
	return r.cat.Load().Describe()
}

// rebuildIfStale rescans the plugin directories unless another goroutine has
// already replaced the snapshot the caller missed on, in which case that newer
// snapshot is returned as is. Concurrent misses therefore share one rescan
// instead of queueing a redundant one each behind the mutex.
func (r *Resolver) rebuildIfStale(seen *Catalogue) *Catalogue {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.cat.Load(); cur != seen {
		return cur
	}
	fresh := Build(r.dirs)
	r.cat.Store(fresh)
	return fresh
}
