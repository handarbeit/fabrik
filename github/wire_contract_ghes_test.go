package github

// #2616 / ADR 2616: GHES 3.19.10's ProjectV2SingleSelectFieldOptionInput has
// no `id` field (name, color and description only), unlike github.com's. There
// is no recorded GHES schema in the repo, so the GHES shape is DERIVED from the
// vendored github.com SDL by removing that one field. The derivation asserts it
// found and removed the field, so a schema refresh that renames or moves it
// fails loudly instead of passing vacuously.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator"
)

// loadGHESShapedWireSchema returns the vendored schema with `id` removed from
// ProjectV2SingleSelectFieldOptionInput.
func loadGHESShapedWireSchema(t *testing.T) *ast.Schema {
	t.Helper()
	data, err := os.ReadFile(wireSchemaPath)
	if err != nil {
		t.Fatalf("reading vendored schema %s: %v", wireSchemaPath, err)
	}
	sdl := string(data)

	const header = "input ProjectV2SingleSelectFieldOptionInput"
	start := strings.Index(sdl, header)
	if start < 0 {
		t.Fatalf("vendored schema has no %q block — update the GHES derivation", header)
	}
	end := strings.Index(sdl[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of the %q block", header)
	}
	end += start + len("\n}\n")
	block := sdl[start:end]

	// The id field and its docstring: a """ ... """ block directly above
	// "  id: String".
	idField := regexp.MustCompile(`(?s)\n  """[^"]*"""\n  id: String\n`)
	if !idField.MatchString(block) {
		t.Fatalf("%q block no longer has the documented `id: String` field — update the GHES derivation:\n%s", header, block)
	}
	stripped := idField.ReplaceAllString(block, "\n")
	if strings.Contains(stripped, "id: String") {
		t.Fatalf("GHES derivation left an id field behind:\n%s", stripped)
	}

	ghes := sdl[:start] + stripped + sdl[end:]
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: "ghes-derived", Input: ghes})
	if err != nil {
		t.Fatalf("parsing GHES-derived schema: %v", err)
	}
	return schema
}

// statusOptionPayload renders options the way Client.setStatusFieldOptions does.
func statusOptionPayload(opts []StatusOptionInput) map[string]any {
	rendered := make([]any, 0, len(opts))
	for _, o := range opts {
		e := map[string]any{"name": o.Name, "color": o.Color, "description": o.Description}
		if o.ID != nil {
			e["id"] = *o.ID
		}
		rendered = append(rendered, e)
	}
	return map[string]any{"fieldId": "F1", "options": rendered}
}

func validateOptionPayload(t *testing.T, schema *ast.Schema, vars map[string]any) error {
	t.Helper()
	doc, errs := gqlparser.LoadQuery(schema, updateProjectV2FieldMutation)
	if errs != nil {
		t.Fatalf("updateProjectV2FieldMutation failed validation: %v", errs)
	}
	_, err := validator.VariableValues(schema, doc.Operations[0], vars)
	return err
}

// TestWireContract_StatusOptionInputShapes pins both input shapes (FR-011):
// the id-echo payload is valid on github.com and rejected by the GHES shape
// with the report's exact error, and the name-only payload is valid on both.
func TestWireContract_StatusOptionInputShapes(t *testing.T) {
	gh := loadWireSchema(t)
	ghes := loadGHESShapedWireSchema(t)

	id := "OPT_1"
	echo := statusOptionPayload([]StatusOptionInput{
		{ID: &id, Name: "Todo", Color: "GRAY", Description: "d"},
		{Name: "Queued", Color: "GRAY", Description: ""},
	})
	nameOnly := statusOptionPayload([]StatusOptionInput{
		{Name: "Todo", Color: "GRAY", Description: "d"},
		{Name: "Queued", Color: "GRAY", Description: ""},
	})

	if err := validateOptionPayload(t, gh, echo); err != nil {
		t.Errorf("id-echo payload must be valid on the github.com shape: %v", err)
	}
	err := validateOptionPayload(t, ghes, echo)
	if err == nil {
		t.Fatal("id-echo payload must be rejected by the GHES shape (no id field)")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("GHES rejection should name the id field: %v", err)
	}
	if err := validateOptionPayload(t, gh, nameOnly); err != nil {
		t.Errorf("name-only payload must be valid on the github.com shape: %v", err)
	}
	if err := validateOptionPayload(t, ghes, nameOnly); err != nil {
		t.Errorf("name-only payload must be valid on the GHES shape: %v", err)
	}
}

// TestWireContract_NewAdminQueriesValidateOnBothShapes: the introspection and
// population documents must be valid against either shape of the schema.
func TestWireContract_NewAdminQueriesValidateOnBothShapes(t *testing.T) {
	for name, schema := range map[string]*ast.Schema{
		"github.com": loadWireSchema(t),
		"ghes":       loadGHESShapedWireSchema(t),
	} {
		for _, q := range []string{fetchOptionInputFieldsQuery, fetchBoardStatusPopulationQuery} {
			if _, errs := gqlparser.LoadQuery(schema, q); errs != nil {
				t.Errorf("%s: document failed validation: %v\n%s", name, errs, q)
			}
		}
	}
}

// TestWireContract_PopulationQuerySelectsArchivedItems: the items connection
// defaults to NOT_ARCHIVED; the population check must ask for both states.
func TestWireContract_PopulationQuerySelectsArchivedItems(t *testing.T) {
	if !strings.Contains(fetchBoardStatusPopulationQuery, "archivedStates: [ARCHIVED, NOT_ARCHIVED]") {
		t.Fatal("population query must request archived items too (they still hold a Status)")
	}
}
