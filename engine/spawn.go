package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

const (
	spawnBeginMarker = "FABRIK_SPAWN_CHILD_BEGIN"
	spawnEndMarker   = "FABRIK_SPAWN_CHILD_END"
)

// spawnRepoRE matches a well-formed "owner/repo" spawn target.
//
// Deliberately stricter than the "contains a slash" check it replaces (#1263):
// a Plan that mentions the marker inside markdown prose produces a token like
// "handarbeit/fabrik-test-beta`" — trailing backtick included — which contains
// a slash and so used to be accepted as a real spawn target.
var spawnRepoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// spawnDependsOnRE matches a canonical positive decimal integer: no leading
// zero, no leading '+'/'-'. strconv.Atoi alone would accept "01" and "+1" as
// valid, silently admitting non-canonical forms into a header whose grammar
// is documented (ADR-1337) as strict about what counts as a valid index.
var spawnDependsOnRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// errPreImplementDeferred signals that preImplement could not conclusively
// resolve the stage:Plan:complete-but-no-Plan-comment inconsistency (#982) on
// this pass — e.g. the live re-read itself failed, or a recovery cooldown is
// active. The parent must not be paused for this outcome: since it is returned
// before StageAttempted/LastAttemptAt is recorded, the item is retried on the
// next poll cycle with no extra plumbing at the dispatch call site.
var errPreImplementDeferred = errors.New("preImplement: spawn recovery deferred to next poll")

// SpawnBlock represents one child issue declared in a Plan's FABRIK_SPAWN_CHILD_BEGIN/END block.
type SpawnBlock struct {
	Repo  string // "owner/repo"
	Title string
	Body  string

	// DependsOnDeclared reports whether the block carried an optional
	// DEPENDS_ON: header, regardless of whether its value parsed as valid.
	// DependsOnRaw preserves the raw header value (for error messages).
	// DependsOn is the parsed 1-based index into this Plan output's own block
	// list; it stays at the sentinel 0 — never a legal 1-based index — when
	// the header was absent or its value could not be parsed as a positive
	// integer, so validateSpawnDependsOn can treat "absent", "malformed", and
	// "zero/negative" uniformly as "invalid" through a single comparison.
	DependsOnDeclared bool
	DependsOnRaw      string
	DependsOn         int
}

// spawnBeginRepo returns the target repo declared by line when line is a
// genuine FABRIK_SPAWN_CHILD_BEGIN marker line, or "" when it is not one.
//
// A marker line must consist of nothing but the marker and a well-formed
// owner/repo. That strictness is the fix for #1263: before it, the marker was
// located with a bare strings.Index anywhere in the body, so a Plan that
// mentioned the marker in prose — e.g. a markdown checklist item
//
//   - [ ] Emit `FABRIK_SPAWN_CHILD_BEGIN owner/repo` block scoping the child work
//
// — was treated as a real block opener. The parser then ran forward to the
// *real* END marker, swallowed the authentic block as that phantom block's
// content, failed the TITLE: check, and discarded it. The genuine spawn was
// destroyed by the sentence describing it, and preImplement reported "nothing
// to spawn" with no error.
//
// Leading whitespace is tolerated so a block nested in a list still parses;
// trailing content after the repo is not, since that is the shape prose takes.
func spawnBeginRepo(line string) string {
	trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
	if !strings.HasPrefix(trimmed, spawnBeginMarker) {
		return ""
	}
	rest := strings.TrimPrefix(trimmed, spawnBeginMarker)

	// The marker must be a whole word. Without this, a repo glued directly to
	// it — "FABRIK_SPAWN_CHILD_BEGINowner/repo" — satisfies HasPrefix, survives
	// TrimPrefix as a lone repo-shaped field, and is accepted as a genuine
	// marker line: the same boundary confusion this function exists to reject.
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return ""
	}

	fields := strings.Fields(rest)
	if len(fields) != 1 || !spawnRepoRE.MatchString(fields[0]) {
		return ""
	}
	return fields[0]
}

// isSpawnEndLine reports whether line is a standalone END marker, applying the
// same own-line rule as spawnBeginRepo so a prose mention of the END marker
// cannot truncate a real block early.
func isSpawnEndLine(line string) bool {
	return strings.TrimSpace(strings.TrimRight(line, "\r")) == spawnEndMarker
}

// logUnparsedSpawnMarkers reports the case where a Plan comment contains the
// BEGIN marker text but no well-formed block parsed out of it. Both that case
// and "the Plan declared no children" return (false, nil) from preImplement,
// and before #1263 they were indistinguishable in the logs too — so a spawn
// that silently failed to happen looked exactly like a spawn that was never
// requested. Called from both the direct and the recovery path.
func (e *Engine) logUnparsedSpawnMarkers(number int, planBody string) {
	if !strings.Contains(planBody, spawnBeginMarker) {
		return
	}
	e.logf(number, "spawn", "pre-Implement: Plan comment mentions %s but no well-formed block parsed — "+
		"each marker must stand on its own line, as %q ... %q; not spawning children\n",
		spawnBeginMarker, spawnBeginMarker+" owner/repo", spawnEndMarker)
}

