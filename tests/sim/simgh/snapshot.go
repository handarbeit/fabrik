package simgh

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// This file is snapshot and restore: round-tripping the whole model so a
// restart scenario can discard the engine and rebuild it against exactly the
// state GitHub would have retained.
//
// # What "restore" means
//
// The durable-state contract Fabrik rests on is that GitHub-side state
// survives a restart and in-memory state does not — the rocket reaction is
// durable "already processed", labels are state, processedSet is not. Restore
// is what makes that testable: the engine dies and comes back, and the world
// did *not* reset.
//
// That is why the fault schedule and the mutation log round-trip too, not just
// the model. A recovery scenario running against a persistently-failing GitHub
// needs the failure to persist across the restart, and the log is the test's
// audit record — an ordering assertion may legitimately span the restart.
//
// # What does not round-trip, and why
//
// The Clock. It is never model-owned, and serialising it would break that; it
// is re-supplied to Restore as an Option, exactly as to New. Everything else
// is enumerated in snapshotFieldRegistry below, whose completeness is asserted
// by reflection in snapshot_test.go — a field added to model.go and not
// handled here fails that test rather than silently vanishing across a
// restart, where it would masquerade as an engine defect.
//
// # Git state
//
// Each backing bare repository is copied as a directory tree, under the repo's
// gitMu and after a defensive `git worktree prune`. The prune matters: git's
// worktree admin entries live inside bareDir and carry *absolute* paths into
// the throwaway checkouts under the old baseDir, and t.TempDir() guarantees a
// restored Sim's baseDir differs every time. A stale entry copied forward
// would point at a directory that no longer exists.
//
// # Quiescence
//
// Snapshot copies git state and model state in two phases and does not hold
// one lock across both — it cannot, without violating the package's
// mu-before-gitMu invariant. Call it when no engine goroutine is running, the
// way a restart scenario naturally would. A snapshot taken mid-flight is not
// torn in any dangerous sense (each half is internally consistent), but the
// two halves may disagree about a mutation that landed between them.

// Snapshot is a restorable copy of a Sim's entire state. Treat it as opaque;
// it is reusable, so one snapshot can seed several restores.
type Snapshot struct {
	// gitDir is the staging directory holding a copy of every backing bare
	// repository, laid out owner/repo.git exactly as under a live baseDir.
	gitDir string

	repos             map[string]*repoState
	projects          map[string]*projectState
	defaultProjectKey string

	nextCommentDatabaseID int
	nextCheckRunID        int64
	seededCheckRunIDs     map[int64]bool

	restRate    rateBudget
	graphqlRate rateBudget

	seedErr error

	// instrumented is set only by Instrumented.Snapshot. RestoreInstrumented
	// refuses a snapshot taken from a bare Sim rather than silently handing
	// back an empty fault table — a scenario that expected its persistent
	// failure to survive the restart would otherwise see GitHub quietly heal.
	instrumented bool
	faultRules   map[string][]*faultRule
	faultCalls   map[string]int
	logEntries   []Entry
}

// GitDir reports the staging directory holding the snapshot's copied
// repositories, for a test that wants to inspect or assert against them.
func (s *Snapshot) GitDir() string { return s.gitDir }

// Snapshot copies the model and every backing repository into stagingDir,
// which must already exist and be empty of anything the snapshot would
// collide with — tests pass t.TempDir(). Ownership of stagingDir stays with
// the caller, so cleanup is never ambiguous.
func (s *Sim) Snapshot(stagingDir string) (*Snapshot, error) {
	if stagingDir == "" {
		return nil, fmt.Errorf("simgh: Snapshot: stagingDir must be non-empty")
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("simgh: Snapshot: creating staging dir: %w", err)
	}

	// Phase 1: the repositories, each under its own gitMu. mu is taken only to
	// read the repo list, and released before any git runs — the package's
	// invariant forbids holding it across a subprocess call.
	s.mu.Lock()
	repos := make([]*repoState, 0, len(s.repos))
	for _, r := range s.repos {
		repos = append(repos, r)
	}
	s.mu.Unlock()

	for _, r := range repos {
		if err := snapshotRepoGit(r, stagingDir); err != nil {
			return nil, err
		}
	}

	// Phase 2: the in-memory model.
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := &Snapshot{
		gitDir:                stagingDir,
		repos:                 make(map[string]*repoState, len(s.repos)),
		projects:              make(map[string]*projectState, len(s.projects)),
		nextCommentDatabaseID: s.nextCommentDatabaseID,
		nextCheckRunID:        s.nextCheckRunID,
		seededCheckRunIDs:     make(map[int64]bool, len(s.seededCheckRunIDs)),
		restRate:              s.restRate,
		graphqlRate:           s.graphqlRate,
		seedErr:               s.seedErr,
	}
	for k, r := range s.repos {
		snap.repos[k] = cloneRepoState(r)
	}
	for k, p := range s.projects {
		snap.projects[k] = cloneProjectState(p)
	}
	for id := range s.seededCheckRunIDs {
		snap.seededCheckRunIDs[id] = true
	}
	if s.defaultProject != nil {
		snap.defaultProjectKey = projectKey(s.defaultProject.owner, s.defaultProject.num)
	}
	return snap, nil
}

