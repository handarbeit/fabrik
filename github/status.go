package github

import (
	"fmt"
	"sort"
)

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

// AddBoardColumn adds a new option to the Status single-select field on a
// GitHub ProjectV2 board. The mutation replaces the entire options list, so
// existingOptions (name→ID from a recent FetchStatusField) must be passed to
// preserve existing columns. Returns the new option's ID.
func (c *Client) AddBoardColumn(projectID, fieldID string, existingOptions map[string]string, newName string) (string, error) {
	// Build the full options list: existing names (sorted for determinism) + new name.
	names := make([]string, 0, len(existingOptions))
	for name := range existingOptions {
		names = append(names, name)
	}
	sort.Strings(names)

	options := make([]map[string]string, 0, len(names)+1)
	for _, name := range names {
		options = append(options, map[string]string{"name": name})
	}
	options = append(options, map[string]string{"name": newName})

	query := `
mutation($projectId: ID!, $fieldId: ID!, $options: [ProjectV2SingleSelectFieldOptionInput!]!) {
  updateProjectV2FieldDefinition(input: {
    projectId: $projectId,
    fieldId: $fieldId,
    singleSelectOptions: $options
  }) {
    field {
      ... on ProjectV2SingleSelectField {
        options {
          id
          name
        }
      }
    }
  }
}`
	vars := map[string]interface{}{
		"projectId": projectID,
		"fieldId":   fieldID,
		"options":   options,
	}

	var result struct {
		Data struct {
			UpdateProjectV2FieldDefinition struct {
				Field struct {
					Options []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"options"`
				} `json:"field"`
			} `json:"updateProjectV2FieldDefinition"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return "", fmt.Errorf("adding board column %q: %w", newName, err)
	}

	// Find the new option's ID in the response.
	for _, opt := range result.Data.UpdateProjectV2FieldDefinition.Field.Options {
		if opt.Name == newName {
			return opt.ID, nil
		}
	}

	return "", fmt.Errorf("adding board column %q: option not found in mutation response", newName)
}
