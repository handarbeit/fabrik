package github

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// labelDef holds the name, human-readable description, and default creation
// color for a Fabrik-managed label. Descriptions must be ≤100 characters.
type labelDef struct {
	name        string
	description string
	color       string
}

// staticLabelDefs lists every static and enumerated Fabrik label with its
// description (≤100 chars) and a sensible default color for first creation.
// Colors are never changed after a label is created.
var staticLabelDefs = []labelDef{
	// --- behaviour labels (blue) ---
	{"fabrik:yolo", "Auto-advance all stages and auto-merge the PR at Validate", "0075ca"},
	{"fabrik:cruise", "Auto-advance all stages; stop at Validate without merging", "0075ca"},
	{"fabrik:extend-turns", "Override: pre-grant 2× turn budget; auto-removed on stage success", "0075ca"},

	// --- warning / waiting labels (yellow) ---
	{"fabrik:paused", "Stage failed or needs intervention; remove to resume", "e4e669"},
	{"fabrik:awaiting-input", "Stage blocked on FABRIK_BLOCKED_ON_INPUT; comment to unblock", "e4e669"},
	{"fabrik:awaiting-review", "Validate complete; waiting for requested PR reviewers", "e4e669"},
	{"fabrik:awaiting-ci", "CI gate active; waiting for CI checks to pass (checks may be running or have failed)", "e4e669"},
	{"fabrik:rebase-needed", "Base branch advanced and PR no longer merges; engine is retrying a rebase invocation", "e4e669"},
	{"fabrik:bot-reprompted", "Bot re-prompt sent; waiting for bot to respond (transient; removed at gate-cycle end)", "e4e669"},

	// --- danger labels (red) ---
	{"fabrik:blocked", "Blocked by one or more open dependency issues", "d73a4a"},
	{"fabrik:unrestricted", "Claude runs with --dangerously-skip-permissions for this issue", "d73a4a"},

	// --- neutral labels (grey) ---
	{"fabrik:editing", "Issue is being edited by the user; processing deferred", "cfd3d7"},

	// --- model override labels (purple) ---
	{"model:opus", "Override the stage's configured Claude model", "6f42c1"},
	{"model:sonnet", "Override the stage's configured Claude model", "6f42c1"},
	{"model:haiku", "Override the stage's configured Claude model", "6f42c1"},

	// --- effort override labels (purple) ---
	{"effort:low", "Override the stage's configured thinking effort", "6f42c1"},
	{"effort:medium", "Override the stage's configured thinking effort", "6f42c1"},
	{"effort:high", "Override the stage's configured thinking effort", "6f42c1"},
	{"effort:max", "Override the stage's configured thinking effort", "6f42c1"},
}

// labelDefFor returns the description and default color for labelName.
// Static and enumerated labels are looked up from staticLabelDefs.
// Dynamic patterns (stage:<name>:*, fabrik:locked:<user>) are derived.
// Falls back to empty description and purple ("6f42c1") for unknown labels.
func labelDefFor(name string) (description, color string) {
	// Static / enumerated lookup first.
	for _, d := range staticLabelDefs {
		if d.name == name {
			return d.description, d.color
		}
	}

	// stage:<name>:in_progress | stage:<name>:complete | stage:<name>:failed
	if strings.HasPrefix(name, "stage:") {
		parts := strings.SplitN(name, ":", 3)
		if len(parts) == 3 {
			stageName := parts[1]
			switch parts[2] {
			case "in_progress":
				return fmt.Sprintf("Fabrik is currently running the %s stage", stageName), "0e8a16"
			case "complete":
				return fmt.Sprintf("%s stage completed successfully", stageName), "0e8a16"
			case "failed":
				return fmt.Sprintf("%s stage failed after max_retries attempts", stageName), "d73a4a"
			}
		}
	}

	// fabrik:locked:<user>
	if strings.HasPrefix(name, "fabrik:locked:") {
		return "Another Fabrik instance is processing this issue", "cfd3d7"
	}

	return "", "6f42c1"
}

// FetchLabels returns the current labels on an issue.
func (c *Client) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", c.baseURL, owner, repo, issueNumber)
	var labels []struct {
		Name string `json:"name"`
	}
	if err := c.restGetJSON(apiURL, &labels); err != nil {
		return nil, err
	}
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names, nil
}

// AddLabelToIssue adds a label to an issue. Creates the label if it doesn't exist.
func (c *Client) AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error {
	desc, color := labelDefFor(labelName)
	// First ensure the label exists with the right description and color.
	if err := c.ensureLabel(owner, repo, labelName, desc, color); err != nil {
		return err
	}

	// Use REST API for simplicity — add label to issue
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", c.baseURL, owner, repo, issueNumber)
	body := map[string]interface{}{
		"labels": []string{labelName},
	}
	return c.restPost(apiURL, body)
}

// RemoveLabelFromIssue removes a label from an issue.
func (c *Client) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels/%s", c.baseURL, owner, repo, issueNumber, url.PathEscape(labelName))
	return c.restDelete(apiURL)
}