// snapshotRepoGit prunes stale worktree admin entries and copies one bare
// repository into the staging directory.
func snapshotRepoGit(r *repoState, stagingDir string) error {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()

	// Defensive, and deliberately fatal. What the prune buys is that stale
	// worktree admin entries — which hold absolute paths into a baseDir the
	// restore will not have — do not travel forward into the copy. A repo with
	// no worktrees to prune succeeds trivially, so the only way this fails is
	// that git itself could not run, and a snapshot copied without the prune
	// having happened is precisely the one whose restore breaks later, on a
	// git operation, in a way that reads as a model bug. Failing here names
	// the cause; deferring it hides it.
	if _, err := runGit(r.bareDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("simgh: Snapshot: pruning worktrees for %s/%s: %w", r.owner, r.repo, err)
	}

	dst := filepath.Join(stagingDir, r.owner, r.repo+".git")
	if err := copyDir(r.bareDir, dst); err != nil {
		return fmt.Errorf("simgh: Snapshot: copying %s/%s: %w", r.owner, r.repo, err)
	}
	return nil
}

// Restore rebuilds a Sim from a snapshot, with its backing repositories copied
// into a fresh baseDir. Options are applied exactly as by New — in particular
// WithClock, since the clock is never model-owned and is not carried by the
// snapshot.
//
// The snapshot is left intact and may be restored again.
func Restore(snap *Snapshot, baseDir string, opts ...Option) (*Sim, error) {
	if snap == nil {
		return nil, fmt.Errorf("simgh: Restore: nil snapshot")
	}
	s := New(baseDir, opts...)
	if err := s.ensureBaseDir(); err != nil {
		return nil, err
	}

	s.nextCommentDatabaseID = snap.nextCommentDatabaseID
	s.nextCheckRunID = snap.nextCheckRunID
	s.restRate = snap.restRate
	s.graphqlRate = snap.graphqlRate
	s.seedErr = snap.seedErr
	for id := range snap.seededCheckRunIDs {
		s.seededCheckRunIDs[id] = true
	}

	for k, r := range snap.repos {
		restored := cloneRepoState(r)
		// The on-disk paths are the one thing that must not survive verbatim:
		// they are absolute, and the restore target is a different directory.
		restored.bareDir = s.repoDir(restored.owner, restored.repo)
		restored.worktreeRoot = restored.bareDir + ".worktrees"
		s.repos[k] = restored

		src := filepath.Join(snap.gitDir, restored.owner, restored.repo+".git")
		if err := copyDir(src, restored.bareDir); err != nil {
			return nil, fmt.Errorf("simgh: Restore: copying %s: %w", k, err)
		}
	}
	for k, p := range snap.projects {
		s.projects[k] = cloneProjectState(p)
	}
	if snap.defaultProjectKey != "" {
		s.defaultProject = s.projects[snap.defaultProjectKey]
	}
	return s, nil
}

// Snapshot copies the wrapped model plus the fault schedule and the mutation
// log. See the file doc for why the two instruments travel with the model.
func (in *Instrumented) Snapshot(stagingDir string) (*Snapshot, error) {
	snap, err := in.sim.Snapshot(stagingDir)
	if err != nil {
		return nil, err
	}
	snap.instrumented = true
	snap.faultRules, snap.faultCalls = in.faults.snapshotRules()
	snap.logEntries = in.log.Entries()
	return snap, nil
}