// ParseSpawnBlocks scans body for all FABRIK_SPAWN_CHILD_BEGIN/END pairs and
// returns the parsed spawn blocks in order. Malformed or incomplete pairs are
// skipped.
//
// Both markers must stand on their own line — the same convention
// FABRIK_STAGE_COMPLETE follows (stageCompleteRE in engine/claude.go) and that
// CLAUDE.md states for markers generally. The BEGIN line carries the target
// repo: "FABRIK_SPAWN_CHILD_BEGIN owner/repo". The first non-empty line inside
// the block is the TITLE: line; the body is everything after it.
func ParseSpawnBlocks(body string) []SpawnBlock {
	var blocks []SpawnBlock
	lines := strings.Split(body, "\n")

	for i := 0; i < len(lines); i++ {
		repo := spawnBeginRepo(lines[i])
		if repo == "" {
			continue
		}

		end := -1
		for j := i + 1; j < len(lines); j++ {
			if isSpawnEndLine(lines[j]) {
				end = j
				break
			}
		}
		if end == -1 {
			// No matching END — nothing further can be well-formed.
			break
		}

		if title, dependsOnDeclared, dependsOnRaw, dependsOn, blockBody := parseTitleAndBody(strings.Join(lines[i+1:end], "\n")); title != "" {
			blocks = append(blocks, SpawnBlock{
				Repo:              repo,
				Title:             title,
				Body:              blockBody,
				DependsOnDeclared: dependsOnDeclared,
				DependsOnRaw:      dependsOnRaw,
				DependsOn:         dependsOn,
			})
		}

		// Resume after this block's END whether or not it was well-formed, so a
		// malformed block cannot re-open scanning inside its own content.
		i = end
	}
	return blocks
}

// stripSpawnBlocks removes every well-formed FABRIK_SPAWN_CHILD_BEGIN/END
// block from body, using the identical line-scanning algorithm
// ParseSpawnBlocks uses (spawnBeginRepo/isSpawnEndLine/parseTitleAndBody) —
// so it strips exactly the set of blocks ParseSpawnBlocks would parse and
// spawnChildren would act on, no more and no less. A malformed block (one
// ParseSpawnBlocks itself skips, e.g. a missing TITLE: line) is left
// visible rather than silently discarded, matching ParseSpawnBlocks's own
// judgment that it was never really a block.
//
// Used by the Review/Validate mid-flight spawn hook (finalizeStageOutcome,
// engine/item.go) so a successfully-processed declaration does not also leak
// its raw BEGIN/END/TITLE: syntax into the posted PR/issue comment alongside
// the receipt note — mirroring the stripMarkers call already made for
// FABRIK_PR_CREATE/FABRIK_ISSUE_UPDATE in the same function. Not used by
// Plan's own comment (preImplement reads a stored comment back later and
// formatSpawnReceiptNote's "declared above" wording depends on the raw
// blocks staying visible there — see that function's doc comment).
func stripSpawnBlocks(body string) string {
	lines := strings.Split(body, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		repo := spawnBeginRepo(lines[i])
		if repo == "" {
			out = append(out, lines[i])
			continue
		}

		end := -1
		for j := i + 1; j < len(lines); j++ {
			if isSpawnEndLine(lines[j]) {
				end = j
				break
			}
		}
		if end == -1 {
			// No matching END — nothing further can be well-formed (mirrors
			// ParseSpawnBlocks's own bail-out). Keep the remainder verbatim.
			out = append(out, lines[i:]...)
			break
		}

		if title, _, _, _, _ := parseTitleAndBody(strings.Join(lines[i+1:end], "\n")); title == "" {
			// Malformed block body — ParseSpawnBlocks would have skipped it
			// too, so nothing was spawned for it. Leave it visible.
			out = append(out, lines[i:end+1]...)
		}
		// Well-formed block: omit its lines (BEGIN through END) entirely.
		i = end
	}
	return strings.Join(out, "\n")
}

// parseTitleAndBody extracts the title (from the "TITLE: ..." line), the
// optional DEPENDS_ON: header, and the remaining body content from the
// inside of a FABRIK_SPAWN_CHILD_BEGIN/END block.
//
// DEPENDS_ON:, when present, must appear on the line immediately following
// TITLE: — no blank line between them, matching the issue's own example. A
// DEPENDS_ON:-looking line separated from TITLE: by a blank line is just
// body content, avoiding any ambiguity about how far to scan for the
// optional header.
func parseTitleAndBody(content string) (title string, dependsOnDeclared bool, dependsOnRaw string, dependsOn int, body string) {
	lines := strings.Split(content, "\n")
	titleIdx := -1
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "TITLE:") {
			title = strings.TrimSpace(strings.TrimPrefix(trimmed, "TITLE:"))
			titleIdx = i
			break
		}
		// First non-empty line that isn't a TITLE: prefix — malformed.
		return "", false, "", 0, ""
	}
	if title == "" || titleIdx == -1 {
		return "", false, "", 0, ""
	}

	bodyStart := titleIdx + 1
	if bodyStart < len(lines) {
		next := strings.TrimSpace(strings.TrimRight(lines[bodyStart], "\r"))
		if strings.HasPrefix(next, "DEPENDS_ON:") {
			dependsOnDeclared = true
			dependsOnRaw = strings.TrimSpace(strings.TrimPrefix(next, "DEPENDS_ON:"))
			if spawnDependsOnRE.MatchString(dependsOnRaw) {
				if n, err := strconv.Atoi(dependsOnRaw); err == nil {
					dependsOn = n
				}
			}
			bodyStart++
		}
	}

	// Body is everything after the TITLE: (and optional DEPENDS_ON:) line, trimmed.
	bodyLines := lines[bodyStart:]
	body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
	return title, dependsOnDeclared, dependsOnRaw, dependsOn, body
}