// FetchLabelAppliedAt returns the time when labelName was last applied to the
// given issue, using the GitHub issue events API. Returns time.Time{} (zero)
// without error if the event is not found (fail-open: timeout will not fire).
// Pages through all events to find the most recent application.
func (c *Client) FetchLabelAppliedAt(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
	var applied time.Time
	for page := 1; ; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/events?per_page=100&page=%d",
			c.baseURL, owner, repo, issueNumber, page)
		var events []struct {
			Event     string `json:"event"`
			CreatedAt string `json:"created_at"`
			Label     *struct {
				Name string `json:"name"`
			} `json:"label"`
		}
		if err := c.restGetJSON(apiURL, &events); err != nil {
			return time.Time{}, fmt.Errorf("fetching issue events for #%d: %w", issueNumber, err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Event != "labeled" || ev.Label == nil || ev.Label.Name != labelName {
				continue
			}
			t, err := time.Parse(time.RFC3339, ev.CreatedAt)
			if err != nil {
				continue
			}
			if t.After(applied) {
				applied = t
			}
		}
	}
	return applied, nil
}

// ensureLabel creates a label with the given description and color if it does
// not already exist. A 422 (label already exists) is silently ignored.
// description and color are only used on creation (POST); existing labels are
// never modified by this function.
func (c *Client) ensureLabel(owner, repo, name, description, color string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/labels", c.baseURL, owner, repo)
	body := map[string]interface{}{
		"name":        name,
		"color":       color,
		"description": description,
	}
	// Ignore 422 (label already exists); propagate all other errors.
	if err := c.restPost(apiURL, body); err != nil && !errors.Is(err, ErrUnprocessableEntity) {
		return err
	}
	return nil
}

// ErrNoRepoConfigured is returned by SeedLabels when repo is empty.
var ErrNoRepoConfigured = errors.New("repo must not be empty")

// SeedLabels ensures all known Fabrik labels exist on the given repo and that
// any label with an empty description is backfilled. It never changes an
// existing label's color or non-empty description. stageNames is the list of
// stage names from the loaded config; lockedUser is the current Fabrik user.
// Returns ErrNoRepoConfigured when repo is empty. Per-label failures are logged
// internally and do not cause an early return.
func (c *Client) SeedLabels(owner, repo string, stageNames []string, lockedUser string) error {
	if repo == "" {
		return ErrNoRepoConfigured
	}

	// Collect all labels to seed.
	defs := make([]labelDef, 0, len(staticLabelDefs)+len(stageNames)*3+1)
	defs = append(defs, staticLabelDefs...)

	// fabrik:locked:<user>
	lockedName := fmt.Sprintf("fabrik:locked:%s", lockedUser)
	defs = append(defs, labelDef{lockedName, "Another Fabrik instance is processing this issue", "cfd3d7"})

	// stage:<name>:in_progress, stage:<name>:complete, stage:<name>:failed
	for _, s := range stageNames {
		defs = append(defs,
			labelDef{
				fmt.Sprintf("stage:%s:in_progress", s),
				fmt.Sprintf("Fabrik is currently running the %s stage", s),
				"0e8a16",
			},
			labelDef{
				fmt.Sprintf("stage:%s:complete", s),
				fmt.Sprintf("%s stage completed successfully", s),
				"0e8a16",
			},
			labelDef{
				fmt.Sprintf("stage:%s:failed", s),
				fmt.Sprintf("%s stage failed after max_retries attempts", s),
				"d73a4a",
			},
		)
	}

	for _, d := range defs {
		if err := c.seedOneLabel(owner, repo, d); err != nil {
			logf(0, "warn", "seeding label %q: %v\n", d.name, err)
		}
	}
	return nil
}

// seedOneLabel creates a label if it does not exist, or backfills its
// description if it exists with an empty description. Color is never changed.
func (c *Client) seedOneLabel(owner, repo string, d labelDef) error {
	getURL := fmt.Sprintf("%s/repos/%s/%s/labels/%s", c.baseURL, owner, repo, url.PathEscape(d.name))

	var existing struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	err := c.restGetJSON(getURL, &existing)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Label does not exist — create it.
			createURL := fmt.Sprintf("%s/repos/%s/%s/labels", c.baseURL, owner, repo)
			body := map[string]interface{}{
				"name":        d.name,
				"color":       d.color,
				"description": d.description,
			}
			if postErr := c.restPost(createURL, body); postErr != nil && !errors.Is(postErr, ErrUnprocessableEntity) {
				return postErr
			}
			return nil
		}
		return err
	}

	// Label exists. Backfill description only if currently empty.
	if existing.Description != "" {
		return nil
	}
	patchURL := fmt.Sprintf("%s/repos/%s/%s/labels/%s", c.baseURL, owner, repo, url.PathEscape(d.name))
	patch := map[string]string{"description": d.description}
	return c.restPatch(patchURL, patch)
}
