package pruefer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/handarbeit/fabrik/internal/skillref"
	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

// reviewPlugin is the per-review plugin directory (#2306, R1): a fresh root
// holding the embedded pruefer-review plugin and, below RepoRoot, any repo
// plugin trees (#2307), never shared between reviews, plus the resolution of
// the default review skill against exactly the embedded plugin (R2).
type reviewPlugin struct {
	Root    string // the per-review directory, removed by Close
	Dir     string // the embedded plugin, symlink-resolved; the same path --plugin-dir and the resolver see
	Ref     string // fully-qualified default skill reference
	Plugin  string // plugin the skill came from
	Version string
	cleanup func()
}

// RepoRoot is where repo plugin trees are written, inside Root.
func (p *reviewPlugin) RepoRoot() string { return filepath.Join(p.Root, "repo") }

// Close removes the directory. Safe to call more than once.
func (p *reviewPlugin) Close() {
	if p != nil && p.cleanup != nil {
		p.cleanup()
		p.cleanup = nil
	}
}

// setupReviewPlugin writes the embedded plugin to a new temporary directory and
// resolves the default review skill against it with internal/skillref. It
// fails closed: a plugin that cannot be written or does not resolve means no
// review (Pruefer posts nothing on invocation failure). The directory is
// removed before returning an error.
func setupReviewPlugin() (*reviewPlugin, error) {
	tmp, err := os.MkdirTemp("", "pruefer-review-plugin-*")
	if err != nil {
		return nil, fmt.Errorf("creating review plugin directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("resolving review plugin directory: %w", err)
	}
	dir := filepath.Join(root, reviewplugin.PluginName)
	if err := os.Mkdir(dir, 0o755); err != nil {
		cleanup()
		return nil, fmt.Errorf("creating review plugin directory: %w", err)
	}
	if err := reviewplugin.Write(dir); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing review plugin: %w", err)
	}
	res, why, cat, ok := skillref.NewResolver([]string{dir}, skillref.Options{}).Resolve(reviewplugin.DefaultRef)
	if !ok {
		cleanup()
		return nil, fmt.Errorf("review skill %q is unresolved: %s; plugins searched: %s", reviewplugin.DefaultRef, why, cat.Describe())
	}
	pluginName := reviewplugin.PluginName
	if pn, found := pluginOf(cat, res.Name); found {
		pluginName = pn
	}
	return &reviewPlugin{Root: root, Dir: dir, Ref: res.Name, Plugin: pluginName, Version: reviewplugin.Version(), cleanup: cleanup}, nil
}

func pluginOf(cat *skillref.Catalogue, ref string) (string, bool) {
	if i := strings.Index(ref, ":"); i > 0 {
		if p, ok := cat.Plugin(ref[:i]); ok {
			return p.Name, true
		}
	}
	return "", false
}

// skillLogLine is the line a review logs for the skill it ran and the plugin it
// came from (#2306 R6, generalised by #2307).
func skillLogLine(ref string, p loadedPlugin, replacesDefault bool) string {
	s := fmt.Sprintf("review skill: %s (plugin %s v%s, %s, dir %s)", ref, p.Name, p.version(), p.Source, p.Dir)
	if replacesDefault {
		s += "; operator/repo guidance replaces the skill's criteria"
	}
	return s
}

// renderSkillSection writes the skill-invocation instruction and any operator
// or repo guidance, in the slot the composed guidance text used to occupy
// (between the dynamic context and the Go-owned output contract). The skill is
// the default layer; operator and repo guidance are additional guidance
// passed to it in the prompt (R5). Output format stays in renderContract (R4).
func renderSkillSection(b *strings.Builder, ref, guidance string, replacesDefault bool) {
	fmt.Fprintf(b, "Carry out this review by invoking the `%s` skill with the Skill tool, before you read anything else. Its instructions are your review criteria: follow them for what to look for, how to verify and how to weigh findings. It does not define the output format; the output contract below does, and it applies to your final message.\n\n", ref)
	if strings.TrimSpace(guidance) == "" {
		return
	}
	if replacesDefault {
		b.WriteString("## Review guidance that replaces the skill's criteria\n\n")
		b.WriteString("The following guidance replaces the skill's own review criteria. Use the skill only for the method it shares with every review; wherever this guidance and the skill differ, follow this guidance.\n\n")
	} else {
		b.WriteString("## Additional review guidance\n\n")
		b.WriteString("Apply the following in addition to the skill's instructions while you carry out the review.\n\n")
	}
	b.WriteString(guidance)
	b.WriteString("\n\n")
}

// skillCalls scans the stream-json transcript for main-thread Skill tool calls
// naming ref. sawEvents reports that the output is a real event stream (it has
// system or assistant events), as opposed to a bare result object.
func skillCalls(raw []byte, ref string) (called, sawEvents bool) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), len(raw)+1)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type            string          `json:"type"`
			ParentToolUseID json.RawMessage `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if env.Type == "system" || env.Type == "assistant" {
			sawEvents = true
		}
		if env.Type != "assistant" {
			continue
		}
		if p := bytes.TrimSpace(env.ParentToolUseID); len(p) > 0 && !bytes.Equal(p, []byte("null")) {
			continue
		}
		var blocks []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(env.Message.Content, &blocks) != nil {
			continue
		}
		for _, bl := range blocks {
			if bl.Type != "tool_use" || bl.Name != "Skill" {
				continue
			}
			var in struct {
				Skill string `json:"skill"`
			}
			if json.Unmarshal(bl.Input, &in) == nil && skillref.SkillNamesMatch(ref, in.Skill) {
				called = true
			}
		}
	}
	return called, sawEvents
}

// checkSkillRan fails a review whose default skill demonstrably did not run:
// the Skill call errored, or the transcript is a real event stream with no
// call to it. Either way the reviewer did not follow the criteria, and nothing
// in the output contract would show it, so nothing is posted. A bare result
// object with no events cannot be judged and passes.
func checkSkillRan(raw []byte, ref string) error {
	if e := skillref.DetectLoadFailure(raw, ref); e != nil {
		return fmt.Errorf("review skill failed to load: %w", e)
	}
	called, sawEvents := skillCalls(raw, ref)
	if sawEvents && !called {
		return fmt.Errorf("reviewer never invoked the %s skill", ref)
	}
	return nil
}