// validateSpawnDependsOn performs the purely structural DEPENDS_ON validation
// that must run before any GitHub mutation (requirement 5): each declared
// DEPENDS_ON must be a forward reference to a strictly earlier block in the
// same Plan output — 1 <= DependsOn < the block's own 1-based index. A single
// comparison covers out-of-range, non-forward (self/higher index), and any
// syntactically malformed value (non-numeric, empty, zero, negative), since
// ParseSpawnBlocks leaves DependsOn at the sentinel 0 for anything it could
// not parse as a positive integer. No graph walk is needed or performed:
// forward-only references make sibling dependency cycles structurally
// impossible.
func validateSpawnDependsOn(blocks []SpawnBlock) error {
	for i, b := range blocks {
		if !b.DependsOnDeclared {
			continue
		}
		ownIndex := i + 1
		if b.DependsOn < 1 || b.DependsOn >= ownIndex {
			if ownIndex == 1 {
				return fmt.Errorf("spawn block #1 declares DEPENDS_ON: %q, but block 1 has no earlier sibling to depend on", b.DependsOnRaw)
			}
			return fmt.Errorf("spawn block #%d declares DEPENDS_ON: %q, which must be a forward reference to an earlier block (valid range: 1-%d)", ownIndex, b.DependsOnRaw, ownIndex-1)
		}
	}
	return nil
}

// childFooter returns the engine-appended back-reference footer for a spawned
// child issue per FR-011.
func childFooter(parentOwner, parentRepo string, parentNumber int) string {
	return fmt.Sprintf("\n---\n\n*Spawned by Fabrik from parent issue %s/%s#%d as a multi-issue decomposition. The parent's plan is at the link above.*",
		parentOwner, parentRepo, parentNumber)
}

// spawnChildLabelPrefix is the durable per-child resume marker spawnChildren
// writes on the PARENT issue immediately after each child issue is created
// (before any of the per-child steps that follow CreateIssue), encoding
// blockIndex:childNumber. It is the only restart-surviving record of "this
// block's child already exists" — itemstate.Store is in-memory only — so a
// retried spawn (whether from an operator removing fabrik:paused, or an
// engine restart) can recognize and reuse an already-created child instead
// of creating a duplicate with the same title. See ADR-1583.
const spawnChildLabelPrefix = "fabrik:spawned-child:"

// spawnChildLabelRE matches a well-formed spawnChildLabelPrefix marker,
// capturing the 1-based block index and the created child's issue number.
var spawnChildLabelRE = regexp.MustCompile(`^` + regexp.QuoteMeta(spawnChildLabelPrefix) + `(\d+):(\d+)$`)

// spawnChildLabel formats the durable resume marker recording that
// blockIndex's child issue was created as childNumber. blockIndex is
// 1-based, matching the existing DEPENDS_ON convention — the owner/repo is
// deliberately not encoded: it is always re-derivable from
// blocks[blockIndex-1].Repo, which is itself deterministically reparsed from
// the same immutable Plan comment (or mid-flight output) on every retry,
// keeping the label well clear of GitHub's 50-char label-name limit
// regardless of repo name length (see TestSpawnChildLabelLength).
func spawnChildLabel(blockIndex, childNumber int) string {
	return fmt.Sprintf("%s%d:%d", spawnChildLabelPrefix, blockIndex, childNumber)
}

// parseSpawnChildLabels extracts every spawnChildLabelPrefix marker present
// in labels into a 1-based blockIndex -> childNumber map — recovering
// exactly which children (if any) a previous, interrupted spawnChildren
// attempt already created for this parent.
func parseSpawnChildLabels(labels []string) map[int]int {
	out := make(map[int]int)
	for _, l := range labels {
		m := spawnChildLabelRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		num, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		out[idx] = num
	}
	return out
}

// blockedByContainsChild reports whether deps (typically the parent item's
// own BlockedBy) already contains an edge to childOwner/childRepo#childNumber.
// It resolves gh.Dependency's "Repo == \"\" means same repo as the issue the
// list belongs to" convention against parentOwner/parentRepo, since deps here
// always belongs to the parent issue.
func blockedByContainsChild(deps []gh.Dependency, parentOwner, parentRepo, childOwner, childRepo string, childNumber int) bool {
	for _, d := range deps {
		if d.Number != childNumber {
			continue
		}
		depOwner, depRepo := parentOwner, parentRepo
		if d.Repo != "" {
			o, r, ok := parseOwnerRepoStr(d.Repo)
			if !ok {
				continue
			}
			depOwner, depRepo = o, r
		}
		if depOwner == childOwner && depRepo == childRepo {
			return true
		}
	}
	return false
}

