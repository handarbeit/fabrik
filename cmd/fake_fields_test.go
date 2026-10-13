package cmd

import (
	"strings"
	"sync"
)

// fakeFieldBoard is the shared in-memory field list behind the display-field
// GraphQL routes of the repair-board and init --create-board test servers
// (#2110). A successful create appends to the list, so a second run sees the
// field as present. creates records every createProjectV2Field call; the test servers route any updateProjectV2Field to the Status field only.
type fakeFieldBoard struct {
	mu       sync.Mutex
	fields   []map[string]interface{}
	creates  []string // "name:TYPE" in call order
	failName string   // createProjectV2Field fails for this name
	emptyID  bool     // createProjectV2Field returns no ID
}

func newFakeFieldBoard(fields ...map[string]interface{}) *fakeFieldBoard {
	return &fakeFieldBoard{fields: fields}
}

func fakeField(id, name, dataType string) map[string]interface{} {
	return map[string]interface{}{"id": id, "name": name, "dataType": dataType}
}

// allDisplayFieldsPresent is the default board: every default display field
// exists with the right type, so tests that are not about display fields see
// a no-op field plan.
func allDisplayFieldsPresent() *fakeFieldBoard {
	return newFakeFieldBoard(
		fakeField("F1", "Fabrik", "TEXT"),
		fakeField("F2", "Last activity", "DATE"),
		fakeField("F3", "Last run", "TEXT"),
	)
}

func (f *fakeFieldBoard) createdNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.creates...)
}

// handle serves the two display-field operations. It reports false for any
// other query.
func (f *fakeFieldBoard) handle(query string, vars map[string]interface{}) (map[string]interface{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(query, "fields(first: 100"):
		nodes := make([]interface{}, 0, len(f.fields))
		for _, fl := range f.fields {
			nodes = append(nodes, fl)
		}
		return map[string]interface{}{"data": map[string]interface{}{"node": map[string]interface{}{
			"fields": map[string]interface{}{
				"nodes":    nodes,
				"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
			},
		}}}, true
	case strings.Contains(query, "createProjectV2Field(input:"):
		name, _ := vars["name"].(string)
		dataType, _ := vars["dataType"].(string)
		f.creates = append(f.creates, name+":"+dataType)
		if name == f.failName {
			return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "Name has already been taken"}}}, true
		}
		if f.emptyID {
			return map[string]interface{}{"data": map[string]interface{}{"createProjectV2Field": map[string]interface{}{"projectV2Field": map[string]interface{}{}}}}, true
		}
		id := "NEW_" + name
		f.fields = append(f.fields, fakeField(id, name, dataType))
		return map[string]interface{}{"data": map[string]interface{}{"createProjectV2Field": map[string]interface{}{"projectV2Field": map[string]interface{}{"id": id}}}}, true
	}
	return nil, false
}
