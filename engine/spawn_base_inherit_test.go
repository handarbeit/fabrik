package engine

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// spawnOrderRecorder wires a mock's label and Status writes into one ordered
// event log, so a test can assert which label writes preceded a child's Status
// placement (#2090 R2) — the mock itself keeps the two call kinds in separate
// slices with no global sequence.
type spawnOrderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *spawnOrderRecorder) add(ev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *spawnOrderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// spawnEventIndex returns the position of the first event equal to ev, or -1.
func spawnEventIndex(events []string, ev string) int {
	for i, e := range events {
		if e == ev {
			return i
		}
	}
	return -1
}

func newOrderedSpawnClient(rec *spawnOrderRecorder) *mockGitHubClient {
	next := 100
	return &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			next++
			return next, "I_child", nil
		},
		addLabelToIssueFn: func(owner, repo string, n int, label string) error {
			rec.add("label:" + repo + "#" + strconv.Itoa(n) + ":" + label)
			return nil
		},
		updateProjectItemStatusFn: func(projectID, itemID, fieldID, optionID string) error {
			rec.add("status:" + itemID)
			return nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
}

const sameRepoTwoChildren = `
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Child one
Body one.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Child two
Body two.
FABRIK_SPAWN_CHILD_END
`

func labelWrites(client *mockGitHubClient, repo string, n int) []string {
	var out []string
	for _, c := range client.addLabelCalls {
		if c.repo == repo && c.issueNumber == n {
			out = append(out, c.labelName)
		}
	}
	return out
}

// TestSpawnChildren_SameRepo_InheritsBase_BeforeStatus is the core #2090
// acceptance test: every same-repo child of a base:develop parent carries
// base:develop, and (with yolo and cruise) every inherited label write precedes
// that child's Status placement.
func TestSpawnChildren_SameRepo_InheritsBase_BeforeStatus(t *testing.T) {
	rec := &spawnOrderRecorder{}
	client := newOrderedSpawnClient(rec)
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(sameRepoTwoChildren)
	item.Labels = append(item.Labels, "base:develop", "fabrik:yolo", "fabrik:cruise")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if spawned, err := eng.preImplement(context.Background(), board, item); err != nil || !spawned {
		t.Fatalf("preImplement: spawned=%v err=%v", spawned, err)
	}

	events := rec.snapshot()
	for _, n := range []int{101, 102} {
		for _, l := range []string{"base:develop", "fabrik:yolo", "fabrik:cruise"} {
			got := labelWrites(client, "repo", n)
			if !hasLabel(got, l) {
				t.Errorf("child #%d: %s not added (writes: %v)", n, l, got)
			}
		}
	}
	// Every label write for a child must precede that child's Status write.
	// Both children share a board item ID in this mock, so check by position:
	// the first Status event must come after the first child's three labels,
	// and the second Status event after the second child's.
	var statusPos []int
	for i, e := range events {
		if strings.HasPrefix(e, "status:") {
			statusPos = append(statusPos, i)
		}
	}
	if len(statusPos) != 2 {
		t.Fatalf("want 2 Status writes, got events %v", events)
	}
	for ci, n := range []int{101, 102} {
		for _, l := range []string{"base:develop", "fabrik:yolo", "fabrik:cruise"} {
			pos := spawnEventIndex(events, "label:repo#"+strconv.Itoa(n)+":"+l)
			if pos < 0 || pos > statusPos[ci] {
				t.Errorf("child #%d: label %s at %d must precede its Status write at %d (events %v)", n, l, pos, statusPos[ci], events)
			}
		}
	}
}

func TestSpawnChildren_NoParentBase_NoBaseLabel(t *testing.T) {
	client := newOrderedSpawnClient(&spawnOrderRecorder{})
	eng := spawnTestEngineWithSpecify(t, client)
	item := planItemWithBlocks(sameRepoTwoChildren)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	for _, c := range client.addLabelCalls {
		if strings.HasPrefix(c.labelName, "base:") {
			t.Errorf("unexpected base label %q added to child #%d", c.labelName, c.issueNumber)
		}
	}
}

func TestSpawnChildren_MultipleParentBase_AppliesOnlyFirst(t *testing.T) {
	client := newOrderedSpawnClient(&spawnOrderRecorder{})
	eng := spawnTestEngineWithSpecify(t, client)
	item := planItemWithBlocks(sameRepoTwoChildren)
	item.Labels = append(item.Labels, "base:develop", "base:release/1")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{101, 102} {
		var bases []string
		for _, l := range labelWrites(client, "repo", n) {
			if strings.HasPrefix(l, "base:") {
				bases = append(bases, l)
			}
		}
		if len(bases) != 1 || bases[0] != "base:develop" {
			t.Errorf("child #%d base labels = %v, want [base:develop]", n, bases)
		}
	}
}

// TestSpawnChildren_CrossRepo_NoBase_StillInheritsAutonomy: a child in another
// repo gets no base: label (the branch may not exist there) but keeps the
// autonomy labels, now applied before its Status placement.
func TestSpawnChildren_CrossRepo_NoBase_StillInheritsAutonomy(t *testing.T) {
	rec := &spawnOrderRecorder{}
	client := newOrderedSpawnClient(rec)
	eng := spawnTestEngineWithSpecify(t, client)
	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Same repo
Body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Other repo
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "base:develop", "fabrik:yolo")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	same := labelWrites(client, "repo", 101)
	cross := labelWrites(client, "child", 102)
	if !hasLabel(same, "base:develop") {
		t.Errorf("same-repo child writes %v, want base:develop", same)
	}
	if hasLabel(cross, "base:develop") {
		t.Errorf("cross-repo child must not inherit base:, writes %v", cross)
	}
	if !hasLabel(cross, "fabrik:yolo") {
		t.Errorf("cross-repo child should still inherit fabrik:yolo, writes %v", cross)
	}
	events := rec.snapshot()
	// The cross-repo child's yolo write must precede that child's (the second)
	// Status write.
	y := spawnEventIndex(events, "label:child#102:fabrik:yolo")
	lastStatus := -1
	for i, e := range events {
		if strings.HasPrefix(e, "status:") {
			lastStatus = i
		}
	}
	if y < 0 || lastStatus < 0 || y > lastStatus {
		t.Errorf("cross-repo yolo (at %d) must precede its Status write (at %d): %v", y, lastStatus, events)
	}
}

