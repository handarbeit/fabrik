package claudeprofile

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BareEnv is Claude Code's own variable. It is exported by unrelated tools, so
// its presence says nothing about intent toward Fabrik, and it ranks below
// config.yaml (see Resolve).
const BareEnv = "CLAUDE_CONFIG_DIR"

// DeliberateEnv is the Fabrik-owned variable: an operator setting it means it.
const DeliberateEnv = "FABRIK_CLAUDE_CONFIG_DIR"

// Layer names one place a profile directory can come from. The string value is
// what operators read in the startup line and the warnings panel.
type Layer string

const (
	LayerFlag     Layer = "--claude-config-dir"
	LayerShellEnv Layer = DeliberateEnv
	LayerDotenv   Layer = ".env"
	LayerYAML     Layer = ".fabrik/config.yaml"
	LayerAmbient  Layer = BareEnv
)

// Names are the operator-facing spellings of the layers a daemon owns. The zero
// value is Fabrik's own (--claude-config-dir, FABRIK_CLAUDE_CONFIG_DIR,
// .fabrik/config.yaml); Pruefer sets its own so the startup line and the
// disagreement warning name what its operator actually typed (#2294). The
// precedence itself is not configurable.
type Names struct {
	// Flag is the command-line flag as written, e.g. "-claude-config-dir".
	Flag string
	// Env is the deliberate, daemon-owned variable. It is also the key looked up
	// in Inputs.Dotenv.
	Env string
	// YAML is the config file's name, e.g. ".pruefer/config.yaml".
	YAML string
}

func (n Names) withDefaults() Names {
	if n.Flag == "" {
		n.Flag = string(LayerFlag)
	}
	if n.Env == "" {
		n.Env = DeliberateEnv
	}
	if n.YAML == "" {
		n.YAML = string(LayerYAML)
	}
	return n
}

// Inputs carries every source as data; Resolve reads no environment, file or
// clock, so the caller decides what the process environment looked like.
type Inputs struct {
	// Flag is the --claude-config-dir value.
	Flag string
	// ShellFabrik is the deliberate variable (FABRIK_CLAUDE_CONFIG_DIR unless
	// Names.Env says otherwise) from the shell environment, captured before .env
	// was loaded.
	ShellFabrik string
	// Dotenv is the parsed .env file (read as data, never exported for these
	// two names). Only the deliberate name and BareEnv are consulted.
	Dotenv map[string]string
	// YAML is config.yaml's claude_config_dir.
	YAML string
	// Ambient is a bare CLAUDE_CONFIG_DIR inherited from the environment,
	// captured before .env was loaded.
	Ambient string
	// Home expands a leading "~" in any value; empty disables expansion.
	Home string
	// Cwd anchors a relative value, so every consumer sees one absolute
	// directory: a worker runs in its worktree, not the daemon's directory, and
	// would otherwise resolve ".claude-work" against a different base than the
	// startup line and footer did. Empty leaves relative values as written.
	Cwd string
	// Names overrides the layer spellings; the zero value is Fabrik's.
	Names Names
}

// Source is one layer that supplied a non-empty value.
type Source struct {
	Layer Layer
	// Name is the exact variable or key, e.g. ".env FABRIK_CLAUDE_CONFIG_DIR".
	Name string
	// Dir is the normalized directory.
	Dir string
	// Email is the account the profile is logged in as, when FillAccounts ran
	// and it was readable.
	Email string
}

// Resolution is the outcome: the effective directory, which layer won, and
// every layer that supplied a value (highest precedence first).
type Resolution struct {
	Dir     string
	Winner  Layer
	Sources []Source
}

// Resolve applies the precedence, highest first: flag; FABRIK_CLAUDE_CONFIG_DIR
// from the shell; .env (its FABRIK_ name beating the bare name); config.yaml;
// bare CLAUDE_CONFIG_DIR from the ambient environment; unset. An empty string at
// any layer means "not set".
func Resolve(in Inputs) Resolution {
	var r Resolution
	n := in.Names.withDefaults()
	add := func(layer Layer, name, raw string) {
		if raw == "" {
			return
		}
		dir := normalize(raw, in.Home, in.Cwd)
		if dir == "" {
			return
		}
		r.Sources = append(r.Sources, Source{Layer: layer, Name: name, Dir: dir})
	}
	add(LayerFlag, n.Flag, in.Flag)
	add(LayerShellEnv, n.Env, in.ShellFabrik)
	add(LayerDotenv, ".env "+n.Env, in.Dotenv[n.Env])
	add(LayerDotenv, ".env "+BareEnv, in.Dotenv[BareEnv])
	add(LayerYAML, n.YAML+" claude_config_dir", in.YAML)
	add(LayerAmbient, BareEnv+" (environment)", in.Ambient)
	if len(r.Sources) > 0 {
		r.Dir = r.Sources[0].Dir
		r.Winner = r.Sources[0].Layer
	}
	return r
}

// ResolveHost fills Home and Cwd from the process when the caller left them
// empty, resolves, and fills each source's account email. It is the one wrapper
// both daemons (fabrik, pruefer) call, so neither carries its own copy.
func ResolveHost(in Inputs) Resolution {
	if in.Home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			in.Home = home
		}
	}
	if in.Cwd == "" {
		if cwd, err := os.Getwd(); err == nil {
			in.Cwd = cwd
		}
	}
	return Resolve(in).FillAccounts(Account)
}

// normalize expands a leading "~", anchors a relative path at cwd and cleans
// it. Symlinks are not resolved, so two spellings of one directory through a
// link still differ.
func normalize(p, home, cwd string) string {
	p = strings.TrimSpace(p)
	if home != "" {
		switch {
		case p == "~":
			p = home
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, p[2:])
		}
	}
	if p == "" {
		return ""
	}
	if cwd != "" && !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// Disagree reports whether two or more sources name different directories.
// Sources naming the same directory are not a disagreement.
func (r Resolution) Disagree() bool {
	for _, s := range r.Sources {
		if s.Dir != r.Sources[0].Dir {
			return true
		}
	}
	return false
}

// Fingerprint is the sorted set of "name=dir" pairs. Account emails are left
// out on purpose: a login change must not resurface a dismissed warning.
func (r Resolution) Fingerprint() string {
	pairs := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		pairs = append(pairs, s.Name+"="+s.Dir)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\n")
}

// FillAccounts returns a copy of r with each source's Email set from lookup.
// It is separate from Resolve so that resolution itself does no I/O.
func (r Resolution) FillAccounts(lookup func(dir string) string) Resolution {
	out := r
	out.Sources = append([]Source(nil), r.Sources...)
	for i := range out.Sources {
		out.Sources[i].Email = lookup(out.Sources[i].Dir)
	}
	return out
}

// WinnerEmail is the winning source's account email, or "".
func (r Resolution) WinnerEmail() string {
	if len(r.Sources) == 0 {
		return ""
	}
	return r.Sources[0].Email
}
