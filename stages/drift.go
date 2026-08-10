package stages

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/handarbeit/fabrik/warnings"
	"gopkg.in/yaml.v3"
)

// WarnStageDrift compares each user stage against the embedded default with the
// same name. If the embedded default contains top-level YAML keys that the user's
// file does not, a warning is written to w naming the missing keys. Custom stages
// (no matching embedded default) are silently skipped. Empty userStages is a no-op.
func WarnStageDrift(userStages []*Stage, version string, w io.Writer) {
	warnDriftFrom(userStages, version, w, DefaultStages)
}

// WarnStageDriftFromFS is the fs.FS-parameterized form of WarnStageDrift,
// exported for callers outside this package that need to verify agreement
// against a synthetic set of embedded defaults (e.g. cmd's refresh-stages
// tests, which check that drift detection and refresh-stages report the
// same set of missing keys for the same fixtures).
func WarnStageDriftFromFS(userStages []*Stage, version string, w io.Writer, defaults fs.FS) {
	warnDriftFrom(userStages, version, w, defaults)
}

// warnDriftFrom is the testable core of WarnStageDrift. It accepts any fs.FS
// containing YAML stage files under "examples/", so tests can inject a synthetic FS.
func warnDriftFrom(userStages []*Stage, version string, w io.Writer, defaults fs.FS) {
	if len(userStages) == 0 {
		return
	}

	// Index user stages by name for fast lookup.
	byName := make(map[string]*Stage, len(userStages))
	for _, s := range userStages {
		byName[s.Name] = s
	}

	// Walk embedded defaults (best-effort; individual entry errors skip that entry).
	fs.WalkDir(defaults, "examples", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the walk
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(d.Name())
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := defaults.Open(path)
		if err != nil {
			return nil // best-effort; skip unreadable embedded file
		}
		defer data.Close()

		var buf []byte
		buf, err = io.ReadAll(data)
		if err != nil {
			return nil
		}

		var defaultMap map[string]any
		if err := yaml.Unmarshal(buf, &defaultMap); err != nil {
			return nil
		}

		name, _ := defaultMap["name"].(string)
		if name == "" {
			return nil
		}

		// Best-effort typed parse for value-aware equivalence checks. A failure
		// here degrades to the pre-existing structural (over-warning) behavior
		// rather than silently under-warning — defaultStage stays nil and
		// FilterNoOpKeys becomes a no-op.
		var defaultStage *Stage
		var typed Stage
		if err := yaml.Unmarshal(buf, &typed); err == nil {
			defaultStage = &typed
		}

		userStage, ok := byName[name]
		if !ok {
			return nil // custom stage — skip
		}
		if userStage.FilePath == "" {
			return nil // in-memory stage (e.g. tests) — skip
		}

		defaultKeys := make(map[string]bool, len(defaultMap))
		for k := range defaultMap {
			defaultKeys[k] = true
		}

		missing, err := MissingTopLevelKeys(userStage.FilePath, defaultKeys)
		if err != nil {
			return nil
		}
		missing = FilterNoOpKeys(missing, defaultStage)
		if len(missing) == 0 {
			_ = warnings.Clear("stage_drift:" + name)
			return nil
		}

		fmt.Fprintf(w, "[startup] warning: %s is missing fields present in %s defaults: %s. Run `fabrik refresh-stages --apply` to add the missing keys.\n",
			userStage.FilePath, version, strings.Join(missing, ", "))
		_ = warnings.Record(warnings.Entry{
			Key:       "stage_drift:" + name,
			Type:      "stage_drift",
			Title:     driftTitle(userStage.FilePath, missing),
			Detail:    fmt.Sprintf("%s is missing fields present in %s defaults: %s\n\nFix: fabrik refresh-stages --apply", userStage.FilePath, version, strings.Join(missing, ", ")),
			FixAction: "fabrik_command",
			FixParams: map[string]string{"args": "refresh-stages --apply"},
		})
		return nil
	})
}

