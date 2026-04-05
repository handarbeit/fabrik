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
	// Optional when Skill is set — the skill provides the instructions.
	Prompt string `yaml:"prompt,omitempty"`

	// Skill names a Fabrik plugin skill (e.g., "fabrik-research") that Claude
	// should follow for this stage. When set, the engine sends a directive
	// prompt instead of the Prompt field. The skill must be installed in the
	// user's Claude Code plugin configuration.
	Skill string `yaml:"skill,omitempty"`

	// Model to use (e.g., "opus", "sonnet"). Optional.
	Model string `yaml:"model,omitempty"`

	// AllowedTools restricts which tools Claude Code can use. Empty = all.
	AllowedTools []string `yaml:"allowed_tools,omitempty"`

	// ReadOnly marks this stage as read-only. When true, the engine will stash
	// any dirty worktree state before invoking Claude and restore it afterward.
	ReadOnly bool `yaml:"read_only,omitempty"`

	// MaxTurns limits how many turns Claude Code can take per invocation.
	MaxTurns int `yaml:"max_turns,omitempty"`

	// CommentMaxTurns limits how many turns Claude Code can take when processing
	// user comments. When 0 (unset), defaults to min(MaxTurns, 15) or 15 if
	// MaxTurns is also 0. This keeps comment processing bounded independently
	// of the main stage turn budget.
	CommentMaxTurns int `yaml:"comment_max_turns,omitempty"`

	// CommentPrompt is the prompt used when processing user comments during this stage.
	// If empty, a default comment-processing prompt is used.
	CommentPrompt string `yaml:"comment_prompt,omitempty"`

	// CommentSkill names a Fabrik plugin skill to use when processing user comments
	// during this stage. When set, the engine sends a directive prompt instead of
	// CommentPrompt. Takes precedence over CommentPrompt if both are set.
	CommentSkill string `yaml:"comment_skill,omitempty"`

	// CompletionCriteria defines when this stage is "done".
	Completion CompletionCriteria `yaml:"completion"`

	// PostToPR routes stage output to the linked PR instead of the issue.
	// A brief summary is still posted on the issue.
	PostToPR bool `yaml:"post_to_pr,omitempty"`

	// CreateDraftPR causes the engine to push the branch and create a draft PR
	// (linked to the issue) before invoking Claude. Idempotent with respect to
	// open PRs — skipped if an open PR already exists for the issue branch.
	CreateDraftPR bool `yaml:"create_draft_pr,omitempty"`

	// MarkPRReadyOnComplete causes the engine to push the branch and mark the PR
	// as ready-for-review after the stage signals completion. This transitions
	// the draft PR and triggers external review bots.
	MarkPRReadyOnComplete bool `yaml:"mark_pr_ready_on_complete,omitempty"`

	// UpdateIssueBody allows this stage to update the issue body via
	// FABRIK_ISSUE_UPDATE markers. Only the Specify stage should have this
	// set to true — other stages post output as stage comments.
	UpdateIssueBody bool `yaml:"update_issue_body,omitempty"`

	// AutoAdvance overrides the global yolo setting for this specific stage.
	// nil means use the global setting.
	AutoAdvance *bool `yaml:"auto_advance,omitempty"`

	// CleanupWorktree causes the engine to remove the issue's worktree directory
	// instead of invoking Claude. No prompt, lock, or in_progress label is needed.
	// Use this for terminal stages like "Done" to reclaim disk space.
	CleanupWorktree bool `yaml:"cleanup_worktree,omitempty"`
}

// CompletionCriteria defines how to determine if a stage is complete.
type CompletionCriteria struct {
	// Type is the kind of completion check. Currently only "claude" is supported.
	// - "claude": Claude Code signals completion by outputting FABRIK_STAGE_COMPLETE (default)
	Type string `yaml:"type"`

	// Value is reserved for future completion types.
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

	if !s.CleanupWorktree {
		if s.Prompt == "" && s.Skill == "" {
			return nil, fmt.Errorf("stage %q must have a 'prompt' or 'skill' field", s.Name)
		}

		if s.Completion.Type == "" {
			s.Completion.Type = "claude"
		} else if s.Completion.Type != "claude" {
			return nil, fmt.Errorf("stage %q: unsupported completion type %q (only \"claude\" is supported)", s.Name, s.Completion.Type)
		}
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
