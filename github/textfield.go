package github

import (
	"fmt"
	"strings"
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

// FetchTextField returns the project's text field with the given name. It
// returns (nil, nil) when no such field exists or the field is not a TEXT
// field — an absent display field is a normal, silent condition, not an
// error. A transport or GraphQL failure is returned as an error.
func (c *Client) FetchTextField(projectID, name string) (*TextField, error) {
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
		return nil, fmt.Errorf("fetching text field %q for project %s: %w", name, projectID, err)
	}

	f := result.Data.Node.Field
	if f == nil || f.ID == "" || !strings.EqualFold(f.DataType, "TEXT") {
		return nil, nil
	}
	return &TextField{ID: f.ID, Name: f.Name}, nil
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
