package pruefer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// This file is the only non-test file in the pruefer package that may call the
// tree, blob, subtree and ref readers (TestRepoPluginReadersAreOnlyReachableBehindTheGate).
// It reads a repo's plugin declarations at the review's base SHA, resolves the
// plugins the repo's base-ref .pruefer/config.yaml names to fixed commits, and
// fetches and validates their trees through the API (#2366). It never touches
// the clone: the clone is the untrusted PR head.

// The named gates, neutralisable in tests (FR-017) like the validation rules.
const (
	gateSwitch        = "gate-switch"         // repo_plugins is on
	gateDefaultBranch = "gate-default-branch" // the PR's base is the repository's default branch
	gateBaseSHA       = "gate-base-sha"       // the base resolved to a commit
	gateAllowlist     = "gate-allowlist"      // every other repository is on the operator allowlist
	gatePinBranch     = "gate-pin-branch"     // a branch is refused as a source
	gatePinAmbiguous  = "gate-pin-ambiguous"  // a name that is both a branch and a tag is refused
)

// repoPluginGateRules lists every gate, for the neutralisation tests.
var repoPluginGateRules = []string{gateSwitch, gateDefaultBranch, gateBaseSHA, gateAllowlist, gatePinBranch, gatePinAmbiguous}

// repoPluginGate is the proof that the capability is on and the PR is eligible.
// Only evaluateRepoPluginGate can produce an open one, and fetchRepoPlugins
// refuses a closed one, so no path to the readers bypasses the gate.
type repoPluginGate struct{ open bool }

// evaluateRepoPluginGate decides whether this review may read and load repo
// plugins at all (#2366, FR-001/FR-002). In order: the operator's repo_plugins
// switch (default off — silent, nothing is read); a non-empty `plugins:`
// selection in the base-ref repo config (silent no-op otherwise: nothing to
// load); a base branch that resolved to a SHA (plugin code never falls back to
// a branch name the way config text does); and a PR whose base is the
// repository's default branch (only there is "merged by the maintainers"
// true; an unknown default branch fails closed). why explains a closed gate
// that is worth a log line, "" when the gate is closed silently.
func evaluateRepoPluginGate(cfg Config, selected []string, baseRef, defaultBranch, baseSHA string) (g repoPluginGate, why string) {
	switch {
	case !cfg.RepoPlugins && repoPluginRuleOn(gateSwitch):
		return repoPluginGate{}, ""
	case len(selected) == 0:
		return repoPluginGate{}, ""
	case baseSHA == "" && repoPluginRuleOn(gateBaseSHA):
		return repoPluginGate{}, "repo plugins not loaded: the base branch did not resolve to a commit, and plugin code is never read from a branch name"
	case defaultBranch == "" && repoPluginRuleOn(gateDefaultBranch):
		return repoPluginGate{}, "repo plugins not loaded: the repository's default branch is unknown, so the PR's base cannot be shown to be maintainer-gated"
	case baseRef != defaultBranch && repoPluginRuleOn(gateDefaultBranch):
		return repoPluginGate{}, fmt.Sprintf("repo plugins not loaded: the PR's base %s is not the repository's default branch %s, so its content is not maintainer-gated", sanitizeNoticeText(baseRef), sanitizeNoticeText(defaultBranch))
	}
	return repoPluginGate{open: true}, ""
}

// repoPluginInput is what fetchRepoPlugins needs.
type repoPluginInput struct {
	Owner, Repo string
	PRNumber    int
	BaseSHA     string
	Selected    []string // <plugin>@<marketplace> entries from the base-ref repo config
	Allow       []string // repo_plugin_marketplaces
	Cache       *repoPluginCache
}

// loadedRepoPlugin is one plugin fetched and validated for a review.
type loadedRepoPlugin struct {
	ID      string // <plugin>@<marketplace>
	Name    string // the plugin (marketplace entry) name
	Origin  string // originInRepo or originExternal
	Repo    string // owner/repo the bytes came from
	Commit  string // the fixed commit they were read at
	Path    string // the plugin directory within the repo ("" is the root)
	TreeSHA string
	Cached  bool // served from the tree cache: no blob was read
	Tree    *repoPluginTree
}

// Plugin origins, for the log.
const (
	originInRepo   = "in-repo"
	originExternal = "external"
)