// RestoreInstrumented rebuilds an Instrumented client from a snapshot taken by
// Instrumented.Snapshot.
//
// It refuses a snapshot taken from a bare Sim rather than handing back an
// empty fault table: a scenario restarting against a persistently-failing
// GitHub would otherwise see it quietly heal, and would pass by testing the
// recovery it was meant to prove impossible.
func RestoreInstrumented(snap *Snapshot, baseDir string, opts ...Option) (*Instrumented, error) {
	if snap == nil {
		return nil, fmt.Errorf("simgh: RestoreInstrumented: nil snapshot")
	}
	if !snap.instrumented {
		return nil, fmt.Errorf("simgh: RestoreInstrumented: snapshot was taken by Sim.Snapshot, " +
			"so it carries no fault schedule or mutation log; take it with Instrumented.Snapshot")
	}
	sim, err := Restore(snap, baseDir, opts...)
	if err != nil {
		return nil, err
	}
	in := Instrument(sim)
	in.faults.restoreRules(snap.faultRules, snap.faultCalls)
	in.log.restoreEntries(snap.logEntries)
	return in, nil
}

// --- deep copies -----------------------------------------------------------

func cloneRepoState(r *repoState) *repoState {
	out := &repoState{
		owner:             r.owner,
		repo:              r.repo,
		bareDir:           r.bareDir,
		worktreeRoot:      r.worktreeRoot,
		defaultBranch:     r.defaultBranch,
		issues:            make(map[int]*issueRecord, len(r.issues)),
		prs:               make(map[int]*prRecord, len(r.prs)),
		checkRuns:         make(map[string][]gh.CheckRun, len(r.checkRuns)),
		commitStatuses:    make(map[string][]gh.CommitStatus, len(r.commitStatuses)),
		ciSchedule:        r.ciSchedule.clone(),
		requiredContexts:  make(map[string][]string, len(r.requiredContexts)),
		requireUpToDate:   make(map[string]bool, len(r.requireUpToDate)),
		requiredApprovals: make(map[string]int, len(r.requiredApprovals)),
		labelVocab:        make(map[string]bool, len(r.labelVocab)),
		access:            r.access,
		mergeQueueEnabled: r.mergeQueueEnabled,
		nextNumber:        r.nextNumber,
	}
	for k, v := range r.issues {
		out.issues[k] = cloneIssueRecord(v)
	}
	for k, v := range r.prs {
		out.prs[k] = clonePRRecord(v)
	}
	for k, v := range r.checkRuns {
		out.checkRuns[k] = append([]gh.CheckRun(nil), v...)
	}
	for k, v := range r.commitStatuses {
		out.commitStatuses[k] = append([]gh.CommitStatus(nil), v...)
	}
	for k, v := range r.requiredContexts {
		out.requiredContexts[k] = cloneStrings(v)
	}
	for k, v := range r.requireUpToDate {
		out.requireUpToDate[k] = v
	}
	for k, v := range r.requiredApprovals {
		out.requiredApprovals[k] = v
	}
	for k, v := range r.labelVocab {
		out.labelVocab[k] = v
	}
	if r.latestRelease != nil {
		rel := *r.latestRelease
		rel.Assets = append([]gh.ReleaseAsset(nil), r.latestRelease.Assets...)
		out.latestRelease = &rel
	}
	return out
}

func cloneIssueRecord(i *issueRecord) *issueRecord {
	out := &issueRecord{
		number:         i.number,
		title:          i.title,
		body:           i.body,
		state:          i.state,
		author:         i.author,
		labels:         cloneStrings(i.labels),
		assignees:      cloneStrings(i.assignees),
		labelAppliedAt: make(map[string]time.Time, len(i.labelAppliedAt)),
		blockedBy:      append([]gh.Dependency(nil), i.blockedBy...),
		createdAt:      i.createdAt,
		updatedAt:      i.updatedAt,
	}
	for k, v := range i.labelAppliedAt {
		out.labelAppliedAt[k] = v
	}
	out.comments = cloneComments(i.comments)
	return out
}

func clonePRRecord(p *prRecord) *prRecord {
	out := &prRecord{
		number:                  p.number,
		title:                   p.title,
		body:                    p.body,
		head:                    p.head,
		base:                    p.base,
		state:                   p.state,
		draft:                   p.draft,
		merged:                  p.merged,
		author:                  p.author,
		labels:                  cloneStrings(p.labels),
		issueNumber:             p.issueNumber,
		comments:                cloneComments(p.comments),
		reviewThreadComment:     cloneComments(p.reviewThreadComment),
		reviews:                 append([]gh.PRReview(nil), p.reviews...),
		reviewRequests:          append([]gh.ReviewRequest(nil), p.reviewRequests...),
		reviewSchedule:          p.reviewSchedule.clone(),
		resolvedThreads:         p.resolvedThreads,
		autoMergeEnabled:        p.autoMergeEnabled,
		autoMergeStrategy:       p.autoMergeStrategy,
		inMergeQueue:            p.inMergeQueue,
		mergeableRecomputeReads: p.mergeableRecomputeReads,
		createdAt:               p.createdAt,
		updatedAt:               p.updatedAt,
	}
	if p.queueEntry != nil {
		entry := *p.queueEntry
		out.queueEntry = &entry
	}
	return out
}

