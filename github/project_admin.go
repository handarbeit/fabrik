package github

import "fmt"

// This file implements the board-administration mutations for #1714:
// creating a Projects v2 board from stage configs (R1) and repairing an
// existing board's Status column set (R3), including R4's id-preserving
// updateProjectV2Field payload. It is the first "administer the board
// itself" write path in this package — project.go/status.go only fetch
// board state or move/add individual items. See
// adrs/1714-board-admin-create-and-repair.md.

// repositoryOwnerQuery is the GraphQL query used by ResolveOwner. A single
// round trip resolves both the owner's node ID (needed for createProjectV2's
// ownerId) and its type ("organization"/"user", needed for R7's refusal
// gate) via __typename — RepositoryOwner is a polymorphic interface
// (Organization | User) whose "id" field is exposed directly on the
// interface, so no inline fragment is needed for id, only for __typename
// disambiguation.
const repositoryOwnerQuery = `
query($login: String!) {
  repositoryOwner(login: $login) {
    __typename
    id
  }
}`

// ResolveOwner resolves a bare owner login (org or user) to its GraphQL node
// ID and type ("organization" or "user"), via a single repositoryOwner query.
// Returns an error if the login does not resolve to a known owner.
func (c *Client) ResolveOwner(login string) (ownerID, ownerType string, err error) {
	vars := map[string]interface{}{"login": login}

	var result struct {
		Data struct {
			RepositoryOwner *struct {
				Typename string `json:"__typename"`
				ID       string `json:"id"`
			} `json:"repositoryOwner"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(repositoryOwnerQuery, vars, &result); err != nil {
		return "", "", fmt.Errorf("resolving owner %q: %w", login, err)
	}
	if result.Data.RepositoryOwner == nil || result.Data.RepositoryOwner.ID == "" {
		return "", "", fmt.Errorf("resolving owner %q: no such user or organization", login)
	}

	switch result.Data.RepositoryOwner.Typename {
	case "Organization":
		ownerType = "organization"
	case "User":
		ownerType = "user"
	default:
		return "", "", fmt.Errorf("resolving owner %q: unrecognized owner type %q", login, result.Data.RepositoryOwner.Typename)
	}

	return result.Data.RepositoryOwner.ID, ownerType, nil
}

// repositoryIDForBoardQuery is the GraphQL query used by FetchRepositoryID.
const repositoryIDForBoardQuery = `
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    id
  }
}`

// FetchRepositoryID resolves owner/name to the repository's GraphQL node ID,
// for use as createProjectV2Input.repositoryId (linking the new board to its
// repo at creation time).
func (c *Client) FetchRepositoryID(owner, name string) (string, error) {
	vars := map[string]interface{}{"owner": owner, "name": name}

	var result struct {
		Data struct {
			Repository *struct {
				ID string `json:"id"`
			} `json:"repository"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(repositoryIDForBoardQuery, vars, &result); err != nil {
		return "", fmt.Errorf("fetching repository id for %s/%s: %w", owner, name, err)
	}
	if result.Data.Repository == nil || result.Data.Repository.ID == "" {
		return "", fmt.Errorf("fetching repository id for %s/%s: not found", owner, name)
	}
	return result.Data.Repository.ID, nil
}

// createProjectV2Mutation is the GraphQL mutation used by CreateProjectV2
// when a repository is given. repositoryId is supplied inline (rather than
// via a separate linkProjectV2ToRepository call) so a board meant to be
// linked is never created unlinked, even transiently.
const createProjectV2Mutation = `
mutation($ownerId: ID!, $title: String!, $repositoryId: ID!) {
  createProjectV2(input: {ownerId: $ownerId, title: $title, repositoryId: $repositoryId}) {
    projectV2 {
      id
      number
    }
  }
}`

// createProjectV2UnlinkedMutation creates an org board linked to no
// repository. The engine discovers repos from the board's items, so a link
// is only a convenience (the board shows on that repo's Projects tab).
const createProjectV2UnlinkedMutation = `
mutation($ownerId: ID!, $title: String!) {
  createProjectV2(input: {ownerId: $ownerId, title: $title}) {
    projectV2 {
      id
      number
    }
  }
}`

