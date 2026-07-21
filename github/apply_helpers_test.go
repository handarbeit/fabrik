package github

import "testing"

// These tests exercise the apply* helpers extracted from FetchItemDetails
// directly, in isolation from the GraphQL request/response plumbing that the
// FetchItemDetails_* tests in fetch_details_test.go already cover end-to-end.

func TestApplyLabels_ResetsAndPopulates(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused.invalid")
	item := &ProjectItem{ID: "I_1", Number: 1, Labels: []string{"stale-label"}}
	node := &fetchItemDetailsNode{}
	node.Labels.Nodes = []struct {
		Name string `json:"name"`
	}{{Name: "bug"}, {Name: "priority:high"}}

	if err := c.applyLabels(item, node); err != nil {
		t.Fatalf("applyLabels: %v", err)
	}
	if len(item.Labels) != 2 || item.Labels[0] != "bug" || item.Labels[1] != "priority:high" {
		t.Fatalf("item.Labels = %v", item.Labels)
	}
}

func TestApplyLabels_NoLabelsClearsStale(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused.invalid")
	item := &ProjectItem{ID: "I_1", Number: 1, Labels: []string{"stale-label"}}
	node := &fetchItemDetailsNode{}

	if err := c.applyLabels(item, node); err != nil {
		t.Fatalf("applyLabels: %v", err)
	}
	if item.Labels != nil {
		t.Fatalf("expected nil labels, got %v", item.Labels)
	}
}

func TestApplyBlockedBy_NilNode(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused.invalid")
	item := &ProjectItem{ID: "I_1", Number: 1, BlockedBy: []Dependency{{Number: 99}}}
	node := &fetchItemDetailsNode{}

	c.applyBlockedBy(item, node)
	if item.BlockedBy != nil {
		t.Fatalf("expected nil BlockedBy for PR item, got %v", item.BlockedBy)
	}
}

func TestApplyBlockedBy_MapsDependencies(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused.invalid")
	item := &ProjectItem{ID: "I_1", Number: 1}
	node := &fetchItemDetailsNode{}
	node.BlockedBy = &struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []blockedByNode `json:"nodes"`
	}{}
	node.BlockedBy.Nodes = []blockedByNode{
		{Number: 42, State: "OPEN"},
	}
	node.BlockedBy.Nodes[0].Repository = &struct {
		NameWithOwner string `json:"nameWithOwner"`
	}{NameWithOwner: "owner/repo"}

	c.applyBlockedBy(item, node)
	if len(item.BlockedBy) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(item.BlockedBy))
	}
	if item.BlockedBy[0].Number != 42 || item.BlockedBy[0].State != "OPEN" || item.BlockedBy[0].Repo != "owner/repo" {
		t.Errorf("BlockedBy[0] = %+v", item.BlockedBy[0])
	}
}

func TestApplyComments_ResetsAndPopulates(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused.invalid")
	item := &ProjectItem{ID: "I_1", Number: 1, Comments: []Comment{{Body: "stale"}}}
	node := &fetchItemDetailsNode{}
	node.Comments.Nodes = []commentNodeData{
		{ID: "C_1", Body: "fresh comment"},
	}

	if err := c.applyComments(item, node); err != nil {
		t.Fatalf("applyComments: %v", err)
	}
	if len(item.Comments) != 1 || item.Comments[0].Body != "fresh comment" {
		t.Fatalf("item.Comments = %+v", item.Comments)
	}
	if item.Comments[0].FromPR != 0 {
		t.Errorf("expected FromPR=0 for item's own comment, got %d", item.Comments[0].FromPR)
	}
}

func TestApplyLinkedPRs_ResetsFieldsWhenNoLinkedPRs(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused.invalid")
	item := &ProjectItem{
		ID:                     "I_1",
		Number:                 1,
		LinkedPRNumber:         7,
		LinkedPRReviewRequests: []ReviewRequest{{Login: "stale"}},
		LinkedPRReviews:        []PRReview{{Author: "stale"}},
	}
	node := &fetchItemDetailsNode{}

	if err := c.applyLinkedPRs(item, node); err != nil {
		t.Fatalf("applyLinkedPRs: %v", err)
	}
	if item.LinkedPRNumber != 0 || item.LinkedPRReviewRequests != nil || item.LinkedPRReviews != nil {
		t.Errorf("expected reset LinkedPR* fields, got Number=%d ReviewRequests=%v Reviews=%v",
			item.LinkedPRNumber, item.LinkedPRReviewRequests, item.LinkedPRReviews)
	}
}

