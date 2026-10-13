package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ---- #2091: tolerant headers and surfaced malformed blocks ----

func TestParseSpawnBlocks_HeaderCaseAndWhitespaceVariants(t *testing.T) {
	canonical := "FABRIK_SPAWN_CHILD_BEGIN owner/repo\nTITLE: Fix Y\nDEPENDS_ON: 3\nBody.\nFABRIK_SPAWN_CHILD_END\n"
	want := ParseSpawnBlocks(canonical)
	if len(want) != 1 || want[0].Title != "Fix Y" || want[0].DependsOn != 3 {
		t.Fatalf("canonical parse unexpected: %+v", want)
	}
	for _, hdr := range []struct{ title, dep string }{
		{"title: Fix Y", "depends_on: 3"},
		{"Title: Fix Y", "Depends_On: 3"},
		{" TITLE : Fix Y", " DEPENDS_ON : 3\r"},
		{"\ttitle\t:\tFix Y\r", "\tdepends_on:\t3"},
	} {
		body := "FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" + hdr.title + "\n" + hdr.dep + "\nBody.\nFABRIK_SPAWN_CHILD_END\n"
		blocks, errs := ParseSpawnBlocksDetailed(body)
		if len(errs) != 0 || len(blocks) != 1 {
			t.Fatalf("%q/%q: blocks=%d errs=%v", hdr.title, hdr.dep, len(blocks), errs)
		}
		if blocks[0] != want[0] {
			t.Errorf("%q/%q: got %+v, want %+v", hdr.title, hdr.dep, blocks[0], want[0])
		}
	}
}

func TestParseSpawnBlocks_DependsOnBlankLineStillBody_CaseInsensitive(t *testing.T) {
	body := "FABRIK_SPAWN_CHILD_BEGIN owner/repo\ntitle: T\n\ndepends_on: 1\nrest\nFABRIK_SPAWN_CHILD_END\n"
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 || blocks[0].DependsOnDeclared {
		t.Fatalf("blank-line-separated depends_on must stay body: %+v", blocks)
	}
}

func TestParseSpawnBlocksDetailed_GoodMalformedGood(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/a
TITLE: First
one
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/b
Summary: not a title @someone ` + "`x`" + `
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/c
title: Third
three
FABRIK_SPAWN_CHILD_END
`
	blocks, errs := ParseSpawnBlocksDetailed(body)
	if len(blocks) != 2 || blocks[0].Title != "First" || blocks[1].Title != "Third" {
		t.Fatalf("blocks: %+v", blocks)
	}
	if len(errs) != 1 {
		t.Fatalf("errs: %+v", errs)
	}
	e := errs[0]
	if e.Index != 2 || e.Repo != "owner/b" {
		t.Errorf("index/repo: %+v", e)
	}
	if !strings.Contains(e.Found, "Summary: not a title") || strings.ContainsAny(e.Found, "`\n") || strings.Contains(e.Found, "@someone") {
		t.Errorf("found line not quoted/sanitised: %q", e.Found)
	}
	if !strings.Contains(e.String(), "block 2") {
		t.Errorf("String(): %q", e.String())
	}
}

func TestParseSpawnBlocksDetailed_EmptyAndEmptyTitleBlocks(t *testing.T) {
	body := "FABRIK_SPAWN_CHILD_BEGIN owner/a\n\nFABRIK_SPAWN_CHILD_END\nFABRIK_SPAWN_CHILD_BEGIN owner/b\nTITLE:\nbody\nFABRIK_SPAWN_CHILD_END\n"
	blocks, errs := ParseSpawnBlocksDetailed(body)
	if len(blocks) != 0 || len(errs) != 2 {
		t.Fatalf("blocks=%d errs=%v", len(blocks), errs)
	}
	if !strings.Contains(errs[0].Reason, "empty") || errs[0].Found != "" {
		t.Errorf("errs[0]: %+v", errs[0])
	}
	if !strings.Contains(errs[1].Reason, "no value") || errs[1].Found != "TITLE:" {
		t.Errorf("errs[1]: %+v", errs[1])
	}
}

func TestParseSpawnBlocksDetailed_MissingEndNoError(t *testing.T) {
	_, errs := ParseSpawnBlocksDetailed("FABRIK_SPAWN_CHILD_BEGIN owner/a\nTITLE: x\n")
	if len(errs) != 0 {
		t.Fatalf("unterminated block keeps its existing silent behaviour, got %v", errs)
	}
}

func TestFormatSpawnReceiptNote_WarnsOnMalformed(t *testing.T) {
	out := "FABRIK_SPAWN_CHILD_BEGIN owner/a\nnot a header\nFABRIK_SPAWN_CHILD_END\n"
	note := formatSpawnReceiptNote(out)
	if !strings.Contains(note, "could not be parsed") || !strings.Contains(note, "block 1") || !strings.Contains(note, "not a header") {
		t.Errorf("note: %q", note)
	}
}

func TestPreImplement_MalformedBlock_PausesAndSpawnsNothing(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Good one
Body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
no header on this one
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil || spawned {
		t.Fatalf("expected (false, err), got (%v, %v)", spawned, err)
	}
	if len(client.createIssueCalls) != 0 {
		t.Errorf("no child may be created when any block is malformed, got %v", client.createIssueCalls)
	}
	var paused bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	if !paused {
		t.Error("expected fabrik:paused")
	}
	var comment string
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "malformed FABRIK_SPAWN_CHILD marker") {
			comment = c.body
		}
	}
	if comment == "" {
		t.Fatalf("expected malformed-marker comment, got %v", client.addCommentCalls)
	}
	if !strings.HasPrefix(comment, "🏭 **Fabrik") || !strings.Contains(comment, "block 2") || !strings.Contains(comment, "no header on this one") {
		t.Errorf("comment should name block 2 and the offending line: %s", comment)
	}
}

func TestPreImplement_LowercaseTitleStillSpawns(t *testing.T) {
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			return 101, "I_child1", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client)
	item := planItemWithBlocks("FABRIK_SPAWN_CHILD_BEGIN owner/child\ntitle: lower\nBody.\nFABRIK_SPAWN_CHILD_END\n")
	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err != nil || !spawned {
		t.Fatalf("got (%v, %v)", spawned, err)
	}
	if len(client.createIssueCalls) != 1 || client.createIssueCalls[0].title != "lower" {
		t.Errorf("createIssueCalls: %v", client.createIssueCalls)
	}
}

func TestFinalizeStageOutcome_ReviewSpawn_MalformedBlockPauses(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := midflightSpawnClient("I_never", 999, 42)
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" +
				"Fix the library first\n" +
				"FABRIK_SPAWN_CHILD_END\n" +
				"FABRIK_STAGE_COMPLETE\n"
			return output, true, TokenUsage{TurnsUsed: 3, MaxTurns: 30}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, reviewStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{ID: "I_parent", Number: 103, Title: "Parent", Status: "Review", ItemID: "PVTI_103"}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(client.createIssueCalls) != 0 {
		t.Errorf("no child may be created, got %v", client.createIssueCalls)
	}
	var paused, complete bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			paused = true
		case "stage:Review:complete":
			complete = true
		}
	}
	if !paused || complete {
		t.Errorf("paused=%v complete=%v, want paused and not complete", paused, complete)
	}
	var found bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "malformed FABRIK_SPAWN_CHILD marker") &&
			strings.Contains(c.body, "block 1") && strings.Contains(c.body, "Fix the library first") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected comment naming block 1 and the offending line, got %v", client.addCommentCalls)
	}
}
