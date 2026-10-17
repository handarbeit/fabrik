package pruefer

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// This file is the pure half of #2366's repo plugin loading: it parses the
// small subset of Claude Code's marketplace declaration formats Pruefer
// understands and decides, without any network access, what a
// <plugin>@<marketplace> selection refers to. Every shape it does not
// recognise becomes a reported refusal, never best effort: the marketplace
// schema belongs to Claude Code and drifts, so "refuse and say why" is the only
// safe reading of anything new.

const (
	// marketplaceFile is where a marketplace's manifest lives within its repo.
	marketplaceFile = ".claude-plugin/marketplace.json"
	// claudeSettingsFile declares extra marketplaces for a repo.
	claudeSettingsFile = ".claude/settings.json"
	// pluginManifestFile is the manifest a plugin directory must carry.
	pluginManifestFile = ".claude-plugin/plugin.json"
	// maxRepoPluginDeclBytes caps one declaration file read at the base SHA,
	// as DefaultMaxRepoConfigBytes does for the repo config.
	maxRepoPluginDeclBytes = 256 * 1024
	// maxRepoPluginsSelected bounds how many plugins one review will try to
	// load, so the API cost of a repo's selection is bounded.
	maxRepoPluginsSelected = 8
	// repoPluginHost is the only git host repo plugins are fetched from: the
	// host Pruefer's GitHub client talks to. Any other host is an unsupported
	// source.
	repoPluginHost = "github.com"
)

var (
	idPartRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	ownerRE    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	sha40RE    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// parsePluginID splits a selection entry "<plugin>@<marketplace>".
func parsePluginID(s string) (plugin, marketplace string, ok bool) {
	p, m, found := strings.Cut(strings.TrimSpace(s), "@")
	if !found || !idPartRE.MatchString(p) || !idPartRE.MatchString(m) {
		return "", "", false
	}
	return p, m, true
}

// normalizeRepo lower-cases a validated owner/repo. Returns "" if it is not one.
func normalizeRepo(s string) string {
	o, r, ok := strings.Cut(s, "/")
	if !ok || !ownerRE.MatchString(o) || !repoNameRE.MatchString(r) || r == "." || r == ".." || strings.Contains(r, "/") {
		return ""
	}
	return strings.ToLower(o + "/" + r)
}

// parseGitHubRepo reads a repository reference: "owner/repo" shorthand or an
// https URL on repoPluginHost (a trailing ".git" and "/" allowed). Anything
// else — another host, ssh, a path beyond owner/repo, userinfo, a query — is
// refused, so a look-alike host can never match.
func parseGitHubRepo(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty repository")
	}
	if !strings.Contains(s, "://") {
		if r := normalizeRepo(strings.TrimSuffix(s, ".git")); r != "" && !strings.ContainsAny(s, ":@") {
			return r, nil
		}
		return "", fmt.Errorf("%q is not an owner/repo on %s", s, repoPluginHost)
	}
	rest, ok := strings.CutPrefix(s, "https://"+repoPluginHost+"/")
	if !ok {
		return "", fmt.Errorf("%q is not a https://%s/ repository", s, repoPluginHost)
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	if strings.ContainsAny(rest, "?#@:") {
		return "", fmt.Errorf("%q is not a plain repository URL", s)
	}
	if r := normalizeRepo(rest); r != "" {
		return r, nil
	}
	return "", fmt.Errorf("%q is not an owner/repo on %s", s, repoPluginHost)
}

// repoAllowed reports whether ownerRepo is on the operator allowlist
// (repo_plugin_marketplaces). Entries are exact "owner/repo" or "owner/*",
// matched case-insensitively against the *resolved* repository — never against
// a marketplace name the reviewed repo chose (FR-011). An empty allowlist, or a
// malformed entry, allows nothing.
func repoAllowed(allow []string, ownerRepo string) bool {
	ownerRepo = strings.ToLower(ownerRepo)
	owner, _, _ := strings.Cut(ownerRepo, "/")
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		o, r, ok := strings.Cut(a, "/")
		if !ok || !ownerRE.MatchString(o) {
			continue
		}
		switch {
		case r == "*" && o == owner:
			return true
		case normalizeRepo(a) == ownerRepo && a == ownerRepo:
			return true
		}
	}
	return false
}

// --- marketplace manifest ----------------------------------------------------

type marketplaceEntry struct {
	Name   string
	Source json.RawMessage
}

// marketplaceManifest is the part of .claude-plugin/marketplace.json Pruefer
// reads: the marketplace name, metadata.pluginRoot and each entry's name and
// source.
type marketplaceManifest struct {
	Name       string
	PluginRoot string
	Plugins    []marketplaceEntry
}

