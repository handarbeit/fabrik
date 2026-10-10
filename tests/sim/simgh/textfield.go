package simgh

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// This file models ProjectV2 text fields, the display-only status-line field
// in particular (#2048). Fabrik never creates one, so a scenario adds it with
// SeedTextField; a board without it is the "missing field" case.
//
// Fidelity: a text write sets the card's value, the value's own write time,
// and bumps both the card's and the project's updatedAt — the behaviour the
// repo's Status-move comments (#1090) imply for any project field write.
// Whether real GitHub bumps the item's updatedAt for a text write is not
// verified (see ADR 2048); modelling the bump is the conservative choice, as
// it is the case the client-side discount exists for. A write of the value
// the field already holds still counts as a write. A clear bumps the same way
// but leaves no value node behind, so it cannot be discounted.

// SeedTextField adds a TEXT field named name to the project. Idempotent.
func (s *Sim) SeedTextField(owner string, num int, name string) *Sim {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectKey(owner, num)]
	if !ok {
		s.fail("simgh: SeedTextField: project %s not seeded", projectKey(owner, num))
		return s
	}
	if p.textFields == nil {
		p.textFields = make(map[string]string)
	}
	if _, exists := p.textFields[name]; !exists {
		p.textFields[name] = "field:" + p.id + ":text:" + name
	}
	return s
}

// FetchTextField returns the project's TEXT field called name, or (nil, nil)
// when there is none.
func (s *Sim) FetchTextField(projectID, name string) (*gh.TextField, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return nil, err
	}
	id, ok := p.textFields[name]
	if !ok {
		return nil, nil
	}
	return &gh.TextField{ID: id, Name: name}, nil
}

// textFieldNameLocked reports whether fieldID is one of the project's text
// fields.
func (p *projectState) hasTextField(fieldID string) bool {
	for _, id := range p.textFields {
		if id == fieldID {
			return true
		}
	}
	return false
}

// UpdateProjectItemTextField sets a text field's value on a card.
func (s *Sim) UpdateProjectItemTextField(projectID, itemID, fieldID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, it, err := s.textWriteTargetLocked(projectID, itemID, fieldID)
	if err != nil {
		return err
	}
	if it.textValues == nil {
		it.textValues = make(map[string]string)
		it.textUpdatedAt = make(map[string]time.Time)
	}
	now := s.now()
	it.textValues[fieldID] = text
	it.textUpdatedAt[fieldID] = now
	it.updatedAt = now
	p.updatedAt = now
	return nil
}

// ClearProjectItemField clears a field's value on a card.
func (s *Sim) ClearProjectItemField(projectID, itemID, fieldID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, it, err := s.textWriteTargetLocked(projectID, itemID, fieldID)
	if err != nil {
		return err
	}
	now := s.now()
	delete(it.textValues, fieldID)
	// A cleared field has no value node, so the real board/probe query
	// (fieldValueByName) returns null and the client has no field updatedAt
	// to discount the card's bump against. Drop the write time to match: the
	// clear's bump then surfaces as ordinary card activity (#2048 review).
	delete(it.textUpdatedAt, fieldID)
	it.updatedAt = now
	p.updatedAt = now
	return nil
}

func (s *Sim) textWriteTargetLocked(projectID, itemID, fieldID string) (*projectState, *itemState, error) {
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return nil, nil, err
	}
	if !p.hasTextField(fieldID) {
		return nil, nil, fmt.Errorf("simgh: text field %q does not belong to project %q", fieldID, projectID)
	}
	it, ok := p.items[itemID]
	if !ok {
		return nil, nil, fmt.Errorf("simgh: project item %q not found in %q", itemID, projectID)
	}
	return p, it, nil
}

// TextFieldValue reads a card's text-field value on the default project, for
// scenario assertions. Returns "" when the field, the card or the value is
// absent.
func (s *Sim) TextFieldValue(ownerRepo string, number int, fieldName string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.defaultProject
	if p == nil {
		return ""
	}
	fieldID, ok := p.textFields[fieldName]
	if !ok {
		return ""
	}
	for _, id := range p.itemOrder {
		it, ok := p.items[id]
		if !ok || it.ownerRepo != ownerRepo || it.number != number {
			continue
		}
		return it.textValues[fieldID]
	}
	return ""
}
