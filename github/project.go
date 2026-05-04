package github

import (
	"fmt"
	"time"
)

// commentNodeData holds the raw data for a comment returned from the API.
type commentNodeData struct {
	ID         string `json:"id"`
	DatabaseID int    `json:"databaseId"`
	Author     *struct {
		Login string `json:"login"`
	} `json:"author"`
	Body           string `json:"body"`
	CreatedAt      string `json:"createdAt"`
	ReactionGroups []struct {
		Content  string `json:"content"`
		Reactors struct {
			TotalCount int `json:"totalCount"`
		} `json:"reactors"`
	} `json:"reactionGroups"`
	// Location fields — populated only for PR review thread comments.
	// Line and OriginalLine are *int because the GitHub API returns null for
	// file-level comments and outdated comments.
	DiffHunk     string `json:"diffHunk"`
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"originalLine"`
}

// itemNode mirrors one element of items.nodes in the FetchProjectBoard query.
// This is the shallow version — only fields needed for pre-filtering are
// included. Body, URL, Author, Assignees, and BlockedBy are fetched in the
// deep phase via FetchItemDetails to reduce GraphQL rate limit cost.
type itemNode struct {
	ID               string `json:"id"`
	UpdatedAt        string `json:"updatedAt"` // Project item updatedAt (bumped by column moves)
	FieldValueByName *struct {
		Name string `json:"name"`
	} `json:"fieldValueByName"`
	Content struct {
		Typename   string `json:"__typename"`
		ID         string `json:"id"`
		Number     int    `json:"number"`
		Title      string `json:"title"`
		State      string `json:"state"`
		UpdatedAt  string `json:"updatedAt"`
		Repository *struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
		Labels struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
		LinkedPRs *struct {
			Nodes []struct {
				UpdatedAt string `json:"updatedAt"`
				Number    int    `json:"number"`
			} `json:"nodes"`
		} `json:"closedByPullRequestsReferences"`
	} `json:"content"`
}

// blockedByNode holds raw data for a single "blocked by" relationship from the API.
type blockedByNode struct {
	Number     int    `json:"number"`
	State      string `json:"state"`
	Repository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// FetchProjectBoard pulls the project board with shallow item data (no comments
// or linked PRs). Use FetchItemDetails to populate comments for specific items.
// This two-phase approach dramatically reduces GraphQL rate limit cost.
//
// When ownerType is non-empty ("user" or "organization"), the board is fetched
// directly using that type, skipping the try-org-then-user fallback. When
// ownerType is empty, the original fallback behavior is preserved.
func (c *Client) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*ProjectBoard, error) {
	if ownerType != "" {
		return c.fetchProjectBoard(owner, repo, projectNum, ownerType)
	}
	// Try organization first, then user. GitHub Projects v2 live at the
	// user or org level. We can't know which without checking, so we try
	// org first then fall back to user.
	board, err := c.fetchProjectBoard(owner, repo, projectNum, "organization")
	if err != nil {
		board, err = c.fetchProjectBoard(owner, repo, projectNum, "user")
	}
	return board, err
}

// projectBoardFetchAttempts is the maximum number of times fetchProjectBoard
// will retry the entire pagination if GitHub returns a response that disagrees
// with itself: fewer raw items than totalCount claims, or zero items at all.
//
// Empirically, the GitHub Projects v2 GraphQL endpoint can return inconsistent
// responses during indexer degradation — the same query, hit back-to-back
// within a single second, returned 100 items or 0 items at random. The 0-item
// responses came back with totalCount=0 and HTTP 200, indistinguishable at the
// API level from a genuinely empty project. Without retry, Fabrik silently
// goes idle every poll until GitHub recovers. With retry, transient bad
// indexer hits get masked.
const projectBoardFetchAttempts = 3

// projectBoardFetchBackoff returns the sleep before retry attempt n (1-indexed):
// 0 for the first attempt, then linear (1s, 2s, ...).
func projectBoardFetchBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	return time.Duration(attempt-1) * time.Second
}

func (c *Client) fetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*ProjectBoard, error) {
	var lastBoard *ProjectBoard
	for attempt := 1; attempt <= projectBoardFetchAttempts; attempt++ {
		if d := projectBoardFetchBackoff(attempt); d > 0 {
			time.Sleep(d)
		}
		board, rawNodeCount, totalCount, err := c.fetchProjectBoardOnce(owner, repo, projectNum, ownerType)
		if err != nil {
			// Errors return immediately — retry is for empty/inconsistent
			// successes, not transport failures (the caller may want to
			// fall back to a different ownerType on error).
			return nil, err
		}
		lastBoard = board
		// Healthy response: either we collected at least totalCount nodes (the
		// indexer-agrees-with-itself case, using >= because totalCount can
		// shift if items are added/removed between page fetches), or we have
		// nodes at all (handles older/test responses where totalCount may not
		// be present — degraded responses always return both rawNodeCount=0
		// AND totalCount=0 together, so "nodes present" is sufficient).
		if (totalCount > 0 && rawNodeCount >= totalCount) || rawNodeCount > 0 {
			return board, nil
		}
		// Inconsistent response: zero raw nodes AND zero totalCount. Could be
		// a genuinely empty project, or the indexer briefly forgot the project
		// (observed during GitHub Projects degradation). Worth retrying; if
		// every attempt agrees, accept it as genuinely empty.
		if attempt < projectBoardFetchAttempts {
			logf(0, "warn",
				"project board fetch returned %d items, totalCount=%d (attempt %d/%d) — retrying in case of indexer hiccup\n",
				rawNodeCount, totalCount, attempt, projectBoardFetchAttempts)
		}
	}
	return lastBoard, nil
}

// fetchProjectBoardOnce performs one full pagination pass and returns the
// resulting board, the raw GraphQL node count (before Fabrik's drafts/repo
// filtering), and the maximum totalCount observed across pages. Callers
// compare rawNodeCount to totalCount to detect indexer-degraded responses
// and decide whether to retry.
func (c *Client) fetchProjectBoardOnce(owner, repo string, projectNum int, ownerType string) (*ProjectBoard, int, int, error) {
	query := fmt.Sprintf(`
query($owner: String!, $projectNum: Int!, $cursor: String) {
  %s(login: $owner) {
    projectV2(number: $projectNum) {
      id
      title`, ownerType) + `
      items(first: 100, after: $cursor) {
        totalCount
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          updatedAt
          fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue {
              name
            }
          }
          content {
            __typename
            ... on Issue {
              id
              number
              title
              state
              updatedAt
              repository {
                nameWithOwner
              }
              labels(first: 15) {
                nodes {
                  name
                }
              }
              closedByPullRequestsReferences(first: 5) {
                nodes {
                  updatedAt
                  number
                }
              }
            }
            ... on PullRequest {
              id
              number
              title
              updatedAt
              repository {
                nameWithOwner
              }
              labels(first: 15) {
                nodes {
                  name
                }
              }
            }
          }
        }
      }
    }
  }
}`

	var projectID string
	var projectTitle string
	var allNodes []itemNode
	// maxTotalCount tracks the largest totalCount observed across pages.
	// Compared to len(allNodes) post-pagination to detect partial fetches.
	maxTotalCount := 0

	// Paginate over items.
	cursor := ""
	for {
		vars := map[string]interface{}{
			"owner":      owner,
			"projectNum": projectNum,
		}
		if cursor != "" {
			vars["cursor"] = cursor
		}

		var result struct {
			Data map[string]struct {
				ProjectV2 struct {
					ID    string `json:"id"`
					Title string `json:"title"`
					Items struct {
						TotalCount int `json:"totalCount"`
						PageInfo   struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []itemNode `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"data"`
		}

		if err := c.graphqlRequest(query, vars, &result); err != nil {
			return nil, 0, 0, fmt.Errorf("fetching project board: %w", err)
		}

		ownerData, ok := result.Data[ownerType]
		if !ok {
			return nil, 0, 0, fmt.Errorf("fetching project board: no %s data in response", ownerType)
		}
		proj := ownerData.ProjectV2
		if projectID == "" {
			projectID = proj.ID
			projectTitle = proj.Title
		}
		if proj.Items.TotalCount > maxTotalCount {
			maxTotalCount = proj.Items.TotalCount
		}
		allNodes = append(allNodes, proj.Items.Nodes...)

		if !proj.Items.PageInfo.HasNextPage {
			break
		}
		if proj.Items.PageInfo.EndCursor == "" {
			return nil, 0, 0, fmt.Errorf("fetching project board: hasNextPage=true but endCursor is empty")
		}
		cursor = proj.Items.PageInfo.EndCursor
	}

	board := &ProjectBoard{ProjectID: projectID, Title: projectTitle, OwnerType: ownerType}

	for _, node := range allNodes {
		// Skip items whose content was not returned (empty content ID, e.g. draft issues)
		if node.Content.ID == "" {
			continue
		}

		item := ProjectItem{
			ID:       node.Content.ID,
			ItemID:   node.ID,
			Number:   node.Content.Number,
			Title:    node.Content.Title,
			IsPR:     node.Content.Typename == "PullRequest",
			IsClosed: node.Content.Typename != "PullRequest" && node.Content.State == "CLOSED",
		}
		if node.Content.Repository != nil {
			item.Repo = node.Content.Repository.NameWithOwner
		}

		if t, err := parseTime(node.Content.UpdatedAt); err == nil {
			item.UpdatedAt = t
		}
		// Project item updatedAt is bumped by board column moves, which don't
		// affect the issue's own updatedAt. Use whichever is later.
		if t, err := parseTime(node.UpdatedAt); err == nil && t.After(item.UpdatedAt) {
			item.UpdatedAt = t
		}
		// Use the latest updatedAt across the issue, project item, and linked PRs
		// so that comments on a linked PR are detected as changes even though the
		// issue itself doesn't update.
		if node.Content.LinkedPRs != nil {
			for _, pr := range node.Content.LinkedPRs.Nodes {
				if t, err := parseTime(pr.UpdatedAt); err == nil && t.After(item.UpdatedAt) {
					item.UpdatedAt = t
				}
			}
			if len(node.Content.LinkedPRs.Nodes) > 0 {
				item.LinkedPRNumberShallow = node.Content.LinkedPRs.Nodes[0].Number
			}
		}

		if node.FieldValueByName != nil {
			item.Status = node.FieldValueByName.Name
		}

		// Populate minimal label set (first:5) for cleanupClosedIssueLocks on
		// closed items (which are never deep-fetched). Open items receive a full,
		// authoritative label set from FetchItemDetails.
		for _, l := range node.Content.Labels.Nodes {
			item.Labels = append(item.Labels, l.Name)
		}

		board.Items = append(board.Items, item)
	}

	return board, len(allNodes), maxTotalCount, nil
}

// FetchItemDetails populates the Comments, Labels, Body, URL, Author, Assignees,
// and BlockedBy fields of a ProjectItem by fetching full item data via individual
// node queries. This is the "deep" phase of the two-phase fetch approach.
func (c *Client) FetchItemDetails(item *ProjectItem) error {
	query := `