func TestInheritChildLabels_RepoComparisonIsCaseInsensitive(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)
	parent := gh.ProjectItem{Number: 42, Labels: []string{"base:develop"}}
	eng.inheritChildLabels(parent, "owner", "repo", "Owner/Repo", "Owner", "Repo", 7, false, nil)
	if got := labelWrites(client, "Repo", 7); !hasLabel(got, "base:develop") {
		t.Errorf("differently-cased same repo must inherit base:, writes %v", got)
	}
}

func resumeParent(labels ...string) gh.ProjectItem {
	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Child one
Body one.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, spawnChildLabel(1, 101))
	item.Labels = append(item.Labels, labels...)
	return item
}

// TestSpawnChildren_Resume_AppliesMissingBase_Idempotently: a recorded child
// that lacks the parent's base: gets it; a child that already has it gets no
// further label write (SC-003).
func TestSpawnChildren_Resume_AppliesMissingBase_Idempotently(t *testing.T) {
	for _, tc := range []struct {
		name       string
		childHas   []string
		wantWrites []string
	}{
		{"missing", []string{"fabrik:sub-issue"}, []string{"base:develop", "fabrik:yolo"}},
		{"already present", []string{"base:develop", "fabrik:yolo"}, nil},
		{"partly present", []string{"base:develop"}, []string{"fabrik:yolo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchProjectItemFn: func(owner, repo string, n int) (*gh.ProjectItem, error) {
					return &gh.ProjectItem{ID: "I_child101", Number: 101, Repo: "owner/repo", Labels: tc.childHas}, nil
				},
				lookupIssueProjectItemFn: func(projectID, repo string, n int) (string, string, error) {
					return "PVTI_existing", "", nil
				},
			}
			eng := spawnTestEngineWithSpecify(t, client)
			item := resumeParent("base:develop", "fabrik:yolo")
			board := &gh.ProjectBoard{ProjectID: "PVT_1"}
			if _, err := eng.preImplement(context.Background(), board, item); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, l := range labelWrites(client, "repo", 101) {
				if l != "fabrik:sub-issue" {
					got = append(got, l)
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.wantWrites, ",") {
				t.Errorf("label writes = %v, want %v", got, tc.wantWrites)
			}
		})
	}
}

// TestSpawnChildren_Resume_PlacedChild_GetsLabelsButKeepsStatus: a resumed
// child that already has a Status receives the missing label (R4) and its
// Status is left untouched (FR-009).
func TestSpawnChildren_Resume_PlacedChild_GetsLabelsButKeepsStatus(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectItemFn: func(owner, repo string, n int) (*gh.ProjectItem, error) {
			return &gh.ProjectItem{ID: "I_child101", Number: 101, Repo: "owner/repo"}, nil
		},
		lookupIssueProjectItemFn: func(projectID, repo string, n int) (string, string, error) {
			return "PVTI_existing", "Implement", nil
		},
		updateProjectItemStatusFn: func(projectID, itemID, fieldID, optionID string) error {
			t.Error("UpdateProjectItemStatus must not be called for an already-placed child")
			return nil
		},
	}
	eng := spawnTestEngineWithSpecify(t, client)
	item := resumeParent("base:develop")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	if got := labelWrites(client, "repo", 101); !hasLabel(got, "base:develop") {
		t.Errorf("placed resumed child should still get base:develop, writes %v", got)
	}
}

// TestSpawnChildren_BaseLabelFailure_IsNonFatal: a failed inherited-label write
// is logged and the spawn completes, still placing the child.
func TestSpawnChildren_BaseLabelFailure_IsNonFatal(t *testing.T) {
	client := newOrderedSpawnClient(&spawnOrderRecorder{})
	client.addLabelToIssueFn = func(owner, repo string, n int, label string) error {
		if strings.HasPrefix(label, "base:") {
			return errors.New("boom")
		}
		return nil
	}
	eng := spawnTestEngineWithSpecify(t, client)
	item := planItemWithBlocks(sameRepoTwoChildren)
	item.Labels = append(item.Labels, "base:develop")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil || !spawned {
		t.Fatalf("spawn must survive a failed base: label write: spawned=%v err=%v", spawned, err)
	}
	if len(client.updateStatusCalls) != 2 {
		t.Errorf("both children should still be placed, got %d Status writes", len(client.updateStatusCalls))
	}
}