// CreateProjectV2 creates a new Projects v2 board owned by ownerID (an
// Organization or User node ID, from ResolveOwner), titled title, and linked
// to repositoryID (from FetchRepositoryID) at creation time. Returns the new
// project's node ID and its number (as shown in its URL).
func (c *Client) CreateProjectV2(ownerID, title, repositoryID string) (projectID string, number int, err error) {
	mutation := createProjectV2Mutation
	vars := map[string]interface{}{
		"ownerId":      ownerID,
		"title":        title,
		"repositoryId": repositoryID,
	}
	if repositoryID == "" {
		mutation = createProjectV2UnlinkedMutation
		delete(vars, "repositoryId")
	}

	var result struct {
		Data struct {
			CreateProjectV2 struct {
				ProjectV2 *struct {
					ID     string `json:"id"`
					Number int    `json:"number"`
				} `json:"projectV2"`
			} `json:"createProjectV2"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(mutation, vars, &result); err != nil {
		return "", 0, fmt.Errorf("creating project %q for owner %s: %w", title, ownerID, err)
	}
	if result.Data.CreateProjectV2.ProjectV2 == nil {
		return "", 0, fmt.Errorf("creating project %q for owner %s: no project returned", title, ownerID)
	}
	return result.Data.CreateProjectV2.ProjectV2.ID, result.Data.CreateProjectV2.ProjectV2.Number, nil
}

// updateProjectV2ShortDescriptionMutation is the GraphQL mutation used by
// SetProjectDescription.
const updateProjectV2ShortDescriptionMutation = `
mutation($projectId: ID!, $shortDescription: String!) {
  updateProjectV2(input: {projectId: $projectId, shortDescription: $shortDescription}) {
    projectV2 {
      id
    }
  }
}`

// SetProjectDescription sets a Projects v2 board's short description field.
func (c *Client) SetProjectDescription(projectID, shortDescription string) error {
	vars := map[string]interface{}{
		"projectId":        projectID,
		"shortDescription": shortDescription,
	}

	var result struct{}
	if err := c.graphqlRequest(updateProjectV2ShortDescriptionMutation, vars, &result); err != nil {
		return fmt.Errorf("setting description for project %s: %w", projectID, err)
	}
	return nil
}

// StatusOptionInput is one entry in the ordered option list sent to
// SetStatusFieldOptions. For an existing option being preserved, ID must be
// the option's current id (non-nil) and Color/Description must be its
// current values, echoed back unchanged (R4) — omitting ID mints a fresh id
// for every option server-side and silently clears every item's Status
// (fieldValueByName returns null), and omitting Color/Description on an
// echoed option fails the mutation's required-field validation. For a new
// option being added, ID must be nil so the server mints one; Color and
// Description may be any valid value (repair.go's default: GRAY, "").
type StatusOptionInput struct {
	ID          *string
	Name        string
	Color       string
	Description string
}

// updateProjectV2FieldMutation is the GraphQL mutation used by
// SetStatusFieldOptions. See StatusOptionInput's doc comment for the R4
// id-preservation contract this call site depends on.
const updateProjectV2FieldMutation = `
mutation($fieldId: ID!, $options: [ProjectV2SingleSelectFieldOptionInput!]!) {
  updateProjectV2Field(input: {fieldId: $fieldId, singleSelectOptions: $options}) {
    projectV2Field {
      ... on ProjectV2SingleSelectField {
        id
      }
    }
  }
}`

// SetStatusFieldOptions replaces the Status field's entire option set with
// options, in the given order. This is the single most consequential call in
// this package: updateProjectV2Field's singleSelectOptions input REPLACES the
// whole option list, so any existing option not included in options is
// deleted, and any included option missing an ID is created fresh (losing
// every item's existing Status assignment to that column). Callers MUST
// build options from a fresh FetchStatusField read, echoing every existing
// option's ID/Color/Description unchanged and appending new options with a
// nil ID — see StatusOptionInput's doc comment and R4/R6 in
// adrs/1714-board-admin-create-and-repair.md.
func (c *Client) SetStatusFieldOptions(fieldID string, options []StatusOptionInput) error {
	rendered := make([]map[string]interface{}, 0, len(options))
	for _, opt := range options {
		entry := map[string]interface{}{
			"name":        opt.Name,
			"color":       opt.Color,
			"description": opt.Description,
		}
		if opt.ID != nil {
			entry["id"] = *opt.ID
		}
		rendered = append(rendered, entry)
	}

	vars := map[string]interface{}{
		"fieldId": fieldID,
		"options": rendered,
	}

	var result struct{}
	if err := c.graphqlRequest(updateProjectV2FieldMutation, vars, &result); err != nil {
		return fmt.Errorf("setting status field options for field %s: %w", fieldID, err)
	}
	return nil
}

// Display-field data types accepted by CreateProjectV2Field and reported by
// FetchProjectFields. They are GitHub's ProjectV2CustomFieldType /
// ProjectV2FieldType enum spellings.
const (
	FieldTypeText = "TEXT"
	FieldTypeDate = "DATE"
)

// ProjectField is one field on a Projects v2 board as listed by
// FetchProjectFields. DataType is GitHub's ProjectV2FieldType spelling
// ("TEXT", "DATE", "SINGLE_SELECT", "ITERATION", "ASSIGNEES", ...), so a
// caller can tell a present field of the wrong type from an absent one.
type ProjectField struct {
	ID       string
	Name     string
	DataType string
}

// fetchProjectFieldsQuery lists every field of a project with its real data
// type. Unlike the engine's field(name:) lookup (fetchTextFieldQuery), which
// collapses "absent" and "wrong type" into one empty result, this returns the
// field whatever its type, which R3 of #2110 needs to report a type mismatch.
const fetchProjectFieldsQuery = `
query($projectId: ID!, $after: String) {
  node(id: $projectId) {
    ... on ProjectV2 {
      fields(first: 100, after: $after) {
        nodes {
          ... on ProjectV2FieldCommon {
            id
            name
            dataType
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

// FetchProjectFields lists every field on the project (built-in and custom),
// following pagination. Used by repair-board and init --create-board to
// classify the display fields before creating any (#2110).
func (c *Client) FetchProjectFields(projectID string) ([]ProjectField, error) {
	var fields []ProjectField
	after := ""
	for {
		vars := map[string]interface{}{"projectId": projectID}
		if after != "" {
			vars["after"] = after
		}

		var result struct {
			Data struct {
				Node *struct {
					Fields *struct {
						Nodes []struct {
							ID       string `json:"id"`
							Name     string `json:"name"`
							DataType string `json:"dataType"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"fields"`
				} `json:"node"`
			} `json:"data"`
		}

		if err := c.graphqlRequest(fetchProjectFieldsQuery, vars, &result); err != nil {
			return nil, fmt.Errorf("listing fields for project %s: %w", projectID, err)
		}
		if result.Data.Node == nil || result.Data.Node.Fields == nil {
			return nil, fmt.Errorf("listing fields for project %s: project not found", projectID)
		}
		for _, n := range result.Data.Node.Fields.Nodes {
			// A node matching no fragment decodes to all-empty; skip it.
			if n.ID == "" && n.Name == "" {
				continue
			}
			fields = append(fields, ProjectField{ID: n.ID, Name: n.Name, DataType: n.DataType})
		}
		pi := result.Data.Node.Fields.PageInfo
		if !pi.HasNextPage || pi.EndCursor == "" {
			return fields, nil
		}
		after = pi.EndCursor
	}
}

// createProjectV2FieldMutation is the GraphQL mutation used by
// CreateProjectV2Field. It is a *create* mutation, never
// updateProjectV2Field, so the Status option-ID trap (ADR-1714 R4) cannot
// arise: no existing field is replaced. The payload is a union, so the id is
// selected through the ProjectV2FieldCommon interface fragment.
const createProjectV2FieldMutation = `
mutation($projectId: ID!, $name: String!, $dataType: ProjectV2CustomFieldType!) {
  createProjectV2Field(input: {projectId: $projectId, name: $name, dataType: $dataType}) {
    projectV2Field {
      ... on ProjectV2FieldCommon {
        id
      }
    }
  }
}`

// CreateProjectV2Field creates a new field named name on the project and
// returns its node ID. dataType must be FieldTypeText or FieldTypeDate — the
// only types Fabrik's display-only fields use (#2110). An empty returned ID
// is an error. Callers must have confirmed the field does not already exist;
// this method never replaces, renames or retypes anything.
func (c *Client) CreateProjectV2Field(projectID, name, dataType string) (string, error) {
	if dataType != FieldTypeText && dataType != FieldTypeDate {
		return "", fmt.Errorf("creating field %q: unsupported data type %q (want %s or %s)", name, dataType, FieldTypeText, FieldTypeDate)
	}
	vars := map[string]interface{}{
		"projectId": projectID,
		"name":      name,
		"dataType":  dataType,
	}

	var result struct {
		Data struct {
			CreateProjectV2Field struct {
				ProjectV2Field *struct {
					ID string `json:"id"`
				} `json:"projectV2Field"`
			} `json:"createProjectV2Field"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(createProjectV2FieldMutation, vars, &result); err != nil {
		return "", fmt.Errorf("creating %s field %q on project %s: %w", dataType, name, projectID, err)
	}
	f := result.Data.CreateProjectV2Field.ProjectV2Field
	if f == nil || f.ID == "" {
		return "", fmt.Errorf("creating %s field %q on project %s: no field ID returned", dataType, name, projectID)
	}
	return f.ID, nil
}
