package simgh

import (
	"fmt"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// AddBlockedByIssue records that issueNodeID is blocked by blockerNodeID.
//
// Both arguments are node IDs, matching the GraphQL mutation production calls.
// Note what a fake at this layer cannot check: the v0.0.66 regression was a
// wrong mutation *name* inside the query document, and every interface-level
// implementation of this method — including this one — is green against it.
// See ../README.md for what closes that gap.
func (s *Sim) AddBlockedByIssue(issueNodeID, blockerNodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	blockerRepo, blockerNum, err := parseIssueNodeID(blockerNodeID)
	if err != nil {
		return fmt.Errorf("simgh: AddBlockedByIssue blocker: %w", err)
	}
	targetRepo, targetNum, err := parseIssueNodeID(issueNodeID)
	if err != nil {
		return fmt.Errorf("simgh: AddBlockedByIssue target: %w", err)
	}

	r, ok := s.repos[targetRepo]
	if !ok {
		return fmt.Errorf("simgh: repo %s not seeded", targetRepo)
	}
	iss, ok := r.issues[targetNum]
	if !ok {
		return fmt.Errorf("simgh: issue %s#%d not found", targetRepo, targetNum)
	}

	// The blocker must exist too. GitHub's mutation fails on a node ID that
	// resolves to nothing, and a dangling blocker is worse here than a plain
	// no-op: resolveDependenciesLocked reports an unresolvable blocker as
	// State "OPEN", so the dependent issue would read as permanently blocked
	// with no diagnostic anywhere — a wrong ID silently accepted, which is the
	// bug class this layer exists to catch.
	br, ok := s.repos[blockerRepo]
	if !ok {
		return fmt.Errorf("simgh: blocker repo %s not seeded", blockerRepo)
	}
	if _, ok := br.issues[blockerNum]; !ok {
		return fmt.Errorf("simgh: blocker issue %s#%d not found", blockerRepo, blockerNum)
	}

	// GitHub silently ignores a duplicate dependency rather than erroring.
	for _, dep := range iss.blockedBy {
		depRepo := dep.Repo
		if depRepo == "" {
			depRepo = targetRepo
		}
		if dep.Number == blockerNum && depRepo == blockerRepo {
			return nil
		}
	}

	repoField := ""
	if blockerRepo != targetRepo {
		repoField = blockerRepo
	}
	iss.blockedBy = append(iss.blockedBy, gh.Dependency{Number: blockerNum, Repo: repoField})
	iss.updatedAt = s.now()
	return nil
}

// parseIssueNodeID splits a synthesised issue node ID ("issue:owner/repo#N")
// back into its parts. Sim node IDs are readable rather than opaque because
// production never parses a node ID it receives — it only round-trips them —
// so the format is an implementation convenience, not a modelled behaviour.
func parseIssueNodeID(nodeID string) (ownerRepo string, number int, err error) {
	return parseNodeID("issue:", nodeID)
}

// parsePRNodeID splits a synthesised PR node ID ("pr:owner/repo#N").
func parsePRNodeID(nodeID string) (ownerRepo string, number int, err error) {
	return parseNodeID("pr:", nodeID)
}

func parseNodeID(prefix, nodeID string) (string, int, error) {
	rest, ok := strings.CutPrefix(nodeID, prefix)
	if !ok {
		return "", 0, fmt.Errorf("simgh: node ID %q does not have prefix %q", nodeID, prefix)
	}
	repoPart, numPart, ok := strings.Cut(rest, "#")
	if !ok {
		return "", 0, fmt.Errorf("simgh: malformed node ID %q", nodeID)
	}
	n, err := strconv.Atoi(numPart)
	if err != nil {
		return "", 0, fmt.Errorf("simgh: malformed number in node ID %q", nodeID)
	}
	return repoPart, n, nil
}
