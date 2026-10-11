package channelevents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestEventTypeNamesAfterRename pins the wire strings of the account-wide
// Claude usage-limit events (#2129).
func TestEventTypeNamesAfterRename(t *testing.T) {
	if got := string(ClaudeUsageLimited); got != "claude-usage-limited" {
		t.Errorf("ClaudeUsageLimited = %q", got)
	}
	if got := string(ClaudeUsageLimitLifted); got != "claude-usage-limit-lifted" {
		t.Errorf("ClaudeUsageLimitLifted = %q", got)
	}
	for _, typ := range []EventType{ClaudeUsageLimited, ClaudeUsageLimitLifted} {
		info, ok := Lookup(typ)
		if !ok || !info.AccountWide {
			t.Errorf("%s: want a catalogued account-wide event, got %+v ok=%v", typ, info, ok)
		}
	}
}

// TestSubscribeRejectsLegacyEventTypes: the hub stays strict; legacy names are
// rewritten by NormalizeEvents before a request reaches it (#2168).
func TestSubscribeRejectsLegacyEventTypes(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	for old := range legacyEventTypes {
		_, err := h.Subscribe(Subscription{Subscriber: "X", Events: []EventType{old}})
		if err == nil || !strings.Contains(err.Error(), "unknown event type") {
			t.Errorf("Subscribe(%q) err = %v, want unknown event type", old, err)
		}
	}
}

// TestOpenMigratesLegacyEventTypes: a subscription and a held entry written by
// a pre-rename build still match / deliver under the new names after reload.
func TestOpenMigratesLegacyEventTypes(t *testing.T) {
	dir := t.TempDir()
	reg := `{"subscriptions":[{"id":"s1","subscriber":"X","events":["claude-limit-suspended","claude-limit-lifted","merged"]}]}`
	if err := os.WriteFile(filepath.Join(dir, "subscriptions.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	h0 := openHub(t, t.TempDir(), nil)
	held := filepath.Base(h0.heldPath("X"))
	q := `{"name":"X","next_seq":2,"dropped":1,"dropped_types":{"claude-limit-lifted":1},` +
		`"entries":[{"seq":1,"event":{"type":"claude-limit-lifted","content":"old"}}]}`
	if err := os.WriteFile(filepath.Join(dir, held), []byte(q), 0o600); err != nil {
		t.Fatal(err)
	}

	h := openHub(t, dir, func(o *Options) { o.Now = func() time.Time { return time.Now() } })
	subs := h.Subscriptions("X")
	if len(subs) != 1 || subs[0].Events[0] != ClaudeUsageLimited || subs[0].Events[1] != ClaudeUsageLimitLifted || subs[0].Events[2] != Merged {
		t.Fatalf("subscription not migrated: %+v", subs)
	}
	if !subs[0].Matches(Event{Type: ClaudeUsageLimited}) {
		t.Error("migrated subscription does not match the new event type")
	}
	s := newSink()
	h.Attach("X", s)
	var sawLifted bool
	for i := 0; i < 2; i++ {
		ev := s.wait(t)
		if ev.Type == ClaudeUsageLimitLifted {
			sawLifted = true
		}
		if strings.HasPrefix(string(ev.Type), "claude-limit-") {
			t.Errorf("legacy type delivered: %s", ev.Type)
		}
	}
	if !sawLifted {
		t.Error("held legacy claude-limit-lifted entry not delivered under its new name")
	}
}

func TestNormalizeEvents(t *testing.T) {
	const old, old2 = EventType("claude-limit-suspended"), EventType("claude-limit-lifted")
	tests := []struct {
		name     string
		in       []EventType
		kept     []EventType
		mapped   []EventMapping
		rejected []EventType
	}{
		{name: "empty", in: nil},
		{name: "current passes through", in: []EventType{ValidateSettled, Merged}, kept: []EventType{ValidateSettled, Merged}},
		{
			name:   "legacy mapped keeps order",
			in:     []EventType{old, ValidateSettled},
			kept:   []EventType{ClaudeUsageLimited, ValidateSettled},
			mapped: []EventMapping{{old, ClaudeUsageLimited}},
		},
		{
			name:   "legacy and its current equivalent collapse",
			in:     []EventType{old, ClaudeUsageLimited, old},
			kept:   []EventType{ClaudeUsageLimited},
			mapped: []EventMapping{{old, ClaudeUsageLimited}},
		},
		{
			name:   "both legacy names",
			in:     []EventType{old2, old},
			kept:   []EventType{ClaudeUsageLimitLifted, ClaudeUsageLimited},
			mapped: []EventMapping{{old2, ClaudeUsageLimitLifted}, {old, ClaudeUsageLimited}},
		},
		{
			name:     "unknown rejected once",
			in:       []EventType{"nope", Merged, "nope"},
			kept:     []EventType{Merged},
			rejected: []EventType{"nope"},
		},
		{
			name:     "synthetic rejected",
			in:       []EventType{Digest, EventsDropped, StreamSuperseded},
			rejected: []EventType{Digest, EventsDropped, StreamSuperseded},
		},
		{
			name:     "all unknown leaves nothing kept",
			in:       []EventType{"a", "b"},
			rejected: []EventType{"a", "b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kept, mapped, rejected := NormalizeEvents(tc.in)
			if !reflect.DeepEqual(kept, tc.kept) || !reflect.DeepEqual(mapped, tc.mapped) || !reflect.DeepEqual(rejected, tc.rejected) {
				t.Fatalf("NormalizeEvents(%v)\n got kept=%v mapped=%v rejected=%v\nwant kept=%v mapped=%v rejected=%v",
					tc.in, kept, mapped, rejected, tc.kept, tc.mapped, tc.rejected)
			}
		})
	}
}

// TestNormalizedLegacySubscriptionIsAccepted: what NormalizeEvents keeps always
// passes the strict Validate, whatever mix the request named.
func TestNormalizedLegacySubscriptionIsAccepted(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	kept, _, _ := NormalizeEvents([]EventType{"claude-limit-suspended", ValidateSettled})
	sub, err := h.Subscribe(Subscription{Subscriber: "X", Events: kept})
	if err != nil {
		t.Fatal(err)
	}
	if !sub.Matches(Event{Type: ClaudeUsageLimited}) || !sub.Matches(Event{Type: ValidateSettled, Repo: "o/r", Issue: 1}) {
		t.Fatalf("normalised subscription does not match: %+v", sub)
	}
}
