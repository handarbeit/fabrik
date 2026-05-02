package cmd

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/verveguy/fabrik/stages"
	"gopkg.in/yaml.v3"
)

// runRefreshStages implements the `fabrik refresh-stages` subcommand.
func runRefreshStages(args []string) error {
	fset := flag.NewFlagSet("refresh-stages", flag.ContinueOnError)
	var apply, interactive bool
	fset.BoolVar(&apply, "apply", false, "Write missing keys into user stage YAML files")
	fset.BoolVar(&interactive, "interactive", false, "Prompt y/n before applying each stage (requires --apply)")
	fset.Usage = func() {
		fmt.Fprintf(fset.Output(), "Usage: fabrik refresh-stages [--apply] [--interactive]\n\n")
		fmt.Fprintf(fset.Output(), "Show (or apply) missing top-level YAML keys from embedded defaults.\n\n")
		fmt.Fprintf(fset.Output(), "By default (no flags): print a unified-diff preview of what would be added.\n")
		fmt.Fprintf(fset.Output(), "  --apply          Add missing keys to user stage YAMLs (never removes or overwrites).\n")
		fmt.Fprintf(fset.Output(), "  --interactive    Prompt y/n before applying each stage (requires --apply).\n")
	}
	if err := fset.Parse(args); err != nil {
		return err
	}

	if interactive && !apply {
		return fmt.Errorf("--interactive requires --apply")
	}

	isTTY := isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
	return refreshStagesWithReader("./.fabrik/stages", apply, interactive, isTTY, os.Stdin, os.Stdout, stages.DefaultStages)
}

// defaultNodeIndex holds the parsed yaml.Node data for a single embedded default stage.
type defaultNodeIndex struct {
	keySet    map[string]bool
	nodesByKey map[string][2]*yaml.Node // key string → [keyNode, valueNode]
}

// refreshStagesWithReader is the testable core of runRefreshStages.
func refreshStagesWithReader(
	stagesDir string,
	apply, interactive, isTTY bool,
	r io.Reader,
	w io.Writer,
	defaults fs.FS,
) error {
	// Build a map from stage name → defaultNodeIndex by walking the embedded FS.
	defaultsByName, err := buildDefaultNodeIndex(defaults)
	if err != nil {
		return fmt.Errorf("reading embedded defaults: %w", err)
	}

	// Read user stage files from stagesDir.
	entries, err := os.ReadDir(stagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("stages directory %q does not exist", stagesDir)
		}
		return fmt.Errorf("reading stages directory: %w", err)
	}

	// Create the input reader once; buffering per-prompt would swallow subsequent stage input.
	inputReader := bufio.NewReader(r)

	var applyErr error
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		userPath := filepath.Join(stagesDir, entry.Name())

		// Quick read to get the stage name.
		userBytes, err := os.ReadFile(userPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", userPath, err)
		}

		var userMap map[string]any
		if err := yaml.Unmarshal(userBytes, &userMap); err != nil {
			return fmt.Errorf("parsing %s: %w", userPath, err)
		}

		name, _ := userMap["name"].(string)
		if name == "" {
			continue // skip files without a name
		}

		idx, ok := defaultsByName[name]
		if !ok {
			continue // custom stage — silently skip
		}

		missing, err := stages.MissingTopLevelKeys(userPath, idx.keySet)
		if err != nil {
			return fmt.Errorf("checking drift for %s: %w", userPath, err)
		}
		if len(missing) == 0 {
			continue // no drift — silently skip
		}

		// Render the diff output for this stage.
		diff := renderStageDiff(userPath, missing, idx)

		if !apply {
			// Dry-run: print and continue.
			fmt.Fprint(w, diff)
			continue
		}

		if interactive {
			// Show diff and prompt.
			fmt.Fprint(w, diff)
			fmt.Fprintf(w, "Apply changes to %s? [y/N] ", userPath)
			line, _ := inputReader.ReadString('\n')
			answer := strings.TrimSpace(line)
			if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
				continue
			}
		}

		// Apply: parse user file to *yaml.Node, append missing key-value pairs.
		if err := applyMissingKeys(userPath, missing, idx); err != nil {
			// Report but continue with remaining stages; track that an error occurred.
			fmt.Fprintf(w, "error applying to %s: %v\n", userPath, err)
			applyErr = err
			continue
		}
		if !interactive {
			fmt.Fprintf(w, "updated %s: added %d field(s): %s\n", userPath, len(missing), strings.Join(missing, ", "))
		}
	}

	return applyErr
}

