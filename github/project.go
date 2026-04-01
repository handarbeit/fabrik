package github

import (
	"fmt"
	"time"
)

// FetchProjectBoard pulls the entire project board in a single GraphQL query.
func (c *Client) FetchProjectBoard(owner, repo string, projectNum int) (*ProjectBoard, error) {
	query := `
query($owner: String!, $repo: String!, $projectNum: Int!) {
  repository(owner: $owner, name: $repo) {
    projectV2(number: $projectNum) {
      id
      items(first: 100) {
        nodes {
          id
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
              body
              url
              author {
                login
              }
              labels(first: 20) {
                nodes {
                  name
                }
              }
              assignees(first: 10) {
                nodes {
                  login
                }
              }
              comments(first: 50) {
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
              }
            }
            ... on PullRequest {
              id
              number
              title
              body
              url
              author {
                login
              }
              labels(first: 20) {
                nodes {
                  name
                }
              }
              assignees(first: 10) {
                nodes {
                  login
                }
              }
              comments(first: 50) {
                nodes {
                  id
                  databaseId
                  author {
                    login
                  }
                  body
                  createdAt
                }
              }
            }
          }
        }
      }
    }
  }
}`

	vars := map[string]interface{}{
		"owner":      owner,
		"repo":       repo,
		"projectNum": projectNum,
	}

	var result struct {
		Data struct {
			Repository struct {
				ProjectV2 struct {
					ID    string `json:"id"`
					Items struct {
						Nodes []struct {
							ID                 string `json:"id"`
							FieldValueByName   *struct {
								Name string `json:"name"`
							} `json:"fieldValueByName"`
							Content struct {
								Typename string `json:"__typename"`
								ID       string `json:"id"`
								Number   int    `json:"number"`
								Title string `json:"title"`
								Body  string `json:"body"`
								URL   string `json:"url"`
								Author *struct {
									Login string `json:"login"`
								} `json:"author"`
								Labels struct {
									Nodes []struct {
										Name string `json:"name"`
									} `json:"nodes"`
								} `json:"labels"`
								Assignees struct {
									Nodes []struct {
										Login string `json:"login"`
									} `json:"nodes"`
								} `json:"assignees"`
								Comments struct {
									Nodes []struct {
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
									} `json:"nodes"`
								} `json:"comments"`
							} `json:"content"`
						} `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"repository"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return nil, fmt.Errorf("fetching project board: %w", err)
	}

	proj := result.Data.Repository.ProjectV2
	board := &ProjectBoard{
		ProjectID: proj.ID,
	}

	for _, node := range proj.Items.Nodes {
		// Skip items whose content was not returned (empty content ID, e.g. draft issues)
		if node.Content.ID == "" {
			continue
		}

		item := ProjectItem{
			ID:     node.Content.ID,
			ItemID: node.ID,
			Number: node.Content.Number,
			Title:  node.Content.Title,
			Body:   node.Content.Body,
			URL:    node.Content.URL,
			IsPR:   node.Content.Typename == "PullRequest",
		}

		if node.FieldValueByName != nil {
			item.Status = node.FieldValueByName.Name
		}

		if node.Content.Author != nil {
			item.Author = node.Content.Author.Login
		}

		for _, l := range node.Content.Labels.Nodes {
			item.Labels = append(item.Labels, l.Name)
		}

		for _, a := range node.Content.Assignees.Nodes {
			item.Assignees = append(item.Assignees, a.Login)
		}

		for _, cm := range node.Content.Comments.Nodes {
			comment := Comment{
				ID:         cm.ID,
				DatabaseID: cm.DatabaseID,
				Body:       cm.Body,
			}
			if cm.Author != nil {
				comment.Author = cm.Author.Login
			}
			// Parse time, ignore error (zero value is fine)
			if t, err := parseTime(cm.CreatedAt); err == nil {
				comment.CreatedAt = t
			}
			for _, rg := range cm.ReactionGroups {
				comment.Reactions = append(comment.Reactions, ReactionGroup{
					Content: rg.Content,
					Count:   rg.Reactors.TotalCount,
				})
			}
			item.Comments = append(item.Comments, comment)
		}

		board.Items = append(board.Items, item)
	}

	return board, nil
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}