// repoPluginSkip is a selected plugin that was not loaded. Reason is short,
// fixed-vocabulary text that may reach a posted review; Detail is log-only and
// may carry API error text.
type repoPluginSkip struct {
	ID     string
	Reason string
	Detail string
}

// repoPluginResult is everything fetchRepoPlugins learned.
type repoPluginResult struct {
	Plugins []loadedRepoPlugin
	Skipped []repoPluginSkip
	// Degraded is set when a failed read, not a deterministic verdict about the
	// repo, shaped the result: the outcome must not be memoised (#1952).
	Degraded bool
}

// sanitizeLogID makes a selection entry safe for a log line, keeping the "@"
// that separates plugin from marketplace.
func sanitizeLogID(id string) string { return sanitizeNoticeRunes(id, 120, "@") }

// describePluginID renders a selection entry for a posted notice, where "@" is
// not allowed (it could form a mention): a well-formed id reads as
// "plugin in marketplace", anything else is sanitised.
func describePluginID(id string) string {
	if p, m, ok := parsePluginID(id); ok {
		return fmt.Sprintf("%s in marketplace %s", sanitizeNoticeText(p), sanitizeNoticeText(m))
	}
	return sanitizeNoticeText(id)
}

// Notes renders why each selected plugin did not load, for any fallback notice
// the review posts. Every id is sanitised; none names a local path.
func (r repoPluginResult) Notes() []string {
	var n []string
	for _, s := range r.Skipped {
		n = append(n, fmt.Sprintf("repo plugin %s not loaded: %s", describePluginID(s.ID), s.Reason))
	}
	return n
}

// Dirs/trees are written by buildPluginSet.

// declState is one declaration file read at the base SHA, read at most once.
type declState struct {
	read     bool
	data     []byte
	absent   bool
	failed   string // transport/API failure
	parseErr string // deterministic: the content is unusable
}

type fetcher struct {
	client   GitHubReviewer
	in       repoPluginInput
	cache    *repoPluginCache
	reviewed string // lower-cased owner/repo
	res      repoPluginResult
	budget   repoPluginBudget

	inRepo      declState
	inRepoMan   marketplaceManifest
	settings    declState
	settingsMkt map[string]settingsMarketplace
	extManifest map[string]*extManifestState // "repo@commit" -> manifest
}

type extManifestState struct {
	man marketplaceManifest
	err string
}

// refusal is a reason a plugin was not loaded. degraded marks a transport
// failure rather than a verdict.
type refusal struct {
	reason   string
	detail   string
	degraded bool
}

func refuse(reason string) *refusal { return &refusal{reason: reason} }

func refuseDetail(reason string, err error) *refusal {
	return &refusal{reason: reason, detail: err.Error()}
}

func transient(reason string, err error) *refusal {
	return &refusal{reason: reason, detail: err.Error(), degraded: true}
}

// fetchRepoPlugins loads the plugins a repo's base-ref config names. It takes a
// gate that only evaluateRepoPluginGate can open.
func fetchRepoPlugins(client GitHubReviewer, gate repoPluginGate, in repoPluginInput) repoPluginResult {
	if !gate.open {
		return repoPluginResult{}
	}
	f := &fetcher{
		client:      client,
		in:          in,
		cache:       in.Cache,
		reviewed:    strings.ToLower(in.Owner + "/" + in.Repo),
		extManifest: map[string]*extManifestState{},
	}
	if f.cache == nil {
		f.cache = defaultRepoPluginCache
	}
	seen := map[string]bool{}
	tried := 0
	for _, id := range in.Selected {
		if seen[id] {
			continue
		}
		seen[id] = true
		if tried >= maxRepoPluginsSelected {
			f.skip(id, refuse(fmt.Sprintf("more than %d plugins are selected", maxRepoPluginsSelected)))
			continue
		}
		tried++
		p, r := f.load(id)
		if r != nil {
			f.skip(id, r)
			continue
		}
		f.res.Plugins = append(f.res.Plugins, *p)
	}
	return f.res
}

func (f *fetcher) skip(id string, r *refusal) {
	f.res.Skipped = append(f.res.Skipped, repoPluginSkip{ID: id, Reason: r.reason, Detail: r.detail})
	if r.degraded {
		f.res.Degraded = true
	}
}

