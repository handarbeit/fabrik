// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package github

import "fmt"

// StatusField holds the Status field metadata for a project.
type StatusField struct {
	FieldID string
	Options map[string]string // status name -> option ID
}

// UpdateProjectItemStatus moves an item to a different status column on the project board.
func (c *Client) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	query := `
mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId,
    itemId: $itemId,
    fieldId: $fieldId,
    value: { singleSelectOptionId: $optionId }
  }) {
    projectV2Item {
      id
    }
  }
}`
	vars := map[string]interface{}{
		"projectId": projectID,
		"itemId":    itemID,
		"fieldId":   statusFieldID,
		"optionId":  statusOptionID,
	}

	var result struct{}
	return c.graphqlRequest(query, vars, &result)
}

// ArchiveProjectItem archives a project item so it no longer appears in paginated board results.
// Archiving is idempotent — calling it on an already-archived item is a no-op.
func (c *Client) ArchiveProjectItem(projectID, itemID string) error {
	query := `
mutation($projectId: ID!, $itemId: ID!) {
  archiveProjectV2Item(input: {projectId: $projectId, itemId: $itemId}) {
    item { id }
  }
}`
	vars := map[string]interface{}{
		"projectId": projectID,
		"itemId":    itemID,
	}

	var result struct{}
	return c.graphqlRequest(query, vars, &result)
}

// FetchStatusField retrieves the Status field ID and its option IDs for a project.
func (c *Client) FetchStatusField(projectID string) (*StatusField, error) {
	query := `
query($projectId: ID!) {
  node(id: $projectId) {
    ... on ProjectV2 {
      field(name: "Status") {
        ... on ProjectV2SingleSelectField {
          id
          options {
            id
            name
          }
        }
      }
    }
  }
}`
	vars := map[string]interface{}{
		"projectId": projectID,
	}

	var result struct {
		Data struct {
			Node struct {
				Field *struct {
					ID      string `json:"id"`
					Options []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"options"`
				} `json:"field"`
			} `json:"node"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return nil, err
	}

	if result.Data.Node.Field == nil || result.Data.Node.Field.ID == "" {
		return nil, fmt.Errorf("project %q has no Status field", projectID)
	}

	sf := &StatusField{
		FieldID: result.Data.Node.Field.ID,
		Options: make(map[string]string),
	}
	for _, opt := range result.Data.Node.Field.Options {
		sf.Options[opt.Name] = opt.ID
	}

	return sf, nil
}