func cloneComments(in []*commentRecord) []*commentRecord {
	if in == nil {
		return nil
	}
	out := make([]*commentRecord, 0, len(in))
	for _, c := range in {
		dup := *c
		dup.reactions = make(map[string]int, len(c.reactions))
		for k, v := range c.reactions {
			dup.reactions[k] = v
		}
		out = append(out, &dup)
	}
	return out
}

func cloneProjectState(p *projectState) *projectState {
	out := &projectState{
		id:                 p.id,
		owner:              p.owner,
		num:                p.num,
		title:              p.title,
		statusFieldID:      p.statusFieldID,
		statusOptions:      make(map[string]string, len(p.statusOptions)),
		orderedStatusNames: cloneStrings(p.orderedStatusNames),
		items:              make(map[string]*itemState, len(p.items)),
		itemOrder:          cloneStrings(p.itemOrder),
		updatedAt:          p.updatedAt,
	}
	for k, v := range p.statusOptions {
		out.statusOptions[k] = v
	}
	for k, v := range p.items {
		dup := *v
		out.items[k] = &dup
	}
	return out
}

// restoreEntries replaces the log's contents, renumbering nothing: sequence
// numbers survive the restart, so an ordering assertion may span it.
func (l *MutationLog) restoreEntries(entries []Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make([]Entry, len(entries))
	copy(l.entries, entries)
}

// --- directory copy --------------------------------------------------------

// copyDir recursively copies src to dst, creating dst.
//
// Hand-rolled rather than os.CopyFS so it fails loudly on anything that is not
// a regular file or a directory. A bare repository contains neither symlinks
// nor devices, so encountering one means either an assumption has broken or
// something is in the tree that should not be — and silently skipping it would
// corrupt the restore in a way that only surfaces as an inexplicable git
// failure much later.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			// The owner write bit is forced on: a directory copied without it
			// could not receive its own children.
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("simgh: copying %s: mode %v is neither a regular file nor a directory; "+
				"a bare repository should contain only those", path, info.Mode())
		}
	})
}

