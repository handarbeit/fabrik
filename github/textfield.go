package github

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// TextField identifies a ProjectV2 text field by node ID. It backs the
// display-only status-line field (#2048): Fabrik writes a one-line status
// into it and never reads the value back.
type TextField struct {
	ID   string
	Name string
}

// fetchTextFieldQuery looks a project field up by name. The field is a
// ProjectV2FieldConfiguration union; only the plain ProjectV2Field member
// carries dataType, so iteration and single-select fields come back with no
// id and are reported as "not a text field".
const fetchTextFieldQuery = `
query($projectId: ID!, $name: String!) {
  node(id: $projectId) {
    ... on ProjectV2 {
      field(name: $name) {
        ... on ProjectV2Field {
          id
          name
          dataType
        }
      }
    }
  }
}`

// fieldNotFoundMessage is the message GitHub gives for a ProjectV2.field(name:)
// lookup of a name the project does not have. GitHub reports that as a GraphQL
// error (not a null field): "Could not resolve to a
// Unions::ProjectV2FieldConfiguration with the name '<name>'" (#2218).
const fieldNotFoundMessage = "Could not resolve to a Unions::ProjectV2FieldConfiguration with the name"

// isFieldNotFoundError reports whether err is GitHub's "no such field" error
// for the field lookup. The match is deliberately narrow: the GraphQL error's
// message must carry the exact shape above and, when GitHub reports a path, the
// path must end in "field". Any other error — a bad project node id ("Could not
// resolve to a node with the global id"), a 5xx, a transport failure — is not a
// missing field.
func isFieldNotFoundError(err error) bool {
	var ge *GraphQLError
	if !errors.As(err, &ge) {
		return false
	}
	if !strings.Contains(ge.Message, fieldNotFoundMessage) {
		return false
	}
	if len(ge.Path) == 0 {
		return true
	}
	last, ok := ge.Path[len(ge.Path)-1].(string)
	return ok && last == "field"
}

// FieldTypeMismatchError reports that the project has a field with the
// requested name but not of the requested type: either a plain field of another
// dataType, or a single-select / iteration field (which the ProjectV2Field
// fragment returns as an empty object). Found is the dataType when known, ""
// otherwise. fabrik repair-board never writes such a field (ADR 2110), so the
// engine surfaces it separately from an absent one.
type FieldTypeMismatchError struct {
	Name  string
	Want  string
	Found string
}

func (e *FieldTypeMismatchError) Error() string {
	if e.Found == "" {
		return fmt.Sprintf("project field %q exists but is not a %s field", e.Name, strings.ToLower(e.Want))
	}
	return fmt.Sprintf("project field %q exists but is a %s field, not %s", e.Name, strings.ToLower(e.Found), strings.ToLower(e.Want))
}