// readDecl reads a declaration file at the base SHA once.
func (f *fetcher) readDecl(st *declState, p string) {
	if st.read {
		return
	}
	st.read = true
	data, err := f.client.FetchFileAtRef(f.in.Owner, f.in.Repo, p, f.in.BaseSHA)
	switch {
	case err == nil:
		st.data = data
	case errors.Is(err, gh.ErrNotFound):
		st.absent = true
	default:
		st.failed = err.Error()
		f.res.Degraded = true
	}
}

// inRepoMarketplace returns the reviewed repo's own marketplace manifest, read
// from the base SHA.
func (f *fetcher) inRepoMarketplace() (marketplaceManifest, *refusal) {
	f.readDecl(&f.inRepo, marketplaceFile)
	switch {
	case f.inRepo.failed != "":
		return marketplaceManifest{}, &refusal{reason: "the repo's marketplace file could not be read", detail: f.inRepo.failed, degraded: true}
	case f.inRepo.absent:
		return marketplaceManifest{}, refuse("the repo has no " + marketplaceFile)
	}
	if f.inRepo.parseErr == "" && f.inRepoMan.Name == "" {
		man, err := parseMarketplaceManifest(f.inRepo.data)
		if err != nil {
			f.inRepo.parseErr = err.Error()
		}
		f.inRepoMan = man
	}
	if f.inRepo.parseErr != "" {
		return marketplaceManifest{}, refuseDetail("the repo's marketplace file is unusable", errors.New(f.inRepo.parseErr))
	}
	return f.inRepoMan, nil
}

func (f *fetcher) settingsMarketplaces() (map[string]settingsMarketplace, *refusal) {
	f.readDecl(&f.settings, claudeSettingsFile)
	switch {
	case f.settings.failed != "":
		return nil, &refusal{reason: "the repo's Claude settings file could not be read", detail: f.settings.failed, degraded: true}
	case f.settings.absent:
		return nil, nil
	}
	if f.settingsMkt == nil && f.settings.parseErr == "" {
		m, err := parseSettingsMarketplaces(f.settings.data)
		if err != nil {
			f.settings.parseErr = err.Error()
		}
		f.settingsMkt = m
	}
	if f.settings.parseErr != "" {
		return nil, refuseDetail("the repo's Claude settings file is unusable", errors.New(f.settings.parseErr))
	}
	return f.settingsMkt, nil
}

// location is a marketplace a selection resolved to: where it lives, at which
// fixed commit, and its manifest.
type location struct {
	repo     string // owner/repo
	commit   string
	external bool
	man      marketplaceManifest
}

// locate finds the marketplace named mkt among those the repo declares at the
// base SHA: its own in-repo marketplace, or an extraKnownMarketplaces entry.
func (f *fetcher) locate(mkt string) (*location, *refusal) {
	man, inErr := f.inRepoMarketplace()
	if inErr == nil && man.Name == mkt {
		return &location{repo: f.reviewed, commit: f.in.BaseSHA, man: man}, nil
	}
	if inErr != nil && inErr.degraded {
		return nil, inErr
	}
	settings, sErr := f.settingsMarketplaces()
	if sErr != nil {
		return nil, sErr
	}
	entry, ok := settings[mkt]
	if !ok {
		if inErr != nil && f.inRepo.parseErr != "" {
			return nil, inErr
		}
		return nil, refuse("its marketplace is not declared by the repo")
	}
	ext, same, err := resolveSettingsMarketplace(entry, f.reviewed)
	if err != nil {
		return nil, refuseDetail("its marketplace source is not supported", err)
	}
	if same {
		return nil, refuse("the repo's own marketplace is not named that")
	}
	if r := f.checkAllowed(ext.Repo); r != nil {
		return nil, r
	}
	commit, r := f.pin(ext.Repo, "", ext.Ref)
	if r != nil {
		return nil, r
	}
	key := ext.Repo + "@" + commit
	st, ok := f.extManifest[key]
	if !ok {
		st = &extManifestState{}
		f.extManifest[key] = st
		owner, repo, _ := strings.Cut(ext.Repo, "/")
		data, err := f.client.FetchFileAtRef(owner, repo, marketplaceFile, commit)
		switch {
		case err == nil:
			man, perr := parseMarketplaceManifest(data)
			if perr != nil {
				st.err = perr.Error()
			}
			st.man = man
		case errors.Is(err, gh.ErrNotFound):
			st.err = "no " + marketplaceFile + " at the pinned commit, or the repository is not readable with this installation's token"
		default:
			f.res.Degraded = true
			return nil, transient("its marketplace file could not be read", err)
		}
	}
	if st.err != "" {
		return nil, refuseDetail("its marketplace file is unusable", errors.New(st.err))
	}
	if st.man.Name != mkt {
		return nil, refuse("the marketplace file at its source is not named that")
	}
	return &location{repo: ext.Repo, commit: commit, external: true, man: st.man}, nil
}

