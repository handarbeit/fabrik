package simgh

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// The engine's poll loop dispatches up to MaxConcurrent workers against one
// GitHubClient, so the model has to survive that. Two hazards are being
// checked here, and they are different: the in-memory model needs a mutex, and
// git itself needs its subprocess calls serialised per repository — git's
// index and worktree administration are not concurrency-safe, so a Go-level
// lock over the model alone would not be enough.
//
// Operation counts are kept modest on purpose: creating and tearing down a
// throwaway worktree is by far the most expensive thing the model does, and an
// unbounded fan-out here would make the suite slow without testing anything
// extra.

func TestConcurrentModelAccess(t *testing.T) {
	s, _ := newSim(t)

	const (
		repoA     = "acme/widgets"
		repoB     = "acme/gadgets"
		issuesPer = 4
	)
	s.SeedRepo(repoA).
		SeedRepo(repoB).
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"})
	for i := 1; i <= issuesPer; i++ {
		s.SeedIssue(repoA, IssueSeed{Number: i, Title: fmt.Sprintf("A%d", i), Status: "Implement"}).
			SeedIssue(repoB, IssueSeed{Number: i, Title: fmt.Sprintf("B%d", i), Status: "Implement"})
	}
	// One PR per repo, on a real branch, so the goroutines below exercise the
	// git-backed paths concurrently and across two repos (whose gitMu locks are
	// independent).
	for _, repo := range []string{repoA, repoB} {
		s.SeedCommit(repo, "main", map[string]string{"base.txt": "base\n"}, "base").
			SeedCommit(repo, issueBranch(1), map[string]string{"feature.txt": "feature\n"}, "feature").
			SeedPR(repo, PRSeed{Number: 100, Head: issueBranch(1), Base: "main"})
	}
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	field, err := s.FetchStatusField(board.ProjectID)
	if err != nil {
		t.Fatalf("FetchStatusField: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// Writers: label churn, comments and reactions, and body updates, spread
	// across both repos and every issue.
	for _, repo := range []string{repoA, repoB} {
		owner, name, err := splitOwnerRepo(repo)
		if err != nil {
			t.Fatalf("splitOwnerRepo: %v", err)
		}
		for i := 1; i <= issuesPer; i++ {
			wg.Add(1)
			go func(owner, name string, num int) {
				defer wg.Done()
				const label = "stage:Implement:in_progress"
				if err := s.AddLabelToIssue(owner, name, num, label); err != nil {
					errs <- fmt.Errorf("AddLabelToIssue: %w", err)
					return
				}
				id, err := s.AddComment(owner, name, num, "worker output")
				if err != nil {
					errs <- fmt.Errorf("AddComment: %w", err)
					return
				}
				if err := s.AddCommentReaction(owner, name, id, "ROCKET"); err != nil {
					errs <- fmt.Errorf("AddCommentReaction: %w", err)
					return
				}
				if err := s.UpdateIssueBody(owner, name, num, fmt.Sprintf("body %d", num)); err != nil {
					errs <- fmt.Errorf("UpdateIssueBody: %w", err)
					return
				}
				if err := s.RemoveLabelFromIssue(owner, name, num, label); err != nil {
					errs <- fmt.Errorf("RemoveLabelFromIssue: %w", err)
				}
			}(owner, name, i)
		}
	}

	// Board movers, contending on the same project state.
	for i := 1; i <= issuesPer; i++ {
		wg.Add(1)
		go func(num int) {
			defer wg.Done()
			itemID, _, err := s.LookupIssueProjectItem(board.ProjectID, repoA, num)
			if err != nil {
				errs <- fmt.Errorf("LookupIssueProjectItem: %w", err)
				return
			}
			if itemID == "" {
				errs <- fmt.Errorf("issue %d not found on the board", num)
				return
			}
			if err := s.UpdateProjectItemStatus(board.ProjectID, itemID, field.FieldID, field.Options["Review"]); err != nil {
				errs <- fmt.Errorf("UpdateProjectItemStatus: %w", err)
			}
		}(i)
	}

	// Readers, including full board projections that touch git for head SHAs.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.FetchProjectBoard("acme", "widgets", 2, "organization"); err != nil {
				errs <- fmt.Errorf("FetchProjectBoard: %w", err)
			}
			if _, _, err := s.ProbeProjectBoard("acme", "widgets", 2, "organization"); err != nil {
				errs <- fmt.Errorf("ProbeProjectBoard: %w", err)
			}
		}()
	}

	// Git-backed readers. These are the ones that would corrupt each other
	// without per-repo serialisation of the git subprocess calls: two
	// concurrent trial merges in the same repository share its object store
	// and worktree administration.
	for _, repo := range []string{repoA, repoB} {
		owner, name, err := splitOwnerRepo(repo)
		if err != nil {
			t.Fatalf("splitOwnerRepo: %v", err)
		}
		for range 2 {
			wg.Add(1)
			go func(owner, name string) {
				defer wg.Done()
				mergeable, err := s.FetchPRMergeable(owner, name, 100)
				if err != nil {
					errs <- fmt.Errorf("FetchPRMergeable: %w", err)
					return
				}
				if mergeable == nil || !*mergeable {
					errs <- fmt.Errorf("FetchPRMergeable = %v, want true", mergeable)
					return
				}
				if _, err := s.FetchCommitsBehind(owner, name, "main", issueBranch(1)); err != nil {
					errs <- fmt.Errorf("FetchCommitsBehind: %w", err)
				}
			}(owner, name)
		}
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Every write must have landed — a lost update under contention is the
	// failure this test exists to catch, and it would not show up as a race
	// report.
	for _, repo := range []string{repoA, repoB} {
		owner, name, err := splitOwnerRepo(repo)
		if err != nil {
			t.Fatalf("splitOwnerRepo: %v", err)
		}
		for i := 1; i <= issuesPer; i++ {
			body, err := s.GetIssueBody(owner, name, i)
			if err != nil {
				t.Fatalf("GetIssueBody(%s#%d): %v", repo, i, err)
			}
			if want := fmt.Sprintf("body %d", i); body != want {
				t.Errorf("%s#%d body = %q, want %q", repo, i, body, want)
			}
			issue, err := s.FetchIssue(owner, name, i)
			if err != nil {
				t.Fatalf("FetchIssue(%s#%d): %v", repo, i, err)
			}
			if issue.Comments != 1 {
				t.Errorf("%s#%d has %d comments, want 1", repo, i, issue.Comments)
			}
		}
	}
	statuses, err := s.FetchProjectItemStatusBatch(board.ProjectID)
	if err != nil {
		t.Fatalf("FetchProjectItemStatusBatch: %v", err)
	}
	moved := 0
	for _, status := range statuses {
		if status == "Review" {
			moved++
		}
	}
	if moved != issuesPer {
		t.Errorf("%d items moved to Review, want %d", moved, issuesPer)
	}
}