func TestApplyLinkedPRs_MapsFirstPRAndReviews(t *testing.T) {
	c := NewClientWithBaseURL("token", "http://unused.invalid")
	item := &ProjectItem{ID: "I_1", Number: 1}
	node := &fetchItemDetailsNode{}
	node.LinkedPRs = &struct {
		Nodes []struct {
			ID                  string               `json:"id"`
			Number              int                  `json:"number"`
			HeadRefOid          string               `json:"headRefOid"`
			IsMergeQueueEnabled bool                 `json:"isMergeQueueEnabled"`
			IsInMergeQueue      bool                 `json:"isInMergeQueue"`
			MergeQueueEntry     *mergeQueueEntryData `json:"mergeQueueEntry"`
			Comments            struct {
				Nodes    []commentNodeData `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"comments"`
			ReviewRequests struct {
				Nodes []struct {
					RequestedReviewer struct {
						Typename string `json:"__typename"`
						Login    string `json:"login"`
					} `json:"requestedReviewer"`
				} `json:"nodes"`
			} `json:"reviewRequests"`
			LatestReviews struct {
				Nodes []struct {
					DatabaseID int `json:"databaseId"`
					Author     *struct {
						Login string `json:"login"`
					} `json:"author"`
					State string `json:"state"`
					Body  string `json:"body"`
				} `json:"nodes"`
			} `json:"latestReviews"`
			ReviewThreads struct {
				Nodes []struct {
					ID           string  `json:"id"`
					IsResolved   bool    `json:"isResolved"`
					Path         string  `json:"path"`
					Line         *int    `json:"line"`
					OriginalLine *int    `json:"originalLine"`
					DiffSide     *string `json:"diffSide"`
					Comments     struct {
						Nodes []commentNodeData `json:"nodes"`
					} `json:"comments"`
				} `json:"nodes"`
			} `json:"reviewThreads"`
		} `json:"nodes"`
	}{}
	pr := struct {
		ID                  string               `json:"id"`
		Number              int                  `json:"number"`
		HeadRefOid          string               `json:"headRefOid"`
		IsMergeQueueEnabled bool                 `json:"isMergeQueueEnabled"`
		IsInMergeQueue      bool                 `json:"isInMergeQueue"`
		MergeQueueEntry     *mergeQueueEntryData `json:"mergeQueueEntry"`
		Comments            struct {
			Nodes    []commentNodeData `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"comments"`
		ReviewRequests struct {
			Nodes []struct {
				RequestedReviewer struct {
					Typename string `json:"__typename"`
					Login    string `json:"login"`
				} `json:"requestedReviewer"`
			} `json:"nodes"`
		} `json:"reviewRequests"`
		LatestReviews struct {
			Nodes []struct {
				DatabaseID int `json:"databaseId"`
				Author     *struct {
					Login string `json:"login"`
				} `json:"author"`
				State string `json:"state"`
				Body  string `json:"body"`
			} `json:"nodes"`
		} `json:"latestReviews"`
		ReviewThreads struct {
			Nodes []struct {
				ID           string  `json:"id"`
				IsResolved   bool    `json:"isResolved"`
				Path         string  `json:"path"`
				Line         *int    `json:"line"`
				OriginalLine *int    `json:"originalLine"`
				DiffSide     *string `json:"diffSide"`
				Comments     struct {
					Nodes []commentNodeData `json:"nodes"`
				} `json:"comments"`
			} `json:"nodes"`
		} `json:"reviewThreads"`
	}{
		ID:         "PR_1",
		Number:     55,
		HeadRefOid: "deadbeef",
	}
	pr.ReviewRequests.Nodes = []struct {
		RequestedReviewer struct {
			Typename string `json:"__typename"`
			Login    string `json:"login"`
		} `json:"requestedReviewer"`
	}{
		{RequestedReviewer: struct {
			Typename string `json:"__typename"`
			Login    string `json:"login"`
		}{Typename: "User", Login: "carol"}},
	}
	pr.LatestReviews.Nodes = []struct {
		DatabaseID int `json:"databaseId"`
		Author     *struct {
			Login string `json:"login"`
		} `json:"author"`
		State string `json:"state"`
		Body  string `json:"body"`
	}{
		{DatabaseID: 1, Author: &struct {
			Login string `json:"login"`
		}{Login: "carol"}, State: "APPROVED", Body: "lgtm"},
	}
	node.LinkedPRs.Nodes = []struct {
		ID                  string               `json:"id"`
		Number              int                  `json:"number"`
		HeadRefOid          string               `json:"headRefOid"`
		IsMergeQueueEnabled bool                 `json:"isMergeQueueEnabled"`
		IsInMergeQueue      bool                 `json:"isInMergeQueue"`
		MergeQueueEntry     *mergeQueueEntryData `json:"mergeQueueEntry"`
		Comments            struct {
			Nodes    []commentNodeData `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"comments"`
		ReviewRequests struct {
			Nodes []struct {
				RequestedReviewer struct {
					Typename string `json:"__typename"`
					Login    string `json:"login"`
				} `json:"requestedReviewer"`
			} `json:"nodes"`
		} `json:"reviewRequests"`
		LatestReviews struct {
			Nodes []struct {
				DatabaseID int `json:"databaseId"`
				Author     *struct {
					Login string `json:"login"`
				} `json:"author"`
				State string `json:"state"`
				Body  string `json:"body"`
			} `json:"nodes"`
		} `json:"latestReviews"`
		ReviewThreads struct {
			Nodes []struct {
				ID           string  `json:"id"`
				IsResolved   bool    `json:"isResolved"`
				Path         string  `json:"path"`
				Line         *int    `json:"line"`
				OriginalLine *int    `json:"originalLine"`
				DiffSide     *string `json:"diffSide"`
				Comments     struct {
					Nodes []commentNodeData `json:"nodes"`
				} `json:"comments"`
			} `json:"nodes"`
		} `json:"reviewThreads"`
	}{pr}

	if err := c.applyLinkedPRs(item, node); err != nil {
		t.Fatalf("applyLinkedPRs: %v", err)
	}
	if item.LinkedPRNumber != 55 || item.LinkedPRHeadSHA != "deadbeef" {
		t.Errorf("LinkedPRNumber/HeadSHA = %d/%s", item.LinkedPRNumber, item.LinkedPRHeadSHA)
	}
	if len(item.LinkedPRReviewRequests) != 1 || item.LinkedPRReviewRequests[0].Login != "carol" {
		t.Errorf("LinkedPRReviewRequests = %+v", item.LinkedPRReviewRequests)
	}
	if len(item.LinkedPRReviews) != 1 || item.LinkedPRReviews[0].Author != "carol" {
		t.Errorf("LinkedPRReviews = %+v", item.LinkedPRReviews)
	}
}
