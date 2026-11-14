package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/closingrefs"
)

// ghClient is the stdlib HTTP client for the three things the check reads: the PR, its
// commits, and board membership. restToken reads the PR; boardToken reads the board.
type ghClient struct {
	http        *http.Client
	restBase    string // e.g. https://api.github.com
	graphqlURL  string
	restToken   string
	boardToken  string
	repo        string // owner/repo of the PR
	pr          int
	boardOwner  string
	boardNumber int

	pulled          bool
	prTitle, prBody string
}

var errNoBoardToken = errors.New("no board token is configured (set the CLOSING_REFS_BOARD_TOKEN or ADD_TO_PROJECT_PAT repository secret)")

func newGHClient(restBase, graphqlURL, restToken, boardToken, repo string, pr int, boardOwner string, boardNumber int) *ghClient {
	if restBase == "" {
		restBase = "https://api.github.com"
	}
	restBase = strings.TrimRight(restBase, "/")
	if graphqlURL == "" {
		graphqlURL = restBase + "/graphql"
	}
	if restToken == "" {
		restToken = boardToken
	}
	return &ghClient{
		http: &http.Client{Timeout: 30 * time.Second}, restBase: restBase, graphqlURL: graphqlURL,
		restToken: restToken, boardToken: boardToken, repo: repo, pr: pr,
		boardOwner: boardOwner, boardNumber: boardNumber,
	}
}

func (c *ghClient) do(ctx context.Context, method, url, token string, body []byte, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "fabrik-check-closing-refs")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := string(data)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, strings.TrimPrefix(url, c.restBase), resp.StatusCode, strings.TrimSpace(snippet))
	}
	return json.Unmarshal(data, v)
}

// pull reads the PR once; Title and Body share the result.
func (c *ghClient) pull(ctx context.Context) (title, body string, err error) {
	if c.pulled {
		return c.prTitle, c.prBody, nil
	}
	var pr struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/pulls/%d", c.restBase, c.repo, c.pr), c.restToken, nil, &pr); err != nil {
		return "", "", err
	}
	if pr.Title != nil {
		c.prTitle = *pr.Title
	}
	if pr.Body != nil {
		c.prBody = *pr.Body
	}
	c.pulled = true
	return c.prTitle, c.prBody, nil
}

func (c *ghClient) Title(ctx context.Context) (string, error) {
	t, _, err := c.pull(ctx)
	return t, err
}

func (c *ghClient) Body(ctx context.Context) (string, error) {
	_, b, err := c.pull(ctx)
	return b, err
}

func (c *ghClient) Commits(ctx context.Context) ([]closingrefs.Commit, error) {
	var all []closingrefs.Commit
	// The endpoint returns at most 250 commits; 3 pages of 100 cover that and one more to
	// notice a full page.
	for page := 1; page <= 3; page++ {
		var batch []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		}
		u := fmt.Sprintf("%s/repos/%s/pulls/%d/commits?per_page=100&page=%d", c.restBase, c.repo, c.pr, page)
		if err := c.do(ctx, http.MethodGet, u, c.restToken, nil, &batch); err != nil {
			return nil, err
		}
		for _, b := range batch {
			all = append(all, closingrefs.Commit{SHA: b.SHA, Message: b.Commit.Message})
		}
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

type gqlError struct {
	Type    string   `json:"type"`
	Message string   `json:"message"`
	Path    []string `json:"path"`
}

type gqlResp struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

func (c *ghClient) graphql(ctx context.Context, query string, vars map[string]any) (gqlResp, error) {
	if c.boardToken == "" {
		return gqlResp{}, errNoBoardToken
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return gqlResp{}, err
	}
	var r gqlResp
	err = c.do(ctx, http.MethodPost, c.graphqlURL, c.boardToken, payload, &r)
	return r, err
}

func (c *ghClient) Verify(ctx context.Context) error {
	r, err := c.graphql(ctx,
		`query($login:String!,$n:Int!){organization(login:$login){projectV2(number:$n){id}}}`,
		map[string]any{"login": c.boardOwner, "n": c.boardNumber})
	if err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("the board lookup returned an error: %s", r.Errors[0].Message)
	}
	var d struct {
		Organization *struct {
			ProjectV2 *struct {
				ID string `json:"id"`
			} `json:"projectV2"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return fmt.Errorf("unreadable board lookup response: %w", err)
	}
	if d.Organization == nil || d.Organization.ProjectV2 == nil || d.Organization.ProjectV2.ID == "" {
		return fmt.Errorf("project %d of %s is not visible to the configured token (it needs read access to organization projects)", c.boardNumber, c.boardOwner)
	}
	return nil
}

func (c *ghClient) OnBoard(ctx context.Context, repo string, number int) (bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return false, fmt.Errorf("malformed repository %q", repo)
	}
	r, err := c.graphql(ctx, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){projectItems(first:50){pageInfo{hasNextPage} nodes{project{number owner{... on Organization{login}}}}}}}}`,
		map[string]any{"o": owner, "r": name, "n": number})
	if err != nil {
		return false, err
	}
	for _, e := range r.Errors {
		// A missing repository, issue, or a number that is a pull request: not a report.
		if e.Type != "NOT_FOUND" {
			return false, errors.New(e.Message)
		}
	}
	var d struct {
		Repository *struct {
			Issue *struct {
				ProjectItems struct {
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						Project struct {
							Number int `json:"number"`
							Owner  struct {
								Login string `json:"login"`
							} `json:"owner"`
						} `json:"project"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	if len(r.Data) > 0 && string(r.Data) != "null" {
		if err := json.Unmarshal(r.Data, &d); err != nil {
			return false, fmt.Errorf("unreadable response: %w", err)
		}
	}
	if d.Repository == nil {
		// GraphQL answers NOT_FOUND for a repository the token cannot read, exactly as it does
		// for one that does not exist, so a hidden repository could hold a board #3 issue.
		// Only a missing issue inside a readable repository is "not a report".
		return false, fmt.Errorf("repository %s is not readable with the configured token, so board membership of #%d cannot be verified", repo, number)
	}
	if d.Repository.Issue == nil {
		if len(r.Errors) == 0 {
			// No NOT_FOUND error yet no issue: refuse to treat silence as "not a report".
			return false, errors.New("the response held neither an issue nor an error")
		}
		return false, nil
	}
	items := d.Repository.Issue.ProjectItems
	for _, n := range items.Nodes {
		if n.Project.Number == c.boardNumber && strings.EqualFold(n.Project.Owner.Login, c.boardOwner) {
			return true, nil
		}
	}
	if items.PageInfo.HasNextPage {
		return false, errors.New("the issue is on more than 50 projects, so board membership cannot be read in one page")
	}
	return false, nil
}
