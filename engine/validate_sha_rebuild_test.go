package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

const rebuildFullHead = "abcdef0123456789abcdef0123456789abcdef01"

func validateStageComment(author, header, commit string) gh.Comment {
	return gh.Comment{
		Author: author,
		Body:   fmt.Sprintf("🏭 **Fabrik — stage: %s**\n*branch: fabrik/issue-1 | commit: %s | main: 1234abcd | 2026-10-15 22:59 UTC*\n\nbody", header, commit),
	}
}

func TestParseStageCommit(t *testing.T) {
	cases := map[string]string{
		"x\n*branch: b | commit: abcdef01 | main: 1111 | ts*":      "abcdef01",
		"x\n*branch: b | commit: ABCDEF01 | ts*":                   "abcdef01",
		"x\n*branch: b | commit: unknown | ts*":                    "",
		"x\n*branch: b | commit: abc | ts*":                        "",
		"x\n*branch: b | commit: abcdefg1 | ts*":                   "",
		"x\n*branch: b | ts*":                                      "",
		"only one line":                                            "",
		"x\n*branch: b | commit: " + strings.Repeat("a", 41) + "*": "",
	}
	for body, want := range cases {
		if got := parseStageCommit(body); got != want {
			t.Errorf("parseStageCommit(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestRebuildValidateSHA(t *testing.T) {
	short := rebuildFullHead[:8]
	own := validateStageComment("testuser", "Validate", short)

	t.Run("unchanged head yields the full live head", func(t *testing.T) {
		got, ok := rebuildValidateSHA([]gh.Comment{own}, "testuser", rebuildFullHead)
		if !ok || got != rebuildFullHead {
			t.Fatalf("got %q,%v", got, ok)
		}
	})
	t.Run("moved head yields the abbreviated commit, which differs from the head", func(t *testing.T) {
		got, ok := rebuildValidateSHA([]gh.Comment{own}, "testuser", "ffffffff00000000000000000000000000000000")
		if !ok || got != short {
			t.Fatalf("got %q,%v", got, ok)
		}
	})
	t.Run("last own comment wins", func(t *testing.T) {
		older := validateStageComment("testuser", "Validate", "11111111")
		got, ok := rebuildValidateSHA([]gh.Comment{older, own}, "testuser", rebuildFullHead)
		if !ok || got != rebuildFullHead {
			t.Fatalf("got %q,%v", got, ok)
		}
	})
	t.Run("foreign author is ignored", func(t *testing.T) {
		if _, ok := rebuildValidateSHA([]gh.Comment{validateStageComment("mallory", "Validate", short)}, "testuser", rebuildFullHead); ok {
			t.Fatal("spoofed comment accepted")
		}
	})
	t.Run("comment-review variants never match", func(t *testing.T) {
		for _, h := range []string{"Validate (comment review)", "Validate (review feedback addressed)"} {
			if _, ok := rebuildValidateSHA([]gh.Comment{validateStageComment("testuser", h, short)}, "testuser", rebuildFullHead); ok {
				t.Fatalf("%q matched", h)
			}
		}
		// ... and cannot pull the SHA forward past the real stage comment.
		later := validateStageComment("testuser", "Validate (comment review)", "99999999")
		got, ok := rebuildValidateSHA([]gh.Comment{own, later}, "testuser", rebuildFullHead)
		if !ok || got != rebuildFullHead {
			t.Fatalf("got %q,%v", got, ok)
		}
	})
	t.Run("unknown commit is rejected", func(t *testing.T) {
		if _, ok := rebuildValidateSHA([]gh.Comment{validateStageComment("testuser", "Validate", "unknown")}, "testuser", rebuildFullHead); ok {
			t.Fatal("unknown accepted")
		}
	})
	t.Run("empty self is rejected", func(t *testing.T) {
		if _, ok := rebuildValidateSHA([]gh.Comment{own}, "", rebuildFullHead); ok {
			t.Fatal("accepted with empty self")
		}
	})
	t.Run("a long thread still yields the own Validate comment", func(t *testing.T) {
		// FetchItemDetails pages through every comment, so 100+ is not truncation.
		cs := make([]gh.Comment, 150)
		cs[0] = own
		if _, ok := rebuildValidateSHA(cs, "testuser", rebuildFullHead); !ok {
			t.Fatal("long thread rejected")
		}
	})
}

// rebuildItemFixture sets up an engine whose Store knows the linked PR head but has no
// completion SHA — a restart over an item at Validate-complete.
func rebuildItemFixture(t *testing.T, labels []string, comments []gh.Comment, head string) (*Engine, *mockGitHubClient) {
	t.Helper()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{
					Number: 50, ItemID: "PVTI_50", Status: "Validate", Repo: "owner/repo",
					Labels: labels, Comments: comments,
				}},
			}, nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesForSHAInvalidation())
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 50, LinkedPRNum: 110, SHA: head})
	return eng, client
}

