package github

import (
	"fmt"
	"net/url"
)

// FetchUserID returns the numeric account ID of login via REST GET
// /users/{login}. It exists for GitHub App commit attribution (#1893): an
// App's commits link to its avatar only when the author email is
// "<id>+<slug>[bot]@users.noreply.github.com", and the ID is that of the
// "<slug>[bot]" user. The endpoint is public. The login is path-escaped
// because a bot login contains "[" and "]".
func (c *Client) FetchUserID(login string) (int64, error) {
	if login == "" {
		return 0, fmt.Errorf("fetching user ID: empty login")
	}
	apiURL := c.baseURL + "/users/" + url.PathEscape(login)
	var raw struct {
		ID int64 `json:"id"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return 0, fmt.Errorf("fetching user %q: %w", login, err)
	}
	if raw.ID <= 0 {
		return 0, fmt.Errorf("fetching user %q: response carried no id", login)
	}
	return raw.ID, nil
}
