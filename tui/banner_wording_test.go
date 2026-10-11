package tui

import (
	"strings"
	"testing"
	"time"
)

// TestUsageLimitBannerWording pins the wording introduced by #2129: the
// account-wide Claude hold reads "usage limit hit — dispatch held", never
// "suspended" (that word is reserved for the item state proposed in #2120).
func TestUsageLimitBannerWording(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	now := time.Now()
	next, _ := m.Update(ClaudeUsageLimitAlertEvent{UsageLimited: true, Reset: now.Add(time.Hour)})
	m = next.(Model)
	next, _ = m.Update(TickEvent{At: now})
	m = next.(Model)

	view := m.usageLimit.View(200)
	if !strings.Contains(view, "Claude usage limit hit — dispatch held.") {
		t.Errorf("usage-limit banner = %q", view)
	}
	if strings.Contains(strings.ToLower(view), "suspend") {
		t.Errorf("usage-limit banner still says suspended: %q", view)
	}
}

// TestBudgetBannerWordingHasNoSuspended covers the REST-only, combined and
// GraphQL-only budget banners (#2129).
func TestBudgetBannerWordingHasNoSuspended(t *testing.T) {
	now := time.Now()
	reset := now.Add(10 * time.Minute)
	cases := []struct {
		name    string
		events  []RateLimitAlertEvent
		want    string
		notWant string
	}{
		{"rest", []RateLimitAlertEvent{{Bucket: RateLimitBucketREST, Exhausted: true, Reset: reset}}, "polling paused for the rate limit", "GraphQL"},
		{"combined", []RateLimitAlertEvent{
			{Bucket: RateLimitBucketREST, Exhausted: true, Reset: reset},
			{Bucket: RateLimitBucketGraphQL, Exhausted: true, Reset: reset},
		}, "polling paused for the rate limit", ""},
		{"graphql", []RateLimitAlertEvent{{Bucket: RateLimitBucketGraphQL, Exhausted: true, Reset: reset}}, "polling backed off", "paused for the rate limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
			for _, ev := range tc.events {
				next, _ := m.Update(ev)
				m = next.(Model)
			}
			next, _ := m.Update(TickEvent{At: now})
			m = next.(Model)
			view := m.alert.View(200)
			if !strings.Contains(view, tc.want) {
				t.Errorf("banner = %q, want %q", view, tc.want)
			}
			if tc.notWant != "" && strings.Contains(view, tc.notWant) {
				t.Errorf("banner = %q, must not contain %q", view, tc.notWant)
			}
			if strings.Contains(strings.ToLower(view), "suspend") {
				t.Errorf("banner still says suspended: %q", view)
			}
		})
	}
}