// checkAllowed enforces the operator allowlist (FR-009, FR-011) for any
// repository other than the reviewed one. The test is on the resolved
// repository, never on a marketplace name the reviewed repo chose.
func (f *fetcher) checkAllowed(repo string) *refusal {
	if repo == f.reviewed || !repoPluginRuleOn(gateAllowlist) {
		return nil
	}
	if !repoAllowed(f.in.Allow, repo) {
		return refuse(fmt.Sprintf("its source repository %s is not on the operator's repo_plugin_marketplaces allowlist", sanitizeNoticeText(repo)))
	}
	return nil
}

// pin turns a sha/ref into a fixed commit of repo (FR-009): a 40-hex SHA as is,
// a tag resolved to its commit, anything that would float refused.
func (f *fetcher) pin(repo, sha, ref string) (string, *refusal) {
	req, err := pinFor(sha, ref)
	if err != nil {
		return "", refuseDetail("its source is not pinned to a fixed commit", err)
	}
	if req.Commit != "" {
		return req.Commit, nil
	}
	owner, name, _ := strings.Cut(repo, "/")
	rr, err := f.client.ResolveRefKind(owner, name, req.Resolve)
	switch {
	case errors.Is(err, gh.ErrNotFound):
		return "", refuse(fmt.Sprintf("its ref %s was not found, or the repository is not readable with this installation's token", sanitizeNoticeText(req.Resolve)))
	case errors.Is(err, gh.ErrForbidden):
		return "", refuse("the repository is not readable with this installation's token")
	case err != nil:
		return "", transient("its ref could not be resolved", err)
	}
	switch rr.Kind {
	case gh.RefTag:
		return rr.SHA, nil
	case gh.RefBranch:
		if repoPluginRuleOn(gatePinBranch) {
			return "", refuse(fmt.Sprintf("its ref %s is a branch, which moves; pin it to a commit SHA or a tag", sanitizeNoticeText(req.Resolve)))
		}
	case gh.RefAmbiguous:
		if repoPluginRuleOn(gatePinAmbiguous) {
			return "", refuse(fmt.Sprintf("its ref %s is both a branch and a tag", sanitizeNoticeText(req.Resolve)))
		}
	}
	if rr.SHA == "" {
		return "", refuse("its ref did not resolve to a commit")
	}
	return rr.SHA, nil
}

// load resolves, fetches and validates one selected plugin.
func (f *fetcher) load(id string) (*loadedRepoPlugin, *refusal) {
	name, mkt, ok := parsePluginID(id)
	if !ok {
		return nil, refuse("it is not a <plugin>@<marketplace> id")
	}
	loc, r := f.locate(mkt)
	if r != nil {
		return nil, r
	}
	entry, ok := loc.man.entry(name)
	if !ok {
		return nil, refuse("its marketplace does not list it")
	}
	src, err := resolvePluginSource(entry.Source, loc.man.PluginRoot)
	if err != nil {
		return nil, refuseDetail("its source is not supported", err)
	}
	repo, commit, external := loc.repo, loc.commit, loc.external
	if !src.Relative {
		if r := f.checkAllowed(src.Repo); r != nil {
			return nil, r
		}
		if commit, r = f.pin(src.Repo, src.SHA, src.Ref); r != nil {
			return nil, r
		}
		repo, external = src.Repo, src.Repo != f.reviewed
	}
	origin := originInRepo
	if external {
		origin = originExternal
	}
	p, r := f.fetchTree(repo, commit, src.Path, external)
	if r != nil {
		return nil, r
	}
	p.ID, p.Name, p.Origin = id, name, origin
	if r := f.checkManifestName(p.Tree, name); r != nil {
		return nil, r
	}
	if why := f.budget.admit(p.Tree.Bytes, len(p.Tree.Files)); why != "" {
		return nil, refuse(why)
	}
	return p, nil
}

