package warnings

import (
	"fmt"
	"io"
	"strings"
)

// Level selects only the log prefix of a reported condition; the panel is flat.
type Level int

const (
	// LevelWarning is a condition that blocks or degrades work.
	LevelWarning Level = iota
	// LevelNotice is a condition that needs attention but does not degrade work
	// (e.g. a deprecation).
	LevelNotice
)

// Condition is one operator-level condition that belongs in the warnings panel
// (docs/state-machine.md §7.11): a configuration, environment or engine fault
// that blocks or degrades work and needs a person to act. Per-item states
// (paused, awaiting input, escalated) are not conditions.
type Condition struct {
	Type    string // e.g. "config_key"; Key is Type + ":" + Subject
	Subject string
	// KeyOverride replaces Type + ":" + Subject for a legacy entry whose key
	// does not have that shape. New conditions must not set it: ClearMissing
	// (and so Reconcile) only works on keys of the "<type>:<subject>" shape.
	KeyOverride string
	Level       Level
	Title       string
	Detail      string
	// FixAction is empty when there is no automatic fix; the panel then shows
	// the title and detail only.
	FixAction string
	FixParams map[string]string
	// Fingerprint is the stable identity for dismissal. Empty defaults to
	// Detail; conditions whose Detail carries volatile content (counts, PIDs)
	// must set an explicit stable value.
	Fingerprint string
	// LogLine overrides the log text (default: Title, then Detail).
	LogLine string
}

// Key returns the panel entry key.
func (c Condition) Key() string {
	if c.KeyOverride != "" {
		return c.KeyOverride
	}
	return c.Type + ":" + c.Subject
}

// Report is the one way to surface an operator-level condition: it writes the
// log line to w (nil = no log line) and records the panel entry in the same
// call, so a condition cannot reach only the log by accident. Safe to call
// every poll; an unchanged repeat does not rewrite the file.
func Report(w io.Writer, c Condition) error {
	if w != nil {
		prefix := "[startup] warning: "
		if c.Level == LevelNotice {
			prefix = "[startup] notice: "
		}
		line := c.LogLine
		if line == "" {
			line = c.Title
			if c.Detail != "" {
				line += ": " + strings.TrimSpace(c.Detail)
			}
		}
		fmt.Fprintf(w, "%s%s\n", prefix, line)
	}
	fp := c.Fingerprint
	if fp == "" {
		fp = c.Detail
	}
	if err := Record(Entry{
		Key:         c.Key(),
		Type:        c.Type,
		Title:       c.Title,
		Detail:      c.Detail,
		FixAction:   c.FixAction,
		FixParams:   c.FixParams,
		Fingerprint: fp,
	}); err != nil {
		return fmt.Errorf("recording warning %s: %w", c.Key(), err)
	}
	return nil
}

// Resolve clears the entry for key once its condition no longer holds. It does
// not write when the entry is absent.
func Resolve(key string) error {
	return Clear(key)
}

// Reconcile reports every condition in cs (all of Type typ) and then clears
// every other entry of that type. Call it ONLY when the check ran to completion:
// an empty cs legitimately clears the whole type, so a check that errored or
// returned early must not call it. Returns an error if any report failed; the
// sweep is skipped in that case so a partial recording cannot wipe entries.
func Reconcile(w io.Writer, typ string, cs []Condition) error {
	present := make(map[string]bool, len(cs))
	var firstErr error
	for _, c := range cs {
		c.Type = typ
		present[c.Subject] = true
		if err := Report(w, c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	_, err := ClearMissing(typ, present)
	return err
}
