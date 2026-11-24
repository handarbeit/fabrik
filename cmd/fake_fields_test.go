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

	// Status option capability and population (#2616). The zero value is a
	// github.com-shaped server whose board has no Status-bearing item.
	ghes            bool   // the option input type has no id field (GHES 3.19.10)
	introspection   string // "" = answer per ghes; "denied" = GraphQL error; "null" = null __type
	statusPopulated bool   // some item holds a Status
	populationFails bool   // the population read errors
	introspections  int
	populationReads int
	statusMutations []map[string]interface{} // variables of every updateProjectV2Field
}

// optionInputFields is the introspected field list of the option input type.
func (f *fakeFieldBoard) optionInputFields() []string {
	if f.ghes {
		return []string{"color", "description", "name"}
	}
	return []string{"color", "description", "id", "name"}
}

// statusMutationCount is how many option-replacing mutations were sent.
func (f *fakeFieldBoard) statusMutationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.statusMutations)
}

// recordStatusMutation records an updateProjectV2Field call. On a GHES-shaped
// board it returns the server's real validation error for an `id`, so a client
// that sends one fails exactly as it does against GHES 3.19.10.
func (f *fakeFieldBoard) recordStatusMutation(vars map[string]interface{}) map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusMutations = append(f.statusMutations, vars)
	if f.ghes {
		if opts, ok := vars["options"].([]interface{}); ok {
			for i, o := range opts {
				if m, ok := o.(map[string]interface{}); ok {
					if _, has := m["id"]; has {
						return map[string]interface{}{"errors": []interface{}{map[string]interface{}{
							"message": "Variable $options of type [ProjectV2SingleSelectFieldOptionInput!]! was provided invalid value for " +
								itoa(i) + ".id (Field is not defined on ProjectV2SingleSelectFieldOptionInput)",
						}}}
					}
				}
			}
		}
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
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
	case strings.Contains(query, "__type(name: \"ProjectV2SingleSelectFieldOptionInput\")"):
		f.introspections++
		switch f.introspection {
		case "denied":
			return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "introspection is disabled"}}}, true
		case "null":
			return map[string]interface{}{"data": map[string]interface{}{"__type": nil}}, true
		}
		names := f.optionInputFields()
		fields := make([]interface{}, 0, len(names))
		for _, n := range names {
			fields = append(fields, map[string]interface{}{"name": n})
		}
		return map[string]interface{}{"data": map[string]interface{}{"__type": map[string]interface{}{"inputFields": fields}}}, true
	case strings.Contains(query, "archivedStates: [ARCHIVED, NOT_ARCHIVED]"):
		f.populationReads++
		if f.populationFails {
			return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "boom"}}}, true
		}
		nodes := []interface{}{}
		if f.statusPopulated {
			nodes = append(nodes, map[string]interface{}{"id": "PVTI_1", "fieldValueByName": map[string]interface{}{"optionId": "OPT_1"}})
		}
		return map[string]interface{}{"data": map[string]interface{}{"node": map[string]interface{}{
			"items": map[string]interface{}{
				"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
				"nodes":    nodes,
			},
		}}}, true
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