// fetchTree reads the plugin directory dir of repo at commit: from the cache
// when its tree SHA (or, for an external source, its commit) is known, else
// through the Trees and Blobs API after validating the listing's metadata.
func (f *fetcher) fetchTree(repo, commit, dir string, external bool) (*loadedRepoPlugin, *refusal) {
	owner, name, _ := strings.Cut(repo, "/")
	p := &loadedRepoPlugin{Repo: repo, Commit: commit, Path: dir}
	// An external source is fixed by its commit alone, so a hit here costs no
	// request at all.
	if external {
		if t, ok := f.cache.get(commitCacheKey(repo, commit, dir)); ok {
			p.Tree, p.Cached = t, true
			return p, nil
		}
	}
	treeSHA, err := f.client.FetchSubtreeSHA(owner, name, commit, dir)
	switch {
	case errors.Is(err, gh.ErrNotFound):
		return nil, refuseDetail("its plugin directory does not exist at that commit", err)
	case errors.Is(err, gh.ErrUnreadable), errors.Is(err, gh.ErrForbidden):
		return nil, refuseDetail("its repository or commit is not readable with this installation's token", err)
	case err != nil:
		return nil, transient("its plugin directory could not be read", err)
	}
	p.TreeSHA = treeSHA
	// A tree SHA is content-addressed: an unchanged plugin has the same one at
	// every commit, so a hit skips the listing and every blob read.
	if t, ok := f.cache.get(treeCacheKey(repo, treeSHA)); ok {
		if external {
			f.cache.put(commitCacheKey(repo, commit, dir), t)
		}
		p.Tree, p.Cached = t, true
		return p, nil
	}
	entries, truncated, err := f.client.FetchTreeBySHA(owner, name, treeSHA)
	if err != nil {
		if errors.Is(err, gh.ErrUnreadable) || errors.Is(err, gh.ErrForbidden) || errors.Is(err, gh.ErrNotFound) {
			return nil, refuseDetail("its plugin tree is not readable with this installation's token", err)
		}
		return nil, transient("its plugin tree could not be listed", err)
	}
	files, why := validateRepoPluginTree(entries, truncated)
	if why != "" {
		return nil, &refusal{reason: "its plugin tree was refused: " + sanitizeNoticeReason(why), detail: why}
	}
	tree := &repoPluginTree{}
	for _, e := range files {
		data, err := f.client.FetchBlob(owner, name, e.SHA, maxRepoPluginFileBytes)
		if err != nil {
			return nil, transient("a file of its plugin could not be read", fmt.Errorf("%s: %w", e.Path, err))
		}
		tree.Files = append(tree.Files, repoPluginFile{Path: e.Path, Data: data, Exec: e.Mode == "100755"})
		tree.Bytes += int64(len(data))
	}
	f.cache.put(treeCacheKey(repo, treeSHA), tree)
	if external {
		f.cache.put(commitCacheKey(repo, commit, dir), tree)
	}
	p.Tree = tree
	return p, nil
}

// checkManifestName requires the fetched directory to be loadable as a plugin
// and its manifest name to equal the marketplace entry name (Claude Code's
// documented rule), so `plugins:` (entry name) and `review_skill:` (manifest
// name) can never disagree about which plugin is meant.
func (f *fetcher) checkManifestName(t *repoPluginTree, entryName string) *refusal {
	for _, file := range t.Files {
		if file.Path != pluginManifestFile {
			continue
		}
		var m struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(file.Data, &m); err != nil || m.Name == "" {
			return refuse("its " + pluginManifestFile + " is unusable")
		}
		if m.Name != entryName {
			return refuse(fmt.Sprintf("its %s names %s, not the marketplace entry name", pluginManifestFile, sanitizeNoticeText(m.Name)))
		}
		return nil
	}
	return refuse("it has no " + pluginManifestFile + ", so it cannot be loaded as a plugin directory")
}
