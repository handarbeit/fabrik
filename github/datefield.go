package github

import "fmt"

// DateField identifies a ProjectV2 date field by node ID. It backs the
// display-only "Last activity" field (#2049): Fabrik writes a calendar date
// into it and never reads the value back.
type DateField struct {
	ID   string
	Name string
}

// FetchDateField returns the project's date field with the given name. It
// returns (nil, nil) when no such field exists or the field is not a DATE
// field — an absent display field is a normal, silent condition, not an
// error. A transport or GraphQL failure is returned as an error.
func (c *Client) FetchDateField(projectID, name string) (*DateField, error) {
	id, fieldName, err := c.fetchProjectFieldID(projectID, name, "DATE")
	if err != nil || id == "" {
		return nil, err
	}
	return &DateField{ID: id, Name: fieldName}, nil
}

// updateProjectItemDateFieldMutation is the GraphQL mutation used by
// UpdateProjectItemDateField. The Date scalar is a plain YYYY-MM-DD string.
const updateProjectItemDateFieldMutation = `
mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $date: Date!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId,
    itemId: $itemId,
    fieldId: $fieldId,
    value: { date: $date }
  }) {
    projectV2Item {
      id
    }
  }
}`

// UpdateProjectItemDateField sets a date field's value on a project item.
// date must be in YYYY-MM-DD form.
func (c *Client) UpdateProjectItemDateField(projectID, itemID, fieldID, date string) error {
	vars := map[string]interface{}{
		"projectId": projectID,
		"itemId":    itemID,
		"fieldId":   fieldID,
		"date":      date,
	}

	var result struct{}
	if err := c.graphqlRequest(updateProjectItemDateFieldMutation, vars, &result); err != nil {
		return fmt.Errorf("updating date field %s for item %s on project %s: %w", fieldID, itemID, projectID, err)
	}
	return nil
}
