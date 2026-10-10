package simgh

import (
	"fmt"
	"regexp"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// This file models ProjectV2 date fields, the display-only "Last activity"
// field in particular (#2049). Fabrik never creates one, so a scenario adds it
// with SeedDateField; a board without it is the "missing field" case.
//
// Fidelity: a date write is modelled exactly like a text write (see
// textfield.go): it sets the card's value, the value's own write time, and
// bumps both the card's and the project's updatedAt. Whether real GitHub
// bumps the item's updatedAt for a date write is not verified (ADR 2049); the
// bump is the conservative case the client-side discount exists for. The
// value must be a YYYY-MM-DD string — the Date scalar — or the write fails,
// as GitHub's would.

var dateValueRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// SeedDateField adds a DATE field named name to the project. Idempotent.
func (s *Sim) SeedDateField(owner string, num int, name string) *Sim {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectKey(owner, num)]
	if !ok {
		s.fail("simgh: SeedDateField: project %s not seeded", projectKey(owner, num))
		return s
	}
	if p.dateFields == nil {
		p.dateFields = make(map[string]string)
	}
	if _, exists := p.dateFields[name]; !exists {
		p.dateFields[name] = "field:" + p.id + ":date:" + name
	}
	return s
}

// FetchDateField returns the project's DATE field called name, or (nil, nil)
// when there is none (including a field of that name of another type).
func (s *Sim) FetchDateField(projectID, name string) (*gh.DateField, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return nil, err
	}
	id, ok := p.dateFields[name]
	if !ok {
		return nil, nil
	}
	return &gh.DateField{ID: id, Name: name}, nil
}

// UpdateProjectItemDateField sets a date field's value on a card.
func (s *Sim) UpdateProjectItemDateField(projectID, itemID, fieldID, date string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.projectByIDLocked(projectID)
	if err != nil {
		return err
	}
	isDate := false
	for _, id := range p.dateFields {
		if id == fieldID {
			isDate = true
		}
	}
	if !isDate {
		return fmt.Errorf("simgh: date field %q does not belong to project %q", fieldID, projectID)
	}
	if !dateValueRE.MatchString(date) {
		return fmt.Errorf("simgh: %q is not a valid Date (want YYYY-MM-DD)", date)
	}
	it, ok := p.items[itemID]
	if !ok {
		return fmt.Errorf("simgh: project item %q not found in %q", itemID, projectID)
	}
	if it.textValues == nil {
		it.textValues = make(map[string]string)
	}
	if it.textUpdatedAt == nil {
		it.textUpdatedAt = make(map[string]time.Time)
	}
	now := s.now()
	it.textValues[fieldID] = date
	it.textUpdatedAt[fieldID] = now
	it.updatedAt = now
	p.updatedAt = now
	return nil
}

// DateFieldValue reads a card's date-field value (YYYY-MM-DD) on the default
// project, for scenario assertions. Returns "" when the field, the card or
// the value is absent.
func (s *Sim) DateFieldValue(ownerRepo string, number int, fieldName string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.defaultProject
	if p == nil {
		return ""
	}
	fieldID, ok := p.dateFields[fieldName]
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