// fetchProjectFieldID looks a plain project field up by name and returns its
// id and name when its dataType is want (case-insensitive). It returns
// ("", "", nil) when the field is absent — including GitHub's "could not
// resolve ... with the name" error — and a *FieldTypeMismatchError when a field
// of that name exists with another type. Shared by FetchTextField and
// FetchDateField so there is one query call site.
func (c *Client) fetchProjectFieldID(projectID, name, want string) (id, fieldName string, err error) {
	vars := map[string]interface{}{
		"projectId": projectID,
		"name":      name,
	}

	var result struct {
		Data struct {
			Node struct {
				Field *struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					DataType string `json:"dataType"`
				} `json:"field"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := c.graphqlRequest(fetchTextFieldQuery, vars, &result); err != nil {
		if isFieldNotFoundError(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("fetching %s field %q for project %s: %w", strings.ToLower(want), name, projectID, err)
	}

	f := result.Data.Node.Field
	if f == nil {
		return "", "", nil
	}
	if f.ID == "" {
		// A same-named single-select or iteration field: the union member
		// carries no id through the ProjectV2Field fragment.
		return "", "", &FieldTypeMismatchError{Name: name, Want: want}
	}
	if !strings.EqualFold(f.DataType, want) {
		return "", "", &FieldTypeMismatchError{Name: name, Want: want, Found: f.DataType}
	}
	return f.ID, f.Name, nil
}

// FetchTextField returns the project's text field with the given name. It
// returns (nil, nil) when no such field exists — an absent display field is a
// normal condition, not an error — and a *FieldTypeMismatchError when the name
// is taken by a field that is not TEXT. A transport or GraphQL failure is
// returned as an error.
func (c *Client) FetchTextField(projectID, name string) (*TextField, error) {
	id, fieldName, err := c.fetchProjectFieldID(projectID, name, "TEXT")
	if err != nil || id == "" {
		return nil, err
	}
	return &TextField{ID: id, Name: fieldName}, nil
}

// updateProjectItemTextFieldMutation is the GraphQL mutation used by
// UpdateProjectItemTextField.
const updateProjectItemTextFieldMutation = `
mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $text: String!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId,
    itemId: $itemId,
    fieldId: $fieldId,
    value: { text: $text }
  }) {
    projectV2Item {
      id
    }
  }
}`

// UpdateProjectItemTextField sets a text field's value on a project item.
func (c *Client) UpdateProjectItemTextField(projectID, itemID, fieldID, text string) error {
	vars := map[string]interface{}{
		"projectId": projectID,
		"itemId":    itemID,
		"fieldId":   fieldID,
		"text":      text,
	}

	var result struct{}
	if err := c.graphqlRequest(updateProjectItemTextFieldMutation, vars, &result); err != nil {
		return fmt.Errorf("updating text field %s for item %s on project %s: %w", fieldID, itemID, projectID, err)
	}
	return nil
}

// clearProjectItemFieldMutation is the GraphQL mutation used by
// ClearProjectItemField.
const clearProjectItemFieldMutation = `
mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!) {
  clearProjectV2ItemFieldValue(input: {
    projectId: $projectId,
    itemId: $itemId,
    fieldId: $fieldId
  }) {
    projectV2Item {
      id
    }
  }
}`

// ClearProjectItemField clears a field's value on a project item.
func (c *Client) ClearProjectItemField(projectID, itemID, fieldID string) error {
	vars := map[string]interface{}{
		"projectId": projectID,
		"itemId":    itemID,
		"fieldId":   fieldID,
	}

	var result struct{}
	if err := c.graphqlRequest(clearProjectItemFieldMutation, vars, &result); err != nil {
		return fmt.Errorf("clearing field %s for item %s on project %s: %w", fieldID, itemID, projectID, err)
	}
	return nil
}

// statusLineValue is the display field's value node as selected by the board
// and probe queries — only its own updatedAt, never the text (display-only,
// #2048: nothing reads the value back).
type statusLineValue struct {
	UpdatedAt string `json:"updatedAt"`
}

// statusValue is the Status single-select value selected by the shallow board
// and probe queries: its name and its own updatedAt, which only a Status write
// bumps (unlike the project item's updatedAt, which every field write bumps).
type statusValue struct {
	Name      string `json:"name"`
	UpdatedAt string `json:"updatedAt"`
}

// movedAt is GitHub's time for the Status value, or the zero time when the
// timestamp is absent or unparseable (unknown, never epoch) (#2246).
func (v *statusValue) movedAt() time.Time {
	if v == nil || v.UpdatedAt == "" {
		return time.Time{}
	}
	t, err := parseTime(v.UpdatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// statusLineUpdatedAtTolerance absorbs timestamp skew between the field
// value's updatedAt and the project item's updatedAt, which one mutation
// bumps at (nearly) the same instant. A real change newer than this
// still counts.
const statusLineUpdatedAtTolerance = 2 * time.Second

// SetStatusLineField tells the client the name of the display-only
// status-line text field ("" = feature off). The board and probe queries then
// also select that field's own updatedAt so a project item's updatedAt bump
// that was caused only by a display-field write can be discounted — otherwise
// every status-line write would look like a change to the item (drift, deep
// fetch). Deliberately not on the engine's GitHubClient interface. Safe to
// call concurrently.
func (c *Client) SetStatusLineField(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusLineField = name
}

// SetLastActivityField is SetStatusLineField for the "Last activity" date
// field (#2049): "" = off.
func (c *Client) SetLastActivityField(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActivityField = name
}

// SetLastRunField is SetStatusLineField for the "Last run" text field
// (#2049): "" = off.
func (c *Client) SetLastRunField(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRunField = name
}

// addDisplayFieldVars adds the display-field query variables. Each name
// variable is non-null, so it is always sent; the @include directive keeps the
// selection off the wire when that field is disabled.
func (c *Client) addDisplayFieldVars(vars map[string]interface{}) {
	c.mu.Lock()
	statusLine, lastActivity, lastRun := c.statusLineField, c.lastActivityField, c.lastRunField
	c.mu.Unlock()
	vars["statusLine"] = statusLine
	vars["withStatusLine"] = statusLine != ""
	vars["lastActivity"] = lastActivity
	vars["withLastActivity"] = lastActivity != ""
	vars["lastRun"] = lastRun
	vars["withLastRun"] = lastRun != ""
}

// projectItemUpdatedAt parses a project item's updatedAt for inclusion in an
// item's effective updatedAt. It reports ok=false when the timestamp is
// unparseable or when it is explained entirely by a write to a display field
// (not after the latest present display-field value's own updatedAt, within
// tolerance): such a bump is Fabrik's own display write and must not read as
// item activity. With no display-field value it behaves exactly as a plain
// parse. Status is compared directly elsewhere, so a Status move is still
// detected. Nil values (field off or unset on the item) are ignored.
func projectItemUpdatedAt(itemUpdatedAt string, display ...*statusLineValue) (time.Time, bool) {
	t, err := parseTime(itemUpdatedAt)
	if err != nil {
		return time.Time{}, false
	}
	var latest time.Time
	for _, v := range display {
		if v == nil {
			continue
		}
		if ft, err := parseTime(v.UpdatedAt); err == nil && ft.After(latest) {
			latest = ft
		}
	}
	if !latest.IsZero() && !t.After(latest.Add(statusLineUpdatedAtTolerance)) {
		return time.Time{}, false
	}
	return t, true
}