// refreshForSpawnResume re-reads item live via FetchItemDetails so a resumed
// spawnChildren call (alreadyCreated non-empty) sees item.BlockedBy fresh
// rather than trusting a possibly-stale snapshot — BlockedBy is a "deep
// field" not populated by the bulk board fetch, mirroring
// recoverMissingPlanComment's identical reasoning for the Plan comment. Only
// called when resuming; a fresh (never-before-attempted) spawn never pays
// this cost.
//
// A per-item cooldown throttles repeated live-read attempts during a
// sustained failure window, mirroring recoverMissingPlanComment's own
// cooldown. On an active cooldown or a live-read failure, returns
// errPreImplementDeferred without pausing the parent — a transient read
// failure defers to the next poll exactly like recoverMissingPlanComment
// already does, rather than escalating to a hard pause.
func (e *Engine) refreshForSpawnResume(item *gh.ProjectItem) error {
	const cooldownReason = "spawn-resume-deferred"

	if snap, err := e.store.Get(item.Repo, item.Number); err == nil {
		if cooldown := snap.CooldownAt(cooldownReason); !cooldown.IsZero() && e.now().Before(cooldown) {
			e.logf(item.Number, "spawn", "resume: spawn-resume cooldown active — deferring without a live re-read\n")
			return errPreImplementDeferred
		}
	}

	if err := e.client.FetchItemDetails(item); err != nil {
		e.logf(item.Number, "spawn", "resume: live re-read failed (%v) — deferring to next poll\n", err)
		e.store.Apply(itemstate.CooldownRecorded{
			Repo:   item.Repo,
			Number: item.Number,
			Reason: cooldownReason,
			Until:  e.now().Add(time.Duration(e.cfg.PollSeconds*10) * time.Second),
		})
		return errPreImplementDeferred
	}
	return nil
}

// spawnRetryInstruction is the shared recovery instruction appended to every
// per-child spawn-failure pause message (CreateIssue, AddProjectV2ItemById,
// AddBlockedByIssue, and the sibling DEPENDS_ON wiring pass). Replaces the
// pre-ADR-1583 "manually close any orphaned children" instruction, which is
// no longer accurate: already-created children are now durably tracked
// (spawnChildLabelPrefix) and reused automatically on retry, not orphaned.
const spawnRetryInstruction = "Remove `fabrik:paused`, then re-advance to retry — Fabrik tracks already-created children and will resume from where the previous attempt stopped, rather than re-creating them."

// resolveSpecifyOptionID returns the project Status option ID for the "Specify"
// column, or the first non-unmanaged, non-terminal column as a fallback. Returns
// "" when no suitable option exists or sf is nil (caller skips the status-set).
// stagesCfg is consulted to skip any column backed by an `unmanaged: true`
// stage (e.g. a declared Backlog), generalizing beyond the literal name
// "Backlog" — the literal check is kept alongside as the same compat net used
// in checkStageColumnAlignment, for installs with no matching stage declared.
func resolveSpecifyOptionID(sf *gh.StatusField, stagesCfg []*stages.Stage) string {
	if sf == nil {
		return ""
	}
	// Exact match on "Specify".
	if id, ok := sf.Options["Specify"]; ok {
		return id
	}
	// Fallback: first option that is not "Backlog", not an unmanaged column,
	// and not the last column.
	names := sf.OrderedOptionNames
	if len(names) < 2 {
		return ""
	}
	last := names[len(names)-1]
	for _, name := range names {
		if name == "Backlog" || name == last {
			continue
		}
		if st := stages.FindStage(stagesCfg, name); st != nil && st.Unmanaged {
			continue
		}
		return sf.Options[name]
	}
	return ""
}

// preImplement runs the pre-Implement step for stage "Implement". It parses
// the parent's Plan comment for FABRIK_SPAWN_CHILD_BEGIN/END blocks and, when
// found, creates the child issues on GitHub, adds them to the project board,
// links them as blockedBy dependencies of the parent, and marks the parent
// with fabrik:children-spawned.
//
// Returns (true, nil) when children were spawned — the Implement Claude
// invocation must be skipped in this case; checkDependencies will block the
// parent on its next evaluation cycle.
// Returns (false, nil) when there is nothing to do — no spawn blocks were
// found, either directly or (see below) after recovery confirmed there is
// genuinely nothing to spawn.
// Returns (false, err) on any fatal error; the parent is paused before returning.
// Returns (false, errPreImplementDeferred) when stage:Plan:complete is present
// but no Plan comment was found in item.Comments (#982's stale-snapshot
// inconsistency) and recovery could not conclusively resolve it — the parent
// is NOT paused; it is retried on a subsequent poll cycle.
func (e *Engine) preImplement(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem) (bool, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Idempotency guard: if children have already been spawned, skip. This must
	// remain the first check — recoverMissingPlanComment's live-read path relies
	// on this ordering to avoid double-spawning across retried recovery attempts.
	if hasLabel(item.Labels, "fabrik:children-spawned") {
		return false, nil
	}

	// Find the most recent Plan stage comment.
	planComment := findStageComment(item.Comments, "Plan")
	if planComment == nil {
		if hasLabel(item.Labels, "stage:Plan:complete") {
			// Inconsistent state: Plan finished (and may have declared spawn
			// blocks) but the comment the spawn logic reads from is missing
			// from this item snapshot — a stale deep-field read (#982).
			return e.recoverMissingPlanComment(ctx, board, item, owner, repo)
		}
		return false, nil
	}

	blocks := ParseSpawnBlocks(planComment.Body)
	if len(blocks) == 0 {
		e.logUnparsedSpawnMarkers(item.Number, planComment.Body)
		return false, nil
	}

	_, ok, err := e.spawnChildren(ctx, board, item, owner, repo, blocks)
	return ok, err
}