query($id: ID!) {
  node(id: $id) {
    ... on Issue {
      body
      url
      author { login }
      labels(first: 20) {
        nodes { name }
        pageInfo { hasNextPage endCursor }
      }
      assignees(first: 10) {
        nodes { login }
      }
      blockedBy(first: 10) {
        pageInfo { hasNextPage }
        nodes {
          number
          state
          repository { nameWithOwner }
        }
      }
      comments(first: 100) {
        nodes {
          id
          databaseId
          author { login }
          body
          createdAt
          reactionGroups {
            content
            reactors { totalCount }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
      closedByPullRequestsReferences(first: 10) {
        nodes {
          id
          number
          comments(first: 100) {
            nodes {
              id
              databaseId
              author { login }
              body
              createdAt
              reactionGroups {
                content
                reactors { totalCount }
              }
            }
            pageInfo { hasNextPage endCursor }
          }
          reviewRequests(first: 10) {
            nodes {
              requestedReviewer {
                __typename
                ... on User { login }
                ... on Bot { login }
              }
            }
          }
          latestReviews(first: 10) {
            nodes {
              databaseId
              author { login }
              state
              body
            }
          }
          reviewThreads(first: 50) {
            nodes {
              id
              isResolved
              path
              line
              originalLine
              diffSide
              comments(first: 20) {
                nodes {
                  id
                  databaseId
                  author { login }
                  body
                  createdAt
                  diffHunk
                  path
                  line
                  originalLine
                  reactionGroups {
                    content
                    reactors { totalCount }
                  }
                }
              }
            }
          }
        }
      }
    }
    ... on PullRequest {
      body
      url
      author { login }
      labels(first: 20) {
        nodes { name }
        pageInfo { hasNextPage endCursor }
      }
      assignees(first: 10) {
        nodes { login }
      }
      comments(first: 100) {
        nodes {
          id
          databaseId
          author { login }
          body
          createdAt
          reactionGroups {
            content
            reactors { totalCount }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

	vars := map[string]interface{}{
		"id": item.ID,
	}

	var result struct {
		Data struct {
			Node *struct {
				Body   string `json:"body"`
				URL    string `json:"url"`
				Author *struct {
					Login string `json:"login"`
				} `json:"author"`
				Labels struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"labels"`
				Assignees struct {
					Nodes []struct {
						Login string `json:"login"`
					} `json:"nodes"`
				} `json:"assignees"`
				BlockedBy *struct {
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []blockedByNode `json:"nodes"`
				} `json:"blockedBy"`
				Comments struct {
					Nodes    []commentNodeData `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"comments"`
				LinkedPRs *struct {
					Nodes []struct {
						ID       string `json:"id"`
						Number   int    `json:"number"`
						Comments struct {
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
				} `json:"closedByPullRequestsReferences"`
			} `json:"node"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return fmt.Errorf("fetching details for item #%d: %w", item.Number, err)
	}
	if result.Data.Node == nil {
		return fmt.Errorf("fetching details for item #%d: node not found", item.Number)
	}

	node := result.Data.Node

	// Populate scalar fields
	item.Body = node.Body
	item.URL = node.URL
	if node.Author != nil {
		item.Author = node.Author.Login
	}

	// Reset and populate labels (authoritative set from deep fetch)
	item.Labels = nil
	for _, l := range node.Labels.Nodes {
		item.Labels = append(item.Labels, l.Name)
	}
	if node.Labels.PageInfo.HasNextPage {
		extra, err := c.fetchNodeLabels(item.ID, node.Labels.PageInfo.EndCursor)
		if err != nil {
			return err
		}
		item.Labels = append(item.Labels, extra...)
	}

	// Populate assignees
	item.Assignees = nil
	for _, a := range node.Assignees.Nodes {
		item.Assignees = append(item.Assignees, a.Login)
	}

	// Populate blockedBy (Issues only; PRs will have nil BlockedBy node)
	item.BlockedBy = nil
	if node.BlockedBy != nil {
		if node.BlockedBy.PageInfo.HasNextPage {
			fmt.Printf("[deep-fetch] #%d: blockedBy has more than 10 entries; only first 10 are used\n", item.Number)
		}
		for _, dep := range node.BlockedBy.Nodes {
			d := Dependency{
				Number: dep.Number,
				State:  dep.State,
			}
			if dep.Repository != nil {
				d.Repo = dep.Repository.NameWithOwner
			}
			item.BlockedBy = append(item.BlockedBy, d)
		}
	}

	// Reset append-to fields before repopulating so repeated FetchItemDetails calls
	// (e.g. during progress detection in the turn-extension loop) are idempotent.
	item.Comments = nil
	item.LinkedPRNumber = 0
	item.LinkedPRReviewRequests = nil
	item.LinkedPRReviews = nil
	item.LinkedPRReviewThreadComments = nil
	item.LinkedPRResolvedThreadCount = 0

	// Process issue/PR comments
	commentNodes := node.Comments.Nodes
	if node.Comments.PageInfo.HasNextPage {
		extra, err := c.fetchNodeComments(item.ID, node.Comments.PageInfo.EndCursor)
		if err != nil {
			return err
		}
		commentNodes = append(commentNodes, extra...)
	}
	for _, cm := range commentNodes {
		item.Comments = append(item.Comments, toComment(cm, 0))
	}

	// Merge comments, review requests, and reviews from linked PRs
	if node.LinkedPRs != nil {
		for i, pr := range node.LinkedPRs.Nodes {
			// Record the first linked PR's number for REST re-request and @mention calls.
			if i == 0 {
				item.LinkedPRNumber = pr.Number
			}
			prCommentNodes := pr.Comments.Nodes
			if pr.Comments.PageInfo.HasNextPage {
				extra, err := c.fetchNodeComments(pr.ID, pr.Comments.PageInfo.EndCursor)
				if err != nil {
					return err
				}
				prCommentNodes = append(prCommentNodes, extra...)
			}
			for _, cm := range prCommentNodes {
				item.Comments = append(item.Comments, toComment(cm, pr.Number))
			}
			for _, rr := range pr.ReviewRequests.Nodes {
				if login := rr.RequestedReviewer.Login; login != "" {
					isBot := rr.RequestedReviewer.Typename == "Bot" || isBotLogin(login)
					item.LinkedPRReviewRequests = append(item.LinkedPRReviewRequests, ReviewRequest{Login: login, IsBot: isBot})
				}
			}
			for _, rev := range pr.LatestReviews.Nodes {
				if rev.Author != nil && rev.Author.Login != "" {
					item.LinkedPRReviews = append(item.LinkedPRReviews, PRReview{
						Author:     rev.Author.Login,
						State:      rev.State,
						Body:       rev.Body,
						DatabaseID: rev.DatabaseID,
					})
				}
			}
			for _, thread := range pr.ReviewThreads.Nodes {
				if thread.IsResolved {
					item.LinkedPRResolvedThreadCount++
					continue
				}
				for _, cm := range thread.Comments.Nodes {
					c := toComment(cm, pr.Number)
					c.ReviewThreadID = thread.ID
					item.LinkedPRReviewThreadComments = append(item.LinkedPRReviewThreadComments, c)
				}
			}
		}
	}

	return nil
}

// toComment converts raw commentNodeData into a domain Comment.
func toComment(cm commentNodeData, fromPR int) Comment {
	var line, originalLine int
	if cm.Line != nil {
		line = *cm.Line
	}
	if cm.OriginalLine != nil {
		originalLine = *cm.OriginalLine
	}
	c := Comment{
		ID:           cm.ID,
		DatabaseID:   cm.DatabaseID,
		Body:         cm.Body,
		FromPR:       fromPR,
		Path:         cm.Path,
		Line:         line,
		OriginalLine: originalLine,
		DiffHunk:     cm.DiffHunk,
	}
	if cm.Author != nil {
		c.Author = cm.Author.Login
	}
	if t, err := parseTime(cm.CreatedAt); err == nil {
		c.CreatedAt = t
	}
	for _, rg := range cm.ReactionGroups {
		c.Reactions = append(c.Reactions, ReactionGroup{
			Content: rg.Content,
			Count:   rg.Reactors.TotalCount,
		})
	}
	return c
}

// fetchNodeComments fetches all remaining comments for an issue or PR node,
// starting from the given cursor.
func (c *Client) fetchNodeComments(nodeID, startCursor string) ([]commentNodeData, error) {
	query := `
query($id: ID!, $cursor: String) {
  node(id: $id) {
    ... on Issue {
      comments(first: 100, after: $cursor) {
        nodes {
          id
          databaseId
          author {
            login
          }
          body
          createdAt
          reactionGroups {
            content
            reactors {
              totalCount
            }
          }
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
    ... on PullRequest {
      comments(first: 100, after: $cursor) {
        nodes {
          id
          databaseId
          author {
            login
          }
          body
          createdAt
          reactionGroups {
            content
            reactors {
              totalCount
            }
          }
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

	var allNodes []commentNodeData
	cursor := startCursor
	for {
		vars := map[string]interface{}{
			"id":     nodeID,
			"cursor": cursor,
		}

		var result struct {
			Data struct {
				Node *struct {
					Comments struct {
						Nodes    []commentNodeData `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"comments"`
				} `json:"node"`
			} `json:"data"`
		}

		if err := c.graphqlRequest(query, vars, &result); err != nil {
			return nil, fmt.Errorf("fetching comments for node %s: %w", nodeID, err)
		}
		if result.Data.Node == nil {
			return nil, fmt.Errorf("fetching comments for node %s: node not found or unsupported type", nodeID)
		}

		page := result.Data.Node.Comments
		allNodes = append(allNodes, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			break
		}
		if page.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("fetching comments for node %s: hasNextPage=true but endCursor is empty", nodeID)
		}
		cursor = page.PageInfo.EndCursor
	}
	return allNodes, nil
}

// fetchNodeLabels fetches all remaining labels for an issue or PR node,
// starting from the given cursor.
func (c *Client) fetchNodeLabels(nodeID, startCursor string) ([]string, error) {
	query := `
query($id: ID!, $cursor: String) {
  node(id: $id) {
    ... on Issue {
      labels(first: 100, after: $cursor) {
        nodes {
          name
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
    ... on PullRequest {
      labels(first: 100, after: $cursor) {
        nodes {
          name
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

	var allLabels []string
	cursor := startCursor
	for {
		vars := map[string]interface{}{
			"id":     nodeID,
			"cursor": cursor,
		}

		var result struct {
			Data struct {
				Node *struct {
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"labels"`
				} `json:"node"`
			} `json:"data"`
		}

		if err := c.graphqlRequest(query, vars, &result); err != nil {
			return nil, fmt.Errorf("fetching labels for node %s: %w", nodeID, err)
		}
		if result.Data.Node == nil {
			return nil, fmt.Errorf("fetching labels for node %s: node not found or unsupported type", nodeID)
		}

		page := result.Data.Node.Labels
		for _, n := range page.Nodes {
			allLabels = append(allLabels, n.Name)
		}
		if !page.PageInfo.HasNextPage {
			break
		}
		if page.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("fetching labels for node %s: hasNextPage=true but endCursor is empty", nodeID)
		}
		cursor = page.PageInfo.EndCursor
	}
	return allLabels, nil
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// FetchProjectItemStatus fetches only the Status field value for a single project
// item identified by its node ID (PVTI_...). Returns "" when no status is set.
func (c *Client) FetchProjectItemStatus(itemID string) (string, error) {
	query := `
query($id: ID!) {
  node(id: $id) {
    ... on ProjectV2Item {
      fieldValueByName(name: "Status") {
        ... on ProjectV2ItemFieldSingleSelectValue {
          name
        }
      }
    }
  }
}`
	vars := map[string]interface{}{"id": itemID}

	var result struct {
		Data struct {
			Node *struct {
				FieldValueByName *struct {
					Name string `json:"name"`
				} `json:"fieldValueByName"`
			} `json:"node"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return "", fmt.Errorf("fetching status for item %s: %w", itemID, err)
	}
	if result.Data.Node == nil {
		return "", fmt.Errorf("fetching status for item %s: item not found or not a ProjectV2Item", itemID)
	}
	if result.Data.Node.FieldValueByName == nil {
		return "", nil
	}
	return result.Data.Node.FieldValueByName.Name, nil
}

// FetchProjectItemStatusBatch fetches a map of projectItemNodeID → statusName for
// every item in the project. Dramatically cheaper than FetchProjectBoard because it
// fetches no nested fields. Paginates identically to fetchProjectBoardOnce.
func (c *Client) FetchProjectItemStatusBatch(projectID string) (map[string]string, error) {
	query := `
query($id: ID!, $cursor: String) {
  node(id: $id) {
    ... on ProjectV2 {
      items(first: 100, after: $cursor) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue {
              name
            }
          }
        }
      }
    }
  }
}`

	type statusNode struct {
		ID               string `json:"id"`
		FieldValueByName *struct {
			Name string `json:"name"`
		} `json:"fieldValueByName"`
	}

	out := make(map[string]string)
	cursor := ""

	for {
		vars := map[string]interface{}{"id": projectID}
		if cursor != "" {
			vars["cursor"] = cursor
		}

		var result struct {
			Data struct {
				Node *struct {
					Items struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []statusNode `json:"nodes"`
					} `json:"items"`
				} `json:"node"`
			} `json:"data"`
		}

		if err := c.graphqlRequest(query, vars, &result); err != nil {
			return nil, fmt.Errorf("fetching project item status batch: %w", err)
		}
		if result.Data.Node == nil {
			return nil, fmt.Errorf("fetching project item status batch: project not found or not a ProjectV2")
		}

		for _, n := range result.Data.Node.Items.Nodes {
			if n.FieldValueByName != nil {
				out[n.ID] = n.FieldValueByName.Name
			} else {
				out[n.ID] = ""
			}
		}

		if !result.Data.Node.Items.PageInfo.HasNextPage {
			break
		}
		if result.Data.Node.Items.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("fetching project item status batch: hasNextPage=true but endCursor is empty")
		}
		cursor = result.Data.Node.Items.PageInfo.EndCursor
	}

	return out, nil
}