// WarnUndeclaredReviewers emits a self-limiting startup notice (FR-7) for
// each stage with wait_for_reviews: true and no expected_reviewers
// declaration. Informational only, never blocking — existing deployments
// keep running unchanged (FR-5), since an omitted declaration is a
// behaviorally identical no-op. Deliberately distinct from WarnStageDrift/
// FilterNoOpKeys, which stays correctly silent here by design: drift answers
// "is this config outdated?" (no — omission IS the documented default),
// while this notice answers "is this config under-specified?" (yes — the
// operator hasn't told Fabrik which unrequested reviewers, if any, to
// expect). Uses the same warnings.Record/warnings.Clear self-limiting idiom
// as WarnStageDrift, keyed per stage name, so the notice disappears the
// moment a declaration — including an explicit expected_reviewers: [] — is
// added, without requiring any permanent state of its own.
func WarnUndeclaredReviewers(userStages []*Stage, w io.Writer) {
	for _, s := range userStages {
		key := "undeclared_reviewers:" + s.Name
		if s.WaitForReviews == nil || !*s.WaitForReviews || s.ExpectedReviewers != nil {
			_ = warnings.Clear(key)
			continue
		}

		fmt.Fprintf(w, "[startup] notice: stage %q has wait_for_reviews: true but no expected_reviewers declaration — "+
			"self-submitting review bots (e.g. Pruefer, Gemini, CodeRabbit) never appear as a requested reviewer, so "+
			"the bot re-prompt ladder cannot engage for them until declared. See docs/USER_GUIDE.md.\n", s.Name)
		_ = warnings.Record(warnings.Entry{
			Key:   key,
			Type:  "undeclared_reviewers",
			Title: fmt.Sprintf("%s has wait_for_reviews but no expected_reviewers declaration", s.Name),
			Detail: fmt.Sprintf(
				"Stage %q sets wait_for_reviews: true but does not declare expected_reviewers. Self-submitting "+
					"review bots (Pruefer, Gemini, CodeRabbit, ...) never appear in GitHub's requested-reviewer "+
					"list, so the bot re-prompt ladder cannot engage until you declare which reviewer(s) are "+
					"expected — or declare expected_reviewers: [] if none are.\n\n"+
					"Fix: add expected_reviewers to %s (see docs/USER_GUIDE.md).",
				s.Name, s.FilePath,
			),
		})
	}
}

// SweepStaleWarnings clears stage_drift and undeclared_reviewers warnings
// for stage names no longer present in userStages (#1348). Both
// WarnStageDrift and WarnUndeclaredReviewers only reach their own
// warnings.Clear branch for a name they actually visit this pass — a
// renamed or deleted stage YAML is therefore never visited again, and its
// warning is otherwise immortal, the same durable-state-leak shape as
// allow_auto_merge (see sweepStaleAllowAutoMergeWarnings in engine/).
//
// userStages must be the same unfiltered set WarnStageDrift/
// WarnUndeclaredReviewers themselves consume (including Unmanaged/cleanup
// stages) — comparing against a filtered subset would wrongly sweep a
// still-valid stage's warning.
//
// Startup-only, not per-poll: the known-good set (stage names) is fixed for
// the life of the process and only changes across a restart, unlike
// allow_auto_merge's per-poll-refreshed board repo set.
//
// Guards against an empty/nil userStages the same way WarnStageDrift does:
// cmd/root.go already fails startup before Run() is ever reached if stage
// loading produces zero configs, so this should be unreachable in practice —
// but without the guard, an empty present set would make every currently
// recorded stage_drift/undeclared_reviewers entry look "absent" and wipe
// them all in one pass. Matching WarnStageDrift's own no-op keeps the two
// functions consistent and removes that failure mode even if the invariant
// is ever violated (e.g. a future caller that doesn't go through
// cmd/root.go's validation).
func SweepStaleWarnings(userStages []*Stage, w io.Writer) {
	if len(userStages) == 0 {
		return
	}
	present := make(map[string]bool, len(userStages))
	for _, s := range userStages {
		present[s.Name] = true
	}
	for _, warningType := range []string{"stage_drift", "undeclared_reviewers"} {
		cleared, err := warnings.ClearMissing(warningType, present)
		if err != nil {
			fmt.Fprintf(w, "[startup] warning: stale %s warning sweep failed: %v\n", warningType, err)
			continue
		}
		for _, key := range cleared {
			fmt.Fprintf(w, "[startup] cleared stale warning %s: stage no longer configured\n", key)
		}
	}
}

// driftTitle builds the one-line warnings-panel title for a stage-drift entry.
// It names the missing keys (not just their count) so the summary is actionable
// without drilling into the detail view.
func driftTitle(filePath string, missing []string) string {
	return fmt.Sprintf("%s missing %d field(s): %s", filePath, len(missing), strings.Join(missing, ", "))
}

// MissingTopLevelKeys reads the YAML file at userPath and returns a sorted slice
// of keys from defaultKeys that are absent from the user file's top-level map.
func MissingTopLevelKeys(userPath string, defaultKeys map[string]bool) ([]string, error) {
	data, err := os.ReadFile(userPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", userPath, err)
	}

	var userMap map[string]any
	if err := yaml.Unmarshal(data, &userMap); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", userPath, err)
	}

	var missing []string
	for k := range defaultKeys {
		if _, ok := userMap[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing, nil
}