// TestConcurrentTrialMergesOnOneRepo runs repeated trial merges against one
// repository from several goroutines, checking that two probes of different
// PRs never contaminate each other's answer.
//
// Note what this does *not* prove: removing the per-repo git mutex leaves it
// green, because each probe works in its own throwaway worktree and moves no
// ref. TestConcurrentMergesOntoOneBase is the case that pins the mutex.
func TestConcurrentTrialMergesOnOneRepo(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo(repoName).
		SeedCommit(repoName, "main", map[string]string{"shared.txt": "original\n"}, "ancestor").
		SeedCommit(repoName, headBranch, map[string]string{"shared.txt": "feature\n"}, "feature edit").
		SeedCommit(repoName, "main", map[string]string{"shared.txt": "main\n"}, "main edit").
		SeedCommit(repoName, "clean-branch", map[string]string{"other.txt": "other\n"}, "unrelated").
		SeedPR(repoName, PRSeed{Number: 1, Head: headBranch, Base: "main"}).
		SeedPR(repoName, PRSeed{Number: 2, Head: "clean-branch", Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The conflicting PR must report false every time...
			conflicting, err := s.FetchPRMergeable("acme", "widgets", 1)
			if err != nil {
				errs <- fmt.Errorf("conflicting probe: %w", err)
				return
			}
			if conflicting == nil || *conflicting {
				errs <- fmt.Errorf("conflicting PR reported mergeable = %v, want false", conflicting)
			}
			// ...and the clean one true, concurrently, in the same repo.
			clean, err := s.FetchPRMergeable("acme", "widgets", 2)
			if err != nil {
				errs <- fmt.Errorf("clean probe: %w", err)
				return
			}
			if clean == nil || !*clean {
				errs <- fmt.Errorf("clean PR reported mergeable = %v, want true", clean)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestConcurrentMergesOntoOneBase is the test that actually pins per-repo git
// serialisation.
//
// Concurrent read-only trial merges turn out to be fairly benign — each runs in
// its own throwaway worktree and touches no ref. The genuine hazard is the
// read-modify-write MergePR performs: it checks the base branch out, merges
// onto it, and moves the ref. Two of those interleaved both fork from the same
// tip and both move the ref, so the first merge is silently lost — a wrong
// answer, not a crash, and one no race detector would report. The merge train
// merges several PRs onto one base, so this is a shape the engine really
// produces.
func TestConcurrentMergesOntoOneBase(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo(repoName).
		SeedCommit(repoName, "main", map[string]string{"base.txt": "base\n"}, "base")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding base: %v", err)
	}

	// Three PRs off main, each adding a distinct file so no merge conflicts.
	const prCount = 3
	for i := 1; i <= prCount; i++ {
		branch := fmt.Sprintf("feature-%d", i)
		s.SeedCommitFrom(repoName, branch, "main", map[string]string{
			fmt.Sprintf("feature-%d.txt", i): "content\n",
		}, fmt.Sprintf("feature %d", i)).
			SeedPR(repoName, PRSeed{Number: i, Head: branch, Base: "main"})
	}
	if err := s.Err(); err != nil {
		t.Fatalf("seeding PRs: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, prCount)
	for i := 1; i <= prCount; i++ {
		wg.Add(1)
		go func(num int) {
			defer wg.Done()
			if err := s.MergePR("acme", "widgets", num); err != nil {
				errs <- fmt.Errorf("MergePR(%d): %w", num, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Every merge must be present in main's tree. A lost update shows up here
	// as a missing file, even though every MergePR call returned nil.
	r, err := s.repoByKey(repoName)
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}
	r.gitMu.Lock()
	tree, gitErr := runGit(r.bareDir, "ls-tree", "--name-only", "refs/heads/main")
	r.gitMu.Unlock()
	if gitErr != nil {
		t.Fatalf("ls-tree: %v", gitErr)
	}
	for i := 1; i <= prCount; i++ {
		want := fmt.Sprintf("feature-%d.txt", i)
		if !strings.Contains(tree, want) {
			t.Errorf("main is missing %s after a concurrent merge; tree = %q", want, tree)
		}
	}

	for i := 1; i <= prCount; i++ {
		merged, err := s.FetchPRMerged("acme", "widgets", i)
		if err != nil {
			t.Fatalf("FetchPRMerged(%d): %v", i, err)
		}
		if !merged {
			t.Errorf("PR %d does not report merged", i)
		}
	}
}

// TestConcurrentReviewMutations covers the review surfaces, which the review
// gate reads and writes from several code paths in one poll pass.
func TestConcurrentReviewMutations(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo(repoName).
		SeedCommit(repoName, headBranch, map[string]string{"a.go": "package a\n"}, "work").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	reviewers := []string{"alice", "bob", "carol", "dependabot"}
	for _, login := range reviewers {
		wg.Add(1)
		go func(login string) {
			defer wg.Done()
			if err := s.AddReviewRequest("acme", "widgets", 42, []string{login}); err != nil {
				errs <- fmt.Errorf("AddReviewRequest(%s): %w", login, err)
			}
		}(login)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.FetchPRReviewRequests("acme", "widgets", 42); err != nil {
				errs <- fmt.Errorf("FetchPRReviewRequests: %w", err)
			}
			if _, err := s.FetchPRReviews("acme", "widgets", 42); err != nil {
				errs <- fmt.Errorf("FetchPRReviews: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	requests, err := s.FetchPRReviewRequests("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if len(requests) != len(reviewers) {
		t.Fatalf("%d review requests recorded, want %d — a concurrent add was lost", len(requests), len(reviewers))
	}
	for _, req := range requests {
		if req.Login == "dependabot" && !req.IsBot {
			t.Error("dependabot was not classified as a bot")
		}
	}
}

// TestConcurrentSeedRepoDoesNotClobber drives the window SeedRepo's re-check
// closes. The "already seeded" guard runs under mu, mu is then released for the
// git init, and mu is retaken to publish — so concurrent callers for one key
// all clear the guard before any of them publishes.
//
// Without the re-check the last writer replaces a live repoState, swapping out
// the repo's gitMu while earlier callers still hold the old one: two mutexes
// guarding one bare directory, which silently voids the per-repo git
// serialisation every other test depends on. A start barrier makes all the
// callers reach the guard together, so the window is entered rather than
// hoped for.
func TestConcurrentSeedRepoDoesNotClobber(t *testing.T) {
	s, _ := newSim(t)

	const callers = 4
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			s.SeedRepo("acme/widgets")
		}()
	}
	start.Done()
	done.Wait()

	// The error must be the refusal itself, not a git failure. That is the
	// assertion that distinguishes a held lock from an unheld one: with the
	// sequence serialised, the losers are turned away before they run git at
	// all, so no caller can ever observe a git error. Without it, they all
	// clear the check together and collide inside initBare against one
	// directory — asserting merely that some error occurred would be satisfied
	// by that collision and would prove nothing.
	err := s.Err()
	if err == nil {
		t.Fatalf("%d concurrent SeedRepo calls for one key all succeeded; want every one after the first to be refused", callers)
	}
	if !strings.Contains(err.Error(), "already seeded") {
		t.Fatalf("concurrent SeedRepo failed with %v; want the \"already seeded\" refusal — a git error here means the callers raced inside initBare instead of being serialised", err)
	}

	// Exactly one repo, and it must be usable — a clobbered state would leave
	// the map pointing at a repoState whose gitMu nobody else is holding.
	r, err := s.repoByKey("acme/widgets")
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}
	r.gitMu.Lock()
	ok := r.branchExists("main")
	r.gitMu.Unlock()
	if !ok {
		t.Fatal("the surviving repoState does not have its default branch; seeding raced")
	}
}

// TestConcurrentSeedPRDoesNotClobber drives the window SeedPR{Merged: true}'s
// git-side merge opens. numberTaken is checked under mu, mu is then released
// for the merge (mu and gitMu must never be held at once — see git.go), and
// mu is retaken to reserve the number and publish — so, without numberMu,
// concurrent callers for one explicit number could all clear the check before
// any of them publishes.
//
// Without numberMu, the last writer to reach the final locked section
// replaces r.prs[num] outright, discarding an earlier caller's PR with no
// error from either call — silent corruption, not a race the tool would even
// flag, since both writes are individually mu-guarded. A start barrier makes
// all the callers reach the check together, so the window is entered rather
// than hoped for.
//
// A single batch of callers is not reliable on its own: whether the window
// is actually entered depends on real scheduling (how far the goroutines get
// through their two short gitMu sections before the first one's git
// subprocess call finishes and publishes), and empirically that lands
// somewhere around a 70% catch rate per batch — a mutation sweep run that
// happens to land in the unlucky 30% would report a real regression as
// vacuous. Running several independent rounds, each against its own fresh
// Sim (so one round's expected "already exists" refusal never sticks around
// as the sim's terminal error and masks the next round's seeding), drives
// the miss probability down geometrically (0.3^rounds) without changing what
// any single round asserts — a genuine numberMu removal must eventually be
// caught, while correct code (which serialises deterministically every
// round, not just probabilistically) never fails regardless of round count.
func TestConcurrentSeedPRDoesNotClobber(t *testing.T) {
	const (
		repo    = "acme/widgets"
		callers = 4
		prNum   = 42
		rounds  = 10
	)

	for round := 0; round < rounds; round++ {
		s, _ := newSim(t)
		s.SeedRepo(repo).SeedCommit(repo, "main", map[string]string{"base.txt": "base\n"}, "base")
		heads := make([]string, callers)
		for i := 0; i < callers; i++ {
			heads[i] = fmt.Sprintf("feature-%d", i)
			s.SeedCommit(repo, heads[i], map[string]string{fmt.Sprintf("f%d.txt", i): "feature\n"}, "feature")
		}
		if err := s.Err(); err != nil {
			t.Fatalf("round %d: seeding: %v", round, err)
		}

		var start, done sync.WaitGroup
		start.Add(1)
		for i := 0; i < callers; i++ {
			done.Add(1)
			go func(head string) {
				defer done.Done()
				start.Wait()
				s.SeedPR(repo, PRSeed{Number: prNum, Head: head, Base: "main", Merged: true, Title: "concurrent"})
			}(heads[i])
		}
		start.Done()
		done.Wait()

		// Exactly one caller can win the explicit number; every other must be
		// turned away by the numberTaken refusal, not a git failure (a git
		// error would mean two callers both cleared the check and raced
		// inside tryMerge instead of being serialised) and not silence (a
		// clobber leaves s.Err() nil, since neither call fails).
		err := s.Err()
		if err == nil {
			t.Fatalf("round %d: %d concurrent SeedPR calls for one explicit number all succeeded; want every one after the first to be refused", round, callers)
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("round %d: concurrent SeedPR failed with %v; want the \"already exists\" refusal — anything else means the callers raced instead of being serialised", round, err)
		}

		// The surviving record must be exactly one whole PR — a real merge
		// commit on main, contributed by exactly one of the candidate heads —
		// not a half-published clobber of two callers' work.
		r, rerr := s.repoByKey(repo)
		if rerr != nil {
			t.Fatalf("round %d: repoByKey: %v", round, rerr)
		}
		s.mu.Lock()
		pr, ok := r.prs[prNum]
		s.mu.Unlock()
		if !ok {
			t.Fatalf("round %d: PR %d does not exist after seeding; want exactly one surviving record", round, prNum)
		}
		found := false
		for _, h := range heads {
			if pr.head == h {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("round %d: surviving PR %d has head %q, not one of the seeded candidates %v", round, prNum, pr.head, heads)
		}
		if !pr.merged {
			t.Fatalf("round %d: surviving PR %d is not marked merged", round, prNum)
		}
	}
}

// TestConcurrentSeedIssueAndSeedPRDoNotClobberNumbers drives numberMu against
// two different numbering APIs racing on one repo, not two calls to the same
// one. SeedPR{Merged: true} releases mu for its git-side merge while holding
// only a peeked, not-yet-reserved, auto-assigned candidate — a window
// SeedIssue's own allocNumber (atomic under mu alone, with no release in the
// middle) could otherwise slip through, claiming the exact number SeedPR
// peeked and silently violating the shared issue-and-PR number space this
// package is built around (numberTaken, node IDs, AddComment's shared REST
// endpoint).
//
// SeedIssue's own critical section is memory-only and finishes in
// nanoseconds, while SeedPR's peek is only reachable after a real git
// subprocess call (the branch-existence check) that takes orders of
// magnitude longer — so a single SeedIssue call racing a single SeedPR call
// almost always finishes (and claims its number) before SeedPR ever reaches
// its peek at all, never entering the vulnerable window by sheer timing
// rather than by being correctly excluded from it. Proving numberMu is what
// keeps the window shut therefore needs *repeated* SeedIssue attempts spread
// across the *entire* duration several real merges are in flight, so at
// least one attempt is very likely to land inside a release window rather
// than merely before or after every one of them.
//
// Unlike TestConcurrentSeedPRDoesNotClobber, every call here is expected to
// succeed — an auto-assigned number is never asked for explicitly, so two
// auto-assigning calls have no reason to be refused against each other. The
// failure mode under test is silent (the same number handed to an issue and
// a PR), not loud, so the assertion is on the resulting state rather than on
// s.Err().
func TestConcurrentSeedIssueAndSeedPRDoNotClobberNumbers(t *testing.T) {
	s, _ := newSim(t)

	const (
		repo      = "acme/widgets"
		prCallers = 5 // concurrent SeedPR{Merged: true} calls, each a real merge
		hammers   = 4 // goroutines that repeatedly auto-assign a SeedIssue
	)
	s.SeedRepo(repo).SeedCommit(repo, "main", map[string]string{"base.txt": "base\n"}, "base")
	heads := make([]string, prCallers)
	for i := 0; i < prCallers; i++ {
		heads[i] = fmt.Sprintf("feature-%d", i)
		s.SeedCommit(repo, heads[i], map[string]string{fmt.Sprintf("f%d.txt", i): "feature\n"}, "feature")
	}
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	stop := make(chan struct{})
	var start, prDone, hammerDone sync.WaitGroup
	start.Add(1)

	for i := 0; i < prCallers; i++ {
		prDone.Add(1)
		go func(head string) {
			defer prDone.Done()
			start.Wait()
			s.SeedPR(repo, PRSeed{Head: head, Base: "main", Merged: true, Title: "concurrent pr"})
		}(heads[i])
	}
	// Each hammer keeps auto-assigning issues for as long as any SeedPR call
	// above is still in flight, so the attempts span every merge's release
	// window rather than racing it just once.
	for i := 0; i < hammers; i++ {
		hammerDone.Add(1)
		go func() {
			defer hammerDone.Done()
			start.Wait()
			for {
				select {
				case <-stop:
					return
				default:
					s.SeedIssue(repo, IssueSeed{Title: "concurrent issue"})
				}
			}
		}()
	}
	start.Done()
	prDone.Wait()
	close(stop)
	hammerDone.Wait()

	// Every call should have succeeded: none of them asked for a specific
	// number, so there is nothing for any of them to be refused against.
	if err := s.Err(); err != nil {
		t.Fatalf("concurrent auto-assigning SeedIssue/SeedPR calls: %v", err)
	}

	r, rerr := s.repoByKey(repo)
	if rerr != nil {
		t.Fatalf("repoByKey: %v", rerr)
	}
	s.mu.Lock()
	var overlap []int
	for num := range r.issues {
		if _, ok := r.prs[num]; ok {
			overlap = append(overlap, num)
		}
	}
	issueCount, prCount := len(r.issues), len(r.prs)
	s.mu.Unlock()

	if len(overlap) != 0 {
		t.Fatalf("number(s) %v claimed by both an issue and a PR — the shared number space was violated", overlap)
	}
	// Every hammer attempt succeeds (auto-assign never refuses), so the issue
	// count is however many landed before stop fired — only the PR count and
	// the overlap check above are deterministic.
	if prCount != prCallers {
		t.Fatalf("want %d PRs (one per caller, none clobbered by a concurrent SeedIssue), got %d", prCallers, prCount)
	}
	if issueCount == 0 {
		t.Fatal("no hammer SeedIssue call landed at all; the test did not exercise any overlap")
	}
}
