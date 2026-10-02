package sim

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/e2e/seedspec"
)

// updateSeedFixtures rewrites tests/e2e/seedspec/testdata/*.json from a fresh engine
// traversal instead of comparing against them:
//
//	go test ./tests/sim -run TestSeedFixturesMatchEngineTraversal -update-seed-fixtures
var updateSeedFixtures = flag.Bool("update-seed-fixtures", false,
	"rewrite tests/e2e/seedspec/testdata from a fresh engine traversal (#1992)")

// seedFixtureDir is where the recorded real-traversal fixtures live. They are read
// by tests/e2e/seedspec's own tests (a pure comparison, no sim) and re-derived and
// compared here.
var seedFixtureDir = filepath.Join("..", "e2e", "seedspec", "testdata")

// parkAtStages returns smokeStages with auto_advance on every stage before column
// (the Env runs with yolo off — see captureTraversal — so nothing else advances it),
// so a label-free item walks Specify → … → column and then parks there with
// stage:<column>:complete, exactly the state a seed claims to reproduce.
func parkAtStages(column string) []*stages.Stage {
	tr := true
	stgs := smokeStages()
	for _, s := range stgs {
		if s.Name == column {
			break
		}
		s.AutoAdvance = &tr
	}
	return stgs
}

// captureTraversal drives one issue through the real engine to column and records
// the parked state. Labels that embed the issue number are not expected at park
// time; any that do are normalised so the fixture is run-independent.
func captureTraversal(t *testing.T, column string) seedspec.Fixture {
	t.Helper()
	noYolo := false
	env := NewEnv(t, EnvOptions{Stages: parkAtStages(column), Yolo: &noYolo})
	num := FileIssue(t, env, "seed fidelity: "+column, "Park an item at "+column+" so its state can be recorded.", "Specify")
	WaitForIssueLabel(t, env, num, "stage:"+column+":complete", 80)
	// A few more polls: whatever the engine does after completion (label cleanup,
	// advance, PR-ready) has happened by the time the state is read, and the item
	// must still be parked at column.
	RunPolls(t, env, 3)

	item := projectItem(t, env, num)
	if item.Status != column {
		t.Fatalf("item advanced past %s: Status=%q, labels=%v", column, item.Status, item.Labels)
	}
	fx := seedspec.Fixture{
		Version:    seedspec.FixtureVersion,
		Column:     column,
		Status:     item.Status,
		Provenance: "sim-engine",
		EngineSHA:  engineSHA(t),
	}
	needle := fmt.Sprintf("%d", num)
	for _, l := range item.Labels {
		fx.Labels = append(fx.Labels, strings.ReplaceAll(l, needle, "<N>"))
	}
	sort.Strings(fx.Labels)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr != nil && pr.Number != 0 {
		base, err := env.Sim.GetPRBase(env.Owner, env.Repo, pr.Number)
		if err != nil {
			t.Fatalf("GetPRBase: %v", err)
		}
		// FetchLinkedPR does not carry the head ref or body; ListPRs does.
		prs, err := env.Sim.ListPRs(env.Owner, env.Repo)
		if err != nil {
			t.Fatalf("ListPRs: %v", err)
		}
		var head, body string
		for _, p := range prs {
			if p.Number == pr.Number {
				head, body = p.HeadRefName, p.Body
			}
		}
		fx.PR = &seedspec.FixturePR{
			Draft:             pr.Draft,
			Base:              base,
			HeadIsIssueBranch: head == fmt.Sprintf("fabrik/issue-%d", num),
			ClosesIssue:       strings.Contains(body, fmt.Sprintf("Closes #%d", num)),
		}
	}
	return fx
}

func engineSHA(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestSeedFixturesMatchEngineTraversal is R4's recorded-fixture guard. For each
// pipeline column a seed can target, it drives the real Engine there with the
// scripted invoker and checks that tests/e2e/seedspec/testdata still describes what
// the engine produces — labels, board column and PR shape. The seeds themselves are
// checked against these fixtures in tests/e2e/seedspec (TestPlanMatchesRecordedTraversal),
// so engine drift fails here and an impossible seed fails there; both run in plain
// `go test ./...` with no live budget. With -update-seed-fixtures the fixtures are
// rewritten instead (the documented recapture procedure, tests/e2e/README.md).
//
// EngineSHA is informational and never compared: it moves with every commit.
func TestSeedFixturesMatchEngineTraversal(t *testing.T) {
	for _, column := range seedspec.Stages[1:] {
		column := column
		t.Run(column, func(t *testing.T) {
			t.Parallel()
			got := captureTraversal(t, column)
			path := filepath.Join(seedFixtureDir, strings.ToLower(column)+".json")
			if *updateSeedFixtures {
				data, err := got.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(seedFixtureDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("rewrote %s", path)
				return
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading recorded fixture: %v (record it with -update-seed-fixtures)", err)
			}
			want, err := seedspec.ParseFixture(raw)
			if err != nil {
				t.Fatal(err)
			}
			got.EngineSHA, want.EngineSHA = "", ""
			if !reflect.DeepEqual(got, want) {
				g, _ := got.Marshal()
				w, _ := want.Marshal()
				t.Fatalf("the engine no longer produces the recorded %s state — the seed fixtures have drifted.\n"+
					"If the engine change is intended, recapture with `go test ./tests/sim -run TestSeedFixturesMatchEngineTraversal -update-seed-fixtures`, "+
					"review the diff, and fix any seed that CheckFidelity then rejects.\nrecorded:\n%s\nengine now:\n%s", column, w, g)
			}
		})
	}
}