// recoverMissingPlanComment handles the inconsistency where stage:Plan:complete
// is present but findStageComment found no Plan comment in item.Comments — a
// stale-snapshot symptom (#982), since Comments is a deep field whose freshness
// is updatedAt-keyed (#957) and can lag behind label state. It recovers the
// true spawn intent via a live, uncached re-read (the same e.client.FetchItemDetails
// primitive verifyAndHealLinkage uses in engine/prcreate.go for a different deep
// field) rather than trusting the possibly-stale snapshot.
//
// A per-item cooldown throttles repeated live-read attempts during a sustained
// failure window (e.g. #971-style rate-limit pressure), mirroring the
// "dep-blocked" cooldown pattern in engine/item.go.
func (e *Engine) recoverMissingPlanComment(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, owner, repo string) (bool, error) {
	const cooldownReason = "spawn-recovery-deferred"

	// Mandatory log line: fires unconditionally on every hit of this
	// inconsistency, regardless of which outcome below is ultimately taken.
	e.logf(item.Number, "spawn", "pre-Implement: inconsistent state — stage:Plan:complete present but no Plan comment in item snapshot; attempting recovery\n")

	if snap, err := e.store.Get(item.Repo, item.Number); err == nil {
		if cooldown := snap.CooldownAt(cooldownReason); !cooldown.IsZero() && e.now().Before(cooldown) {
			e.logf(item.Number, "spawn", "pre-Implement: spawn-recovery cooldown active — deferring without a live re-read\n")
			return false, errPreImplementDeferred
		}
	}

	fresh := item
	if err := e.client.FetchItemDetails(&fresh); err != nil {
		e.logf(item.Number, "spawn", "pre-Implement: live re-read failed (%v) — deferring to next poll\n", err)
		e.store.Apply(itemstate.CooldownRecorded{
			Repo:   item.Repo,
			Number: item.Number,
			Reason: cooldownReason,
			Until:  e.now().Add(time.Duration(e.cfg.PollSeconds*10) * time.Second),
		})
		return false, errPreImplementDeferred
	}

	planComment := findStageComment(fresh.Comments, "Plan")
	if planComment == nil {
		e.logf(item.Number, "spawn", "pre-Implement: live re-read confirms no Plan comment exists — nothing to spawn\n")
		return false, nil
	}

	blocks := ParseSpawnBlocks(planComment.Body)
	if len(blocks) == 0 {
		e.logf(item.Number, "spawn", "pre-Implement: live re-read recovered Plan comment with no spawn blocks — nothing to spawn\n")
		e.logUnparsedSpawnMarkers(item.Number, planComment.Body)
		return false, nil
	}

	e.logf(item.Number, "spawn", "pre-Implement: live re-read recovered %d child(ren) missed by stale snapshot — proceeding to spawn\n", len(blocks))
	_, ok, err := e.spawnChildren(ctx, board, fresh, owner, repo, blocks)
	return ok, err
}

// spawnTargetServedByThisInstance reports whether this Fabrik instance's own
// project board is a legitimate registration target for a spawned child in
// childOwner/childRepo.
//
// A multi-repo instance (cfg.Repo == "") already legitimately serves any repo
// the org grants board access to — this is the existing, working shape of
// same-process cross-repo spawning (the issue's own "Instance A processes
// multiple repos on board 5 without issue"). A repo:-scoped instance serves
// only its own declared repo; per the issue's own out-of-scope boundary (no
// cross-instance board discovery), it has no basis to claim any other repo is
// servable here, so it must refuse rather than silently registering the child
// on its own board anyway (failure mode 1 — see ADR-1419). This is a
// scope-check, not a route-finder: it does not locate the correct board for a
// mismatched repo, it only prevents guessing the wrong one.
func (e *Engine) spawnTargetServedByThisInstance(childOwner, childRepo string) bool {
	if e.cfg.Repo == "" {
		return true
	}
	return childOwner == e.cfg.Owner && childRepo == e.cfg.Repo
}

