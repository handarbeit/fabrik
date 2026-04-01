package stages

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Stage defines a single workflow stage and its Claude Code configuration.
type Stage struct {
	// Name of the stage — must match a Project board column/status value.
	Name string `yaml:"name"`

	// Order determines processing priority (lower = earlier in pipeline).
	Order int `yaml:"order"`

	// Prompt is the system prompt sent to Claude Code for this stage.
	Prompt string `yaml:"prompt"`

	// Model to use (e.g., "opus", "sonnet"). Optional.
	Model string `yaml:"model,omitempty"`

	// AllowedTools restricts which tools Claude Code can use. Empty = all.
	AllowedTools []string `yaml:"allowed_tools,omitempty"`

	// MaxTurns limits how many turns Claude Code can take per invocation.
	MaxTurns int `yaml:"max_turns,omitempty"`

	// CompletionCriteria defines when this stage is "done".
	Completion CompletionCriteria `yaml:"completion"`

	// AutoAdvance overrides the global yolo setting for this specific stage.
	// nil means use the global setting.
	AutoAdvance *bool `yaml:"auto_advance,omitempty"`
}

// CompletionCriteria defines how to determine if a stage is complete.
type CompletionCriteria struct {
	// Type is the kind of completion check: "tasklist", "label", "approval", or "claude".
	// - "tasklist": all checkbox items in the issue body are checked
	// - "label": a specific label is present on the issue
	// - "approval": a comment containing an approval keyword exists
	// - "claude": Claude Code decides when it's done (default)
	Type string `yaml:"type"`

	// Value is type-specific: label name for "label", keyword for "approval".
	Value string `yaml:"value,omitempty"`
}

// LoadAll reads all .yaml/.yml files from dir and returns them sorted by Order.
func LoadAll(dir string) ([]*Stage, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("stages directory %q does not exist", dir)
		}
		return nil, err
	}

	var stages []*Stage
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		path := filepath.Join(dir, e.Name())
		s, err := loadOne(path)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", path, err)
		}
		stages = append(stages, s)
	}

	sort.Slice(stages, func(i, j int) bool {
		return stages[i].Order < stages[j].Order
	})

	return stages, nil
}

func loadOne(path string) (*Stage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var s Stage
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	if s.Name == "" {
		return nil, fmt.Errorf("stage must have a 'name' field")
	}

	if s.Prompt == "" {
		return nil, fmt.Errorf("stage %q must have a 'prompt' field", s.Name)
	}

	if s.Completion.Type == "" {
		s.Completion.Type = "claude"
	}

	return &s, nil
}

// NextStage returns the stage after the given one, or nil if it's the last.
func NextStage(stages []*Stage, current string) *Stage {
	for i, s := range stages {
		if s.Name == current && i+1 < len(stages) {
			return stages[i+1]
		}
	}
	return nil
}

// FindStage returns the stage with the given name, or nil.
func FindStage(stages []*Stage, name string) *Stage {
	for _, s := range stages {
		if s.Name == name {
			return s
		}
	}
	return nil
}