// parseMarketplaceManifest decodes a marketplace manifest. Oversized,
// non-UTF-8 or malformed content is an error (the caller loads nothing from
// that marketplace and reports it).
func parseMarketplaceManifest(data []byte) (marketplaceManifest, error) {
	var m marketplaceManifest
	if len(data) > maxRepoPluginDeclBytes {
		return m, fmt.Errorf("%s is over the %d byte cap", marketplaceFile, maxRepoPluginDeclBytes)
	}
	if !utf8.Valid(data) {
		return m, fmt.Errorf("%s is not valid UTF-8", marketplaceFile)
	}
	var raw struct {
		Name     string `json:"name"`
		Metadata struct {
			PluginRoot string `json:"pluginRoot"`
		} `json:"metadata"`
		Plugins []struct {
			Name   string          `json:"name"`
			Source json.RawMessage `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return m, fmt.Errorf("%s is not valid JSON: %v", marketplaceFile, err)
	}
	if !idPartRE.MatchString(raw.Name) {
		return m, fmt.Errorf("%s has no usable marketplace name", marketplaceFile)
	}
	m.Name = raw.Name
	m.PluginRoot = raw.Metadata.PluginRoot
	for _, p := range raw.Plugins {
		m.Plugins = append(m.Plugins, marketplaceEntry{Name: p.Name, Source: p.Source})
	}
	return m, nil
}

func (m marketplaceManifest) entry(name string) (marketplaceEntry, bool) {
	for _, e := range m.Plugins {
		if e.Name == name {
			return e, true
		}
	}
	return marketplaceEntry{}, false
}

// --- settings: extraKnownMarketplaces ----------------------------------------

// settingsMarketplace is one extraKnownMarketplaces entry. Raw is kept so an
// unusable shape can be reported without guessing at its fields.
type settingsMarketplace struct {
	Kind        string // the source's "source" discriminator
	Repo        string // github: "repo"
	URL         string // git: "url"
	Ref         string
	Path        string
	SparsePaths []string
}

// parseSettingsMarketplaces decodes .claude/settings.json's
// extraKnownMarketplaces. enabledPlugins is deliberately not read: naming a
// plugin in .pruefer/config.yaml is the opt-in, so a plugin the repo merely
// enables for its developers is never loaded (FR-004).
func parseSettingsMarketplaces(data []byte) (map[string]settingsMarketplace, error) {
	if len(data) > maxRepoPluginDeclBytes {
		return nil, fmt.Errorf("%s is over the %d byte cap", claudeSettingsFile, maxRepoPluginDeclBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is not valid UTF-8", claudeSettingsFile)
	}
	var raw struct {
		Extra map[string]struct {
			Source struct {
				Source      string   `json:"source"`
				Repo        string   `json:"repo"`
				URL         string   `json:"url"`
				Ref         string   `json:"ref"`
				Path        string   `json:"path"`
				SparsePaths []string `json:"sparsePaths"`
			} `json:"source"`
		} `json:"extraKnownMarketplaces"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", claudeSettingsFile, err)
	}
	out := make(map[string]settingsMarketplace, len(raw.Extra))
	for name, v := range raw.Extra {
		s := v.Source
		out[name] = settingsMarketplace{Kind: s.Source, Repo: s.Repo, URL: s.URL, Ref: s.Ref, Path: s.Path, SparsePaths: s.SparsePaths}
	}
	return out, nil
}

// externalMarketplace is a settings marketplace that resolved to a GitHub
// repository other than the reviewed one, with the ref it asked for.
type externalMarketplace struct {
	Repo string
	Ref  string
}

// resolveSettingsMarketplace decides what an extraKnownMarketplaces entry is.
// sameRepo is true for the reviewed repository itself, which is the in-repo
// marketplace read at the base SHA (the entry's ref is ignored). Only `github`
// and `git` sources on repoPluginHost with no sub-path are supported; a
// directory, file, url, settings, npm or any other kind — and a path or
// sparsePaths — is refused with the reason.
func resolveSettingsMarketplace(m settingsMarketplace, reviewedRepo string) (ext externalMarketplace, sameRepo bool, err error) {
	var repoStr string
	switch m.Kind {
	case "github":
		repoStr = m.Repo
	case "git":
		repoStr = m.URL
	default:
		return externalMarketplace{}, false, fmt.Errorf("the marketplace source kind %q is not supported (only github and git on %s)", sanitizeNoticeText(m.Kind), repoPluginHost)
	}
	repo, perr := parseGitHubRepo(repoStr)
	if perr != nil {
		return externalMarketplace{}, false, perr
	}
	if m.Path != "" || len(m.SparsePaths) > 0 {
		return externalMarketplace{}, false, errors.New("a marketplace path or sparsePaths is not supported")
	}
	if strings.EqualFold(repo, reviewedRepo) {
		return externalMarketplace{}, true, nil
	}
	return externalMarketplace{Repo: repo, Ref: strings.TrimSpace(m.Ref)}, false, nil
}

// --- plugin source ----------------------------------------------------------

// pluginSource is where a marketplace entry says its plugin lives, before any
// ref is resolved.
type pluginSource struct {
	// Relative is true for a path inside the marketplace's own repository,
	// which is fixed by the marketplace's own commit; Repo/Ref/SHA are unused.
	Relative bool
	Repo     string // external: owner/repo
	Path     string // directory within the repo ("" is the repo root)
	Ref      string // external: the branch/tag/40-hex the entry asked for
	SHA      string // external: a 40-hex commit the entry pinned
}

// cleanSourcePath normalises a plugin directory path within a repo and refuses
// anything that is not a plain relative path. "" and "." are the repo root.
func cleanSourcePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." || p == "./" {
		return "", nil
	}
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	if why := cleanPluginPath(p); why != "" {
		return "", fmt.Errorf("the plugin path %q is not usable (%s)", sanitizeNoticeText(p), why)
	}
	return path.Clean(p), nil
}

