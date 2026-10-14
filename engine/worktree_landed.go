package engine

import (
	gh "github.com/handarbeit/fabrik/github"
)

// worktreeAdoptionOptions builds the EnsureWorktree options that tell the
// WorktreeManager what the engine already knows about the item's PRs (#1917,
// ADR 1917). Both options are lazy: EnsureWorktree invokes them only when an
// origin issue branch is about to be adopted, so the common path (fresh fork,
// or a branch already present locally) pays for nothing. They run under the
// WorktreeManager's mutex, so every read is bounded by the client's own
// timeouts and fails soft — an error means "unknown", never a verdict.
//
//   - Landed heads (R2): the linked PR's head when the cached state says it is
//     merged, else when one live read of the linked PR shows it merged (the
//     cache only learns Merged from a rich PR fetch, so it is blank after a
//     restart), and the head of the item's fabrik:credited-pr:<N> PR when a
//     live read shows it merged. A failed read means unknown, which falls back
//     to the ancestry check (R1): a missed skip, never data loss.
//   - PR base (R3): the base of the item's open linked PR, for the warning only.
func (e *Engine) worktreeAdoptionOptions(item gh.ProjectItem) []EnsureWorktreeOption {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoKey := itemOwnerRepoString(item, e.defaultRepo())

	cachedLinked := func() (number int, state string, merged bool, head string) {
		if e.store == nil {
			return 0, "", false, ""
		}
		snap, ok := e.store.Peek(repoKey, item.Number)
		if !ok {
			return 0, "", false, ""
		}
		st := snap.State()
		if st.LinkedPR == nil {
			return 0, "", false, ""
		}
		return st.LinkedPR.Number, st.LinkedPR.State, st.LinkedPR.Merged, st.LinkedPR.HeadSHA
	}

	// linkedLive reads the item's linked PR live, at most once per option set
	// (both callbacks share it). The cache's Merged/State come only from a rich
	// PRDetails fetch, so after a restart they are unset even for a merged PR;
	// the cached head alone cannot say "merged". Nil means unknown.
	var (
		linkedRead bool
		linkedPR   *gh.PRDetails
	)
	linkedLive := func(number int) *gh.PRDetails {
		if linkedRead {
			return linkedPR
		}
		linkedRead = true
		if number == 0 {
			return nil
		}
		pr, err := e.client.FetchPRDetails(owner, repo, number)
		if err != nil {
			e.logf(item.Number, "worktree", "warn: could not read linked PR #%d (%v) — treating its state as unknown\n", number, err)
			return nil
		}
		linkedPR = pr
		return linkedPR
	}

	landedHeads := func() []string {
		var heads []string
		linkedNum, _, merged, head := cachedLinked()
		if linkedNum == 0 {
			linkedNum = item.LinkedPRNumber
		}
		if merged && head != "" {
			heads = append(heads, head)
		} else if pr := linkedLive(linkedNum); pr != nil && pr.Merged && pr.HeadSHA != "" {
			heads = append(heads, pr.HeadSHA)
		}
		if credited, ok := creditedPRFromLabel(item); ok && credited != linkedNum {
			pr, err := e.client.FetchPRDetails(owner, repo, credited)
			switch {
			case err != nil:
				e.logf(item.Number, "worktree", "warn: could not read credited PR #%d (%v) — treating its head as unknown\n", credited, err)
			case pr != nil && pr.Merged && pr.HeadSHA != "":
				heads = append(heads, pr.HeadSHA)
			}
		}
		return heads
	}

	prBase := func() (string, bool) {
		number, state, merged, _ := cachedLinked()
		if number == 0 {
			number = item.LinkedPRNumber
		}
		if number == 0 || merged || state == "closed" {
			return "", false
		}
		pr := linkedLive(number)
		if pr == nil || pr.State != "open" || pr.BaseRef == "" {
			return "", false
		}
		return pr.BaseRef, true
	}

	return []EnsureWorktreeOption{WithLandedPRHeads(landedHeads), WithPRBaseProbe(prBase)}
}