// buildDefaultNodeIndex walks the embedded defaults FS and builds a map from
// stage name → defaultNodeIndex (containing the default key set and node pairs).
func buildDefaultNodeIndex(defaults fs.FS) (map[string]*defaultNodeIndex, error) {
	result := make(map[string]*defaultNodeIndex)

	err := fs.WalkDir(defaults, "examples", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(d.Name())
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		f, err := defaults.Open(path)
		if err != nil {
			return nil // best-effort
		}
		defer f.Close()

		data, err := io.ReadAll(f)
		if err != nil {
			return nil
		}

		// Parse to yaml.Node for value extraction.
		var docNode yaml.Node
		if err := yaml.Unmarshal(data, &docNode); err != nil {
			return nil
		}
		if docNode.Kind == 0 || len(docNode.Content) == 0 {
			return nil
		}
		mappingNode := docNode.Content[0]
		if mappingNode.Kind != yaml.MappingNode {
			return nil
		}

		idx := &defaultNodeIndex{
			keySet:    make(map[string]bool),
			nodesByKey: make(map[string][2]*yaml.Node),
		}

		// Walk alternating key/value pairs in the mapping.
		for i := 0; i+1 < len(mappingNode.Content); i += 2 {
			kn := mappingNode.Content[i]
			vn := mappingNode.Content[i+1]
			key := kn.Value
			idx.keySet[key] = true
			idx.nodesByKey[key] = [2]*yaml.Node{kn, vn}
		}

		// Extract name for matching.
		namePair, ok := idx.nodesByKey["name"]
		if !ok {
			return nil
		}
		name := namePair[1].Value
		if name == "" {
			return nil
		}

		result[name] = idx
		return nil
	})
	return result, err
}

// renderStageDiff builds the unified-diff preview string for a single drifted stage.
func renderStageDiff(userPath string, missing []string, idx *defaultNodeIndex) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — %d missing field(s):\n", userPath, len(missing))
	for _, key := range missing {
		pair, ok := idx.nodesByKey[key]
		if !ok {
			continue
		}
		sb.WriteString(renderDiff(pair[0], pair[1]))
	}
	return sb.String()
}

// renderDiff marshals a single key-value node pair as a +‑prefixed YAML fragment.
func renderDiff(keyNode, valueNode *yaml.Node) string {
	tmp := &yaml.Node{
		Kind:    yaml.MappingNode,
		Content: []*yaml.Node{keyNode, valueNode},
	}
	data, err := yaml.Marshal(tmp)
	if err != nil {
		return fmt.Sprintf("+ %s: <marshal error: %v>\n", keyNode.Value, err)
	}

	raw := string(data)
	lines := strings.Split(raw, "\n")
	// yaml.Marshal always appends a trailing newline; drop the resulting empty last element.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var sb strings.Builder
	for _, line := range lines {
		sb.WriteString("+ ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// applyMissingKeys reads the user YAML file into a *yaml.Node, appends the
// missing key-value node pairs from the embedded default, and writes back.
func applyMissingKeys(userPath string, missing []string, idx *defaultNodeIndex) error {
	data, err := os.ReadFile(userPath)
	if err != nil {
		return fmt.Errorf("reading: %w", err)
	}

	var docNode yaml.Node
	if err := yaml.Unmarshal(data, &docNode); err != nil {
		return fmt.Errorf("parsing: %w", err)
	}
	if docNode.Kind == 0 || len(docNode.Content) == 0 {
		return fmt.Errorf("unexpected empty document")
	}
	mappingNode := docNode.Content[0]
	if mappingNode.Kind != yaml.MappingNode {
		return fmt.Errorf("top-level element is not a mapping")
	}

	for _, key := range missing {
		pair, ok := idx.nodesByKey[key]
		if !ok {
			continue
		}
		mappingNode.Content = append(mappingNode.Content, pair[0], pair[1])
	}

	out, err := yaml.Marshal(mappingNode)
	if err != nil {
		return fmt.Errorf("marshaling: %w", err)
	}

	if err := os.WriteFile(userPath, out, 0644); err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	return nil
}