func removedLabels50(client *mockGitHubClient) map[string]bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	out := map[string]bool{}
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 50 {
			out[c.labelName] = true
		}
	}
	return out
}

var rebuildLabels = []string{"stage:Validate:complete", "fabrik:auto-merge-enabled", "fabrik:yolo"}

func TestHydrate_MovedHeadAfterRestartInvalidatesValidate(t *testing.T) {
	cs := []gh.Comment{validateStageComment("testuser", "Validate", rebuildFullHead[:8])}
	eng, client := rebuildItemFixture(t, rebuildLabels, cs, "ffffffff00000000000000000000000000000000")
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !removedLabels50(client)["stage:Validate:complete"] {
		t.Fatalf("Validate not invalidated: %v", removedLabels50(client))
	}
}

func TestHydrate_MovedHeadNonVacuity(t *testing.T) {
	cs := []gh.Comment{validateStageComment("testuser", "Validate", rebuildFullHead[:8])}
	eng, client := rebuildItemFixture(t, rebuildLabels, cs, "ffffffff00000000000000000000000000000000")
	defer eng.SetValidateSHARebuildDisabledForTest()()
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if removedLabels50(client)["stage:Validate:complete"] {
		t.Fatal("with the rebuild disabled the item must not be invalidated (old behaviour)")
	}
}

func TestHydrate_UnchangedHeadRecordsFullHeadAndKeepsLabels(t *testing.T) {
	cs := []gh.Comment{validateStageComment("testuser", "Validate", rebuildFullHead[:8])}
	eng, client := rebuildItemFixture(t, rebuildLabels, cs, rebuildFullHead)
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := removedLabels50(client); got["stage:Validate:complete"] {
		t.Fatalf("unchanged head invalidated Validate: %v", got)
	}
	snap, _ := eng.store.Get("owner/repo", 50)
	if snap.ValidateCompletedSHA() != rebuildFullHead {
		t.Fatalf("recorded %q, want full head", snap.ValidateCompletedSHA())
	}
}

func TestHydrate_NoRecoverableSHARerunsValidate(t *testing.T) {
	eng, client := rebuildItemFixture(t, rebuildLabels, nil, rebuildFullHead)
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !removedLabels50(client)["stage:Validate:complete"] {
		t.Fatalf("R3: Validate not re-run when no SHA is recoverable: %v", removedLabels50(client))
	}
}

func TestHydrate_SkipsLandedWorkerAndCatchUpInFlight(t *testing.T) {
	head := "ffffffff00000000000000000000000000000000"
	cs := []gh.Comment{validateStageComment("testuser", "Validate", rebuildFullHead[:8])}

	t.Run("landed", func(t *testing.T) {
		eng, client := rebuildItemFixture(t, append([]string{"fabrik:awaiting-landing-verification"}, rebuildLabels...), cs, head)
		if _, err := eng.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if removedLabels50(client)["stage:Validate:complete"] {
			t.Fatal("landed item invalidated")
		}
	})
	t.Run("worker in flight", func(t *testing.T) {
		eng, client := rebuildItemFixture(t, rebuildLabels, nil, head)
		eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 50})
		if _, err := eng.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if removedLabels50(client)["stage:Validate:complete"] {
			t.Fatal("item with a worker in flight invalidated")
		}
	})
	t.Run("catch-up push in flight", func(t *testing.T) {
		eng, client := rebuildItemFixture(t, rebuildLabels, nil, head)
		eng.trainCatchUp.mu.Lock()
		eng.trainCatchUp.pushing = map[string]int{}
		eng.trainCatchUp.pushing[catchUpPRKey("owner", "repo", 110)] = 1
		eng.trainCatchUp.mu.Unlock()
		if _, err := eng.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if removedLabels50(client)["stage:Validate:complete"] {
			t.Fatal("item mid catch-up invalidated")
		}
	})
}

// R4: after a restart the engine's own pure singleton catch-up merge is still not an
// external push — the rebuilt (older) SHA is re-recorded as the new head, not invalidated.
func TestHydrate_PureCatchUpHeadAfterRestartKeepsValidate(t *testing.T) {
	head := "ffffffff00000000000000000000000000000000"
	cs := []gh.Comment{validateStageComment("testuser", "Validate", rebuildFullHead[:8])}
	eng, client := rebuildItemFixture(t, rebuildLabels, cs, head)
	client.fetchIssueCommentsFn = func(owner, repo string, n int) ([]gh.Comment, error) {
		return []gh.Comment{{
			Author: "testuser",
			Body:   fmt.Sprintf("<!-- fabrik:train-catch-up head=%s base=1234abcd pure=true -->", head),
		}}, nil
	}
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if removedLabels50(client)["stage:Validate:complete"] {
		t.Fatal("pure catch-up head invalidated Validate after restart")
	}
	snap, _ := eng.store.Get("owner/repo", 50)
	if snap.ValidateCompletedSHA() != head {
		t.Fatalf("completion SHA = %q, want the catch-up head re-recorded", snap.ValidateCompletedSHA())
	}
}
