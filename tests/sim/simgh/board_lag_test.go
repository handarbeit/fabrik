package simgh

import "testing"

// TestLagBoardStatus_AffectsBulkReadsOnly pins the seam's contract (#1871): the
// board-scoped bulk reads report the held view, while the single-node reads keep
// reporting the truth, and ClearBoardLag restores the bulk reads.
func TestLagBoardStatus_AffectsBulkReadsOnly(t *testing.T) {
	s, _ := seedBasicBoard(t)
	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil || len(board.Items) != 1 {
		t.Fatalf("FetchProjectBoard: %v, %d item(s)", err, len(board.Items))
	}
	itemID := board.Items[0].ItemID

	s.LagBoardStatus(itemID, "Backlog", []string{"held-label"})

	board, err = s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if got := board.Items[0]; got.Status != "Backlog" || len(got.Labels) != 1 || got.Labels[0] != "held-label" {
		t.Errorf("board read should report the held view, got status=%q labels=%v", got.Status, got.Labels)
	}
	batch, err := s.FetchProjectItemStatusBatch(board.ProjectID)
	if err != nil || batch[itemID] != "Backlog" {
		t.Errorf("batch status read should report the held status, got %q (err %v)", batch[itemID], err)
	}
	if got, err := s.FetchProjectItemStatus(itemID); err != nil || got != "Implement" {
		t.Errorf("FetchProjectItemStatus must keep reporting the truth, got %q (err %v)", got, err)
	}
	if _, status, err := s.LookupIssueProjectItem(board.ProjectID, "acme/widgets", 7); err != nil || status != "Implement" {
		t.Errorf("LookupIssueProjectItem must keep reporting the truth, got %q (err %v)", status, err)
	}

	s.ClearBoardLag(itemID)
	board, _ = s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if got := board.Items[0].Status; got != "Implement" {
		t.Errorf("after ClearBoardLag the board read should be truthful, got %q", got)
	}
}