// spawnChildren creates the child issues described by blocks, adds them to the
// project board, assigns them to cfg.User, links them as blockedBy
// dependencies of the parent, and marks the parent with fabrik:children-spawned.
// Shared by all spawn origins — preImplement's direct path, recoverMissingPlanComment's
// recovery path, and finalizeStageOutcome's Review/Validate mid-flight hook —
// so every origin gets identical wiring through a single code path (ADR-1419).
//
// Returns (spawned, true, nil) when children were spawned, where spawned lists
// each child as "owner/repo#N". For the Implement dispatch caller specifically,
// this also means the Implement Claude invocation must be skipped in this
// case — checkDependencies will block the parent on its next evaluation cycle.
// Returns (partial, false, err) on any fatal error; the parent is paused
// before returning. partial lists whatever children were created before the
// failure, for error-message purposes.
func (e *Engine) spawnChildren(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, owner, repo string, blocks []SpawnBlock) ([]string, bool, error) {
	e.logf(item.Number, "spawn", "found %d child(ren) to spawn\n", len(blocks))

	// Validate DEPENDS_ON headers upfront, before any GitHub mutation. This is
	// purely structural (no created-issue data needed) so an invalid index
	// fails loud and cheap, with zero orphaned issues — "Created so far: none"
	// is always accurate for this failure class.
	if err := validateSpawnDependsOn(blocks); err != nil {
		msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\n%s. Created so far: %s\n\nRemove `fabrik:paused` after fixing the output to retry.",
			err, formatSpawnedList(nil))
		e.pauseIssue(item, msg, pauseOpts{
			labelEcho: true,
		})
		return nil, false, fmt.Errorf("spawn: %w", err)
	}

	// Ensure every target repo is one this instance is actually configured to
	// serve, before any GitHub mutation — the board-servability guard
	// (requirement 3). A repo:-scoped instance that doesn't cover a target
	// repo fails loud here rather than silently registering the child onto
	// its own (wrong) board.
	uniqueRepos := make(map[string]struct{})
	for _, b := range blocks {
		uniqueRepos[b.Repo] = struct{}{}
	}
	for targetRepo := range uniqueRepos {
		targetOwner, targetRepoName, ok := parseOwnerRepoStr(targetRepo)
		if !ok {
			// Malformed repo string — the per-block loop below will catch and report it.
			continue
		}
		if !e.spawnTargetServedByThisInstance(targetOwner, targetRepoName) {
			msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\nSpawn target `%s` is not served by this Fabrik instance (configured for `%s/%s` only, and this instance's `repo:` scoping means it does not process other repos' boards). Created so far: %s\n\nEither reconfigure this instance's `repo:`/`project:` to cover `%s`, or run the spawn from an instance that does. Remove `fabrik:paused` after fixing, then re-advance to retry.",
				targetRepo, e.cfg.Owner, e.cfg.Repo, formatSpawnedList(nil), targetRepo)
			e.pauseIssue(item, msg, pauseOpts{
				labelEcho: true,
			})
			return nil, false, fmt.Errorf("spawn: target %s not served by this instance (configured for %s/%s)", targetRepo, e.cfg.Owner, e.cfg.Repo)
		}
	}

	// Ensure all target repos are initialized (bare-cloned) before any mutation.
	// On-demand clone via singleflight — no prior processing of an issue from the
	// target repo is required. Error comment and labels are posted by
	// ensureSpawnTargetReady on failure.
	for targetRepo := range uniqueRepos {
		targetOwner, targetRepoName, ok := parseOwnerRepoStr(targetRepo)
		if !ok {
			// Malformed repo string — the per-block loop below will catch and report it.
			continue
		}
		if err := e.ensureSpawnTargetReady(ctx, targetOwner, targetRepoName, item); err != nil {
			return nil, false, fmt.Errorf("spawn: initializing spawn target %s: %w", targetRepo, err)
		}
	}

	// Snapshot statusField once before the loop (stable once set; avoids holding the mutex during network calls).
	e.mu.Lock()
	sf := e.statusField
	e.mu.Unlock()

	// Resume support (ADR-1583): recover which blocks (if any) already had
	// their child created by a previous, interrupted attempt — the only
	// durable, restart-surviving record of that fact. When non-empty, refresh
	// item live first: BlockedBy is a "deep field" the bulk board fetch never
	// populates, and the per-child loop below needs it fresh to tell whether
	// an already-created child was also already linked. A live-read failure
	// or active cooldown defers to the next poll without pausing the parent.
	alreadyCreated := parseSpawnChildLabels(item.Labels)
	if len(alreadyCreated) > 0 {
		if err := e.refreshForSpawnResume(&item); err != nil {
			return nil, false, err
		}
		alreadyCreated = parseSpawnChildLabels(item.Labels)
	}

	// Spawn children in order, retaining the block-index -> child node-ID
	// mapping so the sibling-wiring pass below can resolve DEPENDS_ON
	// references after all children exist.
	var spawned []string
	childNodeIDs := make([]string, len(blocks))
	childNumbers := make([]int, len(blocks))
	for i, block := range blocks {
		blockIndex := i + 1
		childOwner, childRepo, ok := parseOwnerRepoStr(block.Repo)
		if !ok {
			msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\nInvalid repo in spawn block #%d: `%s`. Created so far: %s\n\nRemove `fabrik:paused` after fixing the output to retry.",
				blockIndex, block.Repo, formatSpawnedList(spawned))
			e.pauseIssue(item, msg, pauseOpts{
				labelEcho: true,
			})
			return spawned, false, fmt.Errorf("spawn: invalid repo %q in block %d", block.Repo, blockIndex)
		}

		resuming := false
		var childNumber int
		var childNodeID string
		if n, ok := alreadyCreated[blockIndex]; ok {
			// This block's child was already created by a previous attempt —
			// resolve its node ID instead of creating a duplicate.
			resuming = true
			childNumber = n
			pi, err := e.client.FetchProjectItem(childOwner, childRepo, childNumber)
			if err != nil || pi == nil || pi.ID == "" {
				msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\nPreviously created child `%s#%d` (spawn block #%d) could not be found: `%v`. It may have been deleted.\n\nCreated so far: %s\n\nIf the child was deleted, remove the `%s` label to force re-creation of this one block, then remove `fabrik:paused` and re-advance to retry. Otherwise this may be a transient error — try removing `fabrik:paused` again.",
					block.Repo, childNumber, blockIndex, err, formatSpawnedList(spawned), spawnChildLabel(blockIndex, childNumber))
				e.pauseIssue(item, msg, pauseOpts{
					labelEcho: true,
				})
				return spawned, false, fmt.Errorf("spawn: resuming block %d (child %s#%d): %w", blockIndex, block.Repo, childNumber, err)
			}
			childNodeID = pi.ID
			e.logf(item.Number, "spawn", "resuming block %d: child %s/%s#%d already created\n", blockIndex, childOwner, childRepo, childNumber)
		} else {
			// Every spawned child is assigned to cfg.User — the user of the
			// instance meant to process it (requirement 4). Folded into the same
			// CreateIssue POST rather than a separate call, so a bad/misconfigured
			// user still fails loud through this single, already-fail-loud path.
			fullBody := block.Body + childFooter(owner, repo, item.Number)
			n, nodeID, err := e.client.CreateIssue(childOwner, childRepo, block.Title, fullBody, []string{e.cfg.User})
			if err != nil {
				msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\nFailed to create child issue %d/%d in `%s`: `%v`\n\nCreated so far: %s\n\n%s",
					blockIndex, len(blocks), block.Repo, err, formatSpawnedList(spawned), spawnRetryInstruction)
				e.pauseIssue(item, msg, pauseOpts{
					labelEcho: true,
				})
				return spawned, false, fmt.Errorf("spawn: creating child %d: %w", blockIndex, err)
			}
			childNumber, childNodeID = n, nodeID
			e.logf(item.Number, "spawn", "created child %s/%s#%d\n", childOwner, childRepo, childNumber)

			// Durable resume marker: best-effort (addLabelChecked's error is
			// logged, not fatal). A dropped write only regresses this one
			// child to the pre-ADR-1583 behavior if a later step in this same
			// attempt also fails — strictly narrower than the bug being fixed.
			if lerr := e.addLabelChecked(item, spawnChildLabel(blockIndex, childNumber)); lerr != nil {
				e.logf(item.Number, "warn", "could not write spawn resume marker for block %d (child %s#%d): %v — a later failure in this attempt could duplicate this child on retry\n", blockIndex, block.Repo, childNumber, lerr)
			}
		}
		spawned = append(spawned, fmt.Sprintf("%s#%d", block.Repo, childNumber))
		childNodeIDs[i] = childNodeID
		childNumbers[i] = childNumber

		// Add child to the project board. When resuming, check first rather
		// than assume AddProjectV2ItemById is safe to call twice (unconfirmed
		// against real GitHub — see ADR-1583); a fresh child takes the
		// unchanged direct-call path. existingStatus, when known, also gates
		// the placement step below.
		var childItemID, existingStatus string
		addedAlready := false
		if resuming {
			id, status, lerr := e.client.LookupIssueProjectItem(board.ProjectID, block.Repo, childNumber)
			if lerr != nil {
				e.logf(item.Number, "warn", "spawn resume: could not check existing board membership for %s#%d: %v — attempting AddProjectV2ItemById anyway\n", block.Repo, childNumber, lerr)
			} else if id != "" {
				childItemID, existingStatus, addedAlready = id, status, true
			}
		}
		if !addedAlready {
			id, err := e.client.AddProjectV2ItemById(board.ProjectID, childNodeID)
			if err != nil {
				msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\nFailed to add child %s/%s#%d to project board: `%v`\n\nCreated so far: %s\n\n%s",
					childOwner, childRepo, childNumber, err, formatSpawnedList(spawned), spawnRetryInstruction)
				e.pauseIssue(item, msg, pauseOpts{
					labelEcho: true,
				})
				return spawned, false, fmt.Errorf("spawn: adding child %s#%d to project: %w", block.Repo, childNumber, err)
			}
			childItemID = id
		}

		// Link child as a blockedBy dependency of the parent. When resuming,
		// check item's (freshly refreshed) BlockedBy first, for the same
		// unconfirmed-idempotency reason as the board-add above.
		// item.ID is the parent issue's GraphQL node ID.
		alreadyLinked := resuming && blockedByContainsChild(item.BlockedBy, owner, repo, childOwner, childRepo, childNumber)
		if !alreadyLinked {
			if err := e.client.AddBlockedByIssue(item.ID, childNodeID); err != nil {
				msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\nFailed to link child %s/%s#%d as blocked-by of parent: `%v`\n\nCreated so far: %s\n\n%s",
					childOwner, childRepo, childNumber, err, formatSpawnedList(spawned), spawnRetryInstruction)
				e.pauseIssue(item, msg, pauseOpts{
					labelEcho: true,
				})
				return spawned, false, fmt.Errorf("spawn: linking child %s#%d as blocked-by: %w", block.Repo, childNumber, err)
			}
		}

		// Apply fabrik:sub-issue label to child (idempotent add; for human-visible filtering; no engine semantics).
		if err := e.client.AddLabelToIssue(childOwner, childRepo, childNumber, "fabrik:sub-issue"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:sub-issue to %s#%d: %v\n", block.Repo, childNumber, err)
		}

		// Set child's project Status to Specify (or first processing stage) when statusField is available.
		// Any failure here is non-fatal to spawning (the child issue, board item, and
		// blockedBy link already exist) but leaves the child stranded in whatever column
		// GitHub defaulted it to (typically Backlog) — a column with no configured stage,
		// so ordinary dispatch (itemMayNeedWork/itemNeedsWork) would never revisit it.
		// recordChildPlacementFailure writes a durable marker so the settle scan in
		// poll.go retries the placement independent of stage dispatch (see spawn_settle.go).
		//
		// When resuming a child that already carries a non-empty Status,
		// placement is skipped entirely rather than re-run: the child may
		// have already progressed past Specify under its own pipeline while
		// the parent sat paused, and blindly re-setting its column would
		// clobber that real progress (ADR-1583).
		if resuming && existingStatus != "" {
			e.logf(item.Number, "spawn", "resume: %s#%d already has project status %q — leaving placement alone\n", block.Repo, childNumber, existingStatus)
		} else if optionID := resolveSpecifyOptionID(sf, e.cfg.Stages); optionID != "" {
			if err := e.client.UpdateProjectItemStatus(board.ProjectID, childItemID, sf.FieldID, optionID); err != nil {
				e.logf(item.Number, "warn", "could not set project status on %s#%d: %v\n", block.Repo, childNumber, err)
				e.recordChildPlacementFailure(childOwner, childRepo, childNumber)
			}
		} else if sf == nil {
			e.logf(item.Number, "warn", "project status field unavailable for %s#%d; child lands in Backlog\n", block.Repo, childNumber)
			e.recordChildPlacementFailure(childOwner, childRepo, childNumber)
		} else {
			e.logf(item.Number, "warn", "no Specify/processing status option found for %s#%d; child lands in Backlog\n", block.Repo, childNumber)
			e.recordChildPlacementFailure(childOwner, childRepo, childNumber)
		}

		// Inherit fabrik:yolo and fabrik:cruise from parent (idempotent add; enables autonomous child pipeline).
		if hasLabel(item.Labels, "fabrik:yolo") {
			if err := e.client.AddLabelToIssue(childOwner, childRepo, childNumber, "fabrik:yolo"); err != nil {
				e.logf(item.Number, "warn", "could not add fabrik:yolo to %s#%d: %v\n", block.Repo, childNumber, err)
			}
		}
		if hasLabel(item.Labels, "fabrik:cruise") {
			if err := e.client.AddLabelToIssue(childOwner, childRepo, childNumber, "fabrik:cruise"); err != nil {
				e.logf(item.Number, "warn", "could not add fabrik:cruise to %s#%d: %v\n", block.Repo, childNumber, err)
			}
		}
	}

	// Second pass: wire each declared DEPENDS_ON as a sibling blockedBy edge,
	// in addition to the parent edges added above. Runs after all children
	// exist per requirement 2's two-phase design — a block may depend on any
	// earlier sibling regardless of where creation happened to succeed.
	for i, block := range blocks {
		if !block.DependsOnDeclared {
			continue
		}
		blockerIdx := block.DependsOn - 1
		if err := e.client.AddBlockedByIssue(childNodeIDs[i], childNodeIDs[blockerIdx]); err != nil {
			msg := fmt.Sprintf("🏭 **Fabrik — spawn failed**\n\nFailed to link sibling dependency for spawn block #%d (DEPENDS_ON: %d): `%v`\n\nCreated so far: %s\n\n%s",
				i+1, block.DependsOn, err, formatSpawnedList(spawned), spawnRetryInstruction)
			e.pauseIssue(item, msg, pauseOpts{
				labelEcho: true,
			})
			return spawned, false, fmt.Errorf("spawn: linking sibling dependency for block %d: %w", i+1, err)
		}
		e.logf(item.Number, "spawn", "linked sibling dependency: block %d depends on block %d\n", i+1, block.DependsOn)
	}

	// All children spawned and sibling dependencies wired — remove the
	// per-child resume markers (steady state carries none of them; they only
	// exist transiently during an interrupted/in-progress spawn) before
	// marking the parent with the idempotency guard, so a poll racing this
	// exact instant never observes fabrik:children-spawned alongside a
	// stale marker. Both are best-effort: a dropped removal here is
	// harmless clutter, not a correctness issue (parseSpawnChildLabels is
	// only ever consulted before fabrik:children-spawned is set — see
	// preImplement's idempotency-guard-first check).
	for i := range blocks {
		e.removeLabel(item, spawnChildLabel(i+1, childNumbers[i]))
	}

	// Mark parent with idempotency guard. This must come after the
	// sibling-wiring pass so the guard covers the full two-phase operation: a
	// wiring failure retries (now resuming every already-created child rather
	// than duplicating them) on the next attempt.
	// No webhook echo here — preserving prior behavior (never echoed at this site).
	e.applyLabelAdd(item, "fabrik:children-spawned", false)

	e.logf(item.Number, "spawn", "spawned %d child(ren); parent will be gated until all close\n", len(blocks))
	return spawned, true, nil
}

// addPausedLabelToItem adds fabrik:paused to the given item, with cache write-through
// and webhook echo registration.
func (e *Engine) addPausedLabelToItem(owner, repo string, item gh.ProjectItem) {
	e.addLabel(gh.ProjectItem{Number: item.Number, Repo: owner + "/" + repo}, "fabrik:paused")
}

// parseOwnerRepoStr splits "owner/repo" into owner and repo. Returns false if
// the string does not contain exactly one "/" with non-empty parts on each side.
func parseOwnerRepoStr(s string) (owner, repo string, ok bool) {
	idx := strings.Index(s, "/")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// formatSpawnedList formats the list of already-spawned children for error messages.
func formatSpawnedList(spawned []string) string {
	if len(spawned) == 0 {
		return "none"
	}
	return strings.Join(spawned, ", ")
}