// resolvePluginSource reads a marketplace entry's source. pluginRoot is the
// manifest's metadata.pluginRoot, applied to a bare-name source only.
func resolvePluginSource(raw json.RawMessage, pluginRoot string) (pluginSource, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return pluginSource{}, errors.New("the marketplace entry has no source")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return pluginSource{}, errors.New("the marketplace entry's source is malformed")
		}
		switch {
		case s == ".", strings.HasPrefix(s, "./"):
			p, err := cleanSourcePath(s)
			return pluginSource{Relative: true, Path: p}, err
		case pluginRoot != "" && idPartRE.MatchString(s):
			root, err := cleanSourcePath(pluginRoot)
			if err != nil {
				return pluginSource{}, err
			}
			return pluginSource{Relative: true, Path: path.Join(root, s)}, nil
		}
		return pluginSource{}, fmt.Errorf("the relative source %q must start with ./", sanitizeNoticeText(s))
	}
	var o struct {
		Source string `json:"source"`
		Repo   string `json:"repo"`
		URL    string `json:"url"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		SHA    string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return pluginSource{}, errors.New("the marketplace entry's source is malformed")
	}
	var repoStr string
	switch o.Source {
	case "github":
		repoStr = o.Repo
	case "git-subdir", "url":
		repoStr = o.URL
		if repoStr == "" {
			repoStr = o.Repo
		}
	default:
		return pluginSource{}, fmt.Errorf("the plugin source kind %q is not supported (only a relative path, github and git-subdir on %s)", sanitizeNoticeText(o.Source), repoPluginHost)
	}
	repo, err := parseGitHubRepo(repoStr)
	if err != nil {
		return pluginSource{}, err
	}
	p := ""
	if o.Source == "git-subdir" {
		if p, err = cleanSourcePath(o.Path); err != nil {
			return pluginSource{}, err
		}
	} else if o.Path != "" {
		return pluginSource{}, fmt.Errorf("a path on a %s source is not supported", o.Source)
	}
	return pluginSource{Repo: repo, Path: p, Ref: strings.TrimSpace(o.Ref), SHA: strings.TrimSpace(o.SHA)}, nil
}

// pinRequest says how a ref must be turned into a fixed commit.
type pinRequest struct {
	Commit  string // already a fixed 40-hex commit
	Resolve string // otherwise: a ref name to resolve (a tag is accepted, a branch refused)
}

// pinFor applies the fixed-commit rule (FR-009): a 40-hex sha, or a 40-hex ref,
// is used as is (sha wins when both are set, as in Claude Code); a named ref
// must be resolved by the caller; no ref at all would float on the default
// branch and is refused.
func pinFor(sha, ref string) (pinRequest, error) {
	switch {
	case sha != "":
		if !sha40RE.MatchString(sha) {
			return pinRequest{}, fmt.Errorf("the pinned sha %q is not a full 40-character commit SHA", sanitizeNoticeText(sha))
		}
		return pinRequest{Commit: strings.ToLower(sha)}, nil
	case ref == "":
		return pinRequest{}, errors.New("no sha or ref is given, so the source would follow the default branch; pin it to a commit SHA or a tag")
	case sha40RE.MatchString(ref):
		return pinRequest{Commit: strings.ToLower(ref)}, nil
	}
	return pinRequest{Resolve: ref}, nil
}