// copyFile copies one regular file, preserving its permission bits.
func copyFile(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Replace rather than truncate-in-place. Git writes its loose objects and
	// packs read-only (0444), so opening an existing one for writing fails
	// outright — which makes a second snapshot into the same staging
	// directory, or a restore into a directory that already holds a copy, fail
	// on the first object it meets. Removing first is what makes both
	// idempotent.
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("simgh: replacing %s: %w", dst, err)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() {
		// A close error on a write handle can be the only report of a failed
		// flush, so it must not be discarded.
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("simgh: closing %s: %w", dst, cerr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("simgh: copying %s to %s: %w", src, dst, err)
	}
	return nil
}

// --- field registry --------------------------------------------------------
//
// Every field of every model type, with what Snapshot/Restore does about it.
// snapshot_test.go reflects over the types and asserts the registry names
// exactly the fields that exist — so adding a field to model.go without
// deciding its disposition here fails the test rather than silently vanishing
// across a restart, where it would look like an engine defect.

type fieldDisposition string

const (
	// fieldCopied travels with the snapshot.
	fieldCopied fieldDisposition = "copied"
	// fieldRebuilt is recomputed from the restore target rather than carried —
	// the absolute on-disk paths, which must not survive verbatim.
	fieldRebuilt fieldDisposition = "rebuilt at restore"
	// fieldSkipped is deliberately not carried: mutexes, the Clock (which is
	// re-supplied as an Option), and pure housekeeping counters with no
	// real-GitHub correlate — restoring one to zero is always safe, since it
	// only ever widens the window before the next scheduled housekeeping
	// pass rather than changing an answer.
	fieldSkipped fieldDisposition = "deliberately not carried"
)

var snapshotFieldRegistry = map[string]map[string]fieldDisposition{
	"Sim": {
		"baseDir":               fieldRebuilt,
		"clock":                 fieldSkipped,
		"seedRepoMu":            fieldSkipped,
		"mu":                    fieldSkipped,
		"repos":                 fieldCopied,
		"projects":              fieldCopied,
		"defaultProject":        fieldCopied,
		"nextCommentDatabaseID": fieldCopied,
		"nextCheckRunID":        fieldCopied,
		"seededCheckRunIDs":     fieldCopied,
		"restRate":              fieldCopied,
		"graphqlRate":           fieldCopied,
		"seedErr":               fieldCopied,
	},
	"repoState": {
		"owner":             fieldCopied,
		"repo":              fieldCopied,
		"bareDir":           fieldRebuilt,
		"worktreeRoot":      fieldRebuilt,
		"defaultBranch":     fieldCopied,
		"gitMu":             fieldSkipped,
		"numberMu":          fieldSkipped,
		"issues":            fieldCopied,
		"prs":               fieldCopied,
		"checkRuns":         fieldCopied,
		"commitStatuses":    fieldCopied,
		"ciSchedule":        fieldCopied,
		"requiredContexts":  fieldCopied,
		"requireUpToDate":   fieldCopied,
		"requiredApprovals": fieldCopied,
		"labelVocab":        fieldCopied,
		"access":            fieldCopied,
		"latestRelease":     fieldCopied,
		"mergeQueueEnabled": fieldCopied,
		"nextNumber":        fieldCopied,
		"probesSinceGC":     fieldSkipped,
	},
	"issueRecord": {
		"number":         fieldCopied,
		"title":          fieldCopied,
		"body":           fieldCopied,
		"state":          fieldCopied,
		"author":         fieldCopied,
		"labels":         fieldCopied,
		"assignees":      fieldCopied,
		"labelAppliedAt": fieldCopied,
		"comments":       fieldCopied,
		"blockedBy":      fieldCopied,
		"createdAt":      fieldCopied,
		"updatedAt":      fieldCopied,
	},
	"prRecord": {
		"number":                  fieldCopied,
		"title":                   fieldCopied,
		"body":                    fieldCopied,
		"head":                    fieldCopied,
		"base":                    fieldCopied,
		"state":                   fieldCopied,
		"draft":                   fieldCopied,
		"merged":                  fieldCopied,
		"author":                  fieldCopied,
		"labels":                  fieldCopied,
		"issueNumber":             fieldCopied,
		"comments":                fieldCopied,
		"reviewThreadComment":     fieldCopied,
		"reviews":                 fieldCopied,
		"reviewRequests":          fieldCopied,
		"reviewSchedule":          fieldCopied,
		"resolvedThreads":         fieldCopied,
		"autoMergeEnabled":        fieldCopied,
		"autoMergeStrategy":       fieldCopied,
		"inMergeQueue":            fieldCopied,
		"queueEntry":              fieldCopied,
		"mergeableRecomputeReads": fieldCopied,
		"createdAt":               fieldCopied,
		"updatedAt":               fieldCopied,
	},
	"commentRecord": {
		"databaseID":     fieldCopied,
		"author":         fieldCopied,
		"body":           fieldCopied,
		"createdAt":      fieldCopied,
		"reactions":      fieldCopied,
		"fromPR":         fieldCopied,
		"reviewThreadID": fieldCopied,
		"path":           fieldCopied,
		"line":           fieldCopied,
		"originalLine":   fieldCopied,
		"diffHunk":       fieldCopied,
		"isOutdated":     fieldCopied,
		"threadResolved": fieldCopied,
	},
	"projectState": {
		"id":                 fieldCopied,
		"owner":              fieldCopied,
		"num":                fieldCopied,
		"title":              fieldCopied,
		"statusFieldID":      fieldCopied,
		"statusOptions":      fieldCopied,
		"orderedStatusNames": fieldCopied,
		"items":              fieldCopied,
		"itemOrder":          fieldCopied,
		"updatedAt":          fieldCopied,
	},
	"itemState": {
		"itemID":    fieldCopied,
		"ownerRepo": fieldCopied,
		"number":    fieldCopied,
		"isPR":      fieldCopied,
		"status":    fieldCopied,
		"archived":  fieldCopied,
		"updatedAt": fieldCopied,
	},
}

// snapshotRegisteredTypes ties each registry entry to the type it describes,
// so the completeness test can reflect over it.
var snapshotRegisteredTypes = map[string]reflect.Type{
	"Sim":           reflect.TypeOf(Sim{}),
	"repoState":     reflect.TypeOf(repoState{}),
	"issueRecord":   reflect.TypeOf(issueRecord{}),
	"prRecord":      reflect.TypeOf(prRecord{}),
	"commentRecord": reflect.TypeOf(commentRecord{}),
	"projectState":  reflect.TypeOf(projectState{}),
	"itemState":     reflect.TypeOf(itemState{}),
}
