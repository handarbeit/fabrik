//go:build e2e

package e2e

import (
	"reflect"
	"testing"
	"time"
)

// Pure-function tests for the cold-cache base-member scenario's helpers. Their
// fixtures are the engine's own format strings (engine/poll.go, engine/merge_train.go)
// with the log prefix a real line carries, so an engine-side rewording is caught
// here as well as by the live scenario.

func TestMatchNotHydrated(t *testing.T) {
	// engine/poll.go: e.logf(item.Number, "merge-train", "cache entry for #%d not yet hydrated (no deep-fetch recorded) — excluding from batching this poll, will retry\n", …)
	const line = `2026-09-26T21:50:00Z [#42 merge-train] cache entry for #42 not yet hydrated (no deep-fetch recorded) — excluding from batching this poll, will retry`
	tests := []struct {
		name string
		line string
		n    int
		want bool
	}{
		{"match", line, 42, true},
		{"other member", line, 43, false},
		{"prefix of a longer number", line, 4, false},
		{"suffix of a longer number", `[#142 merge-train] cache entry for #142 not yet hydrated (no deep-fetch recorded) — excluding from batching this poll, will retry`, 42, false},
		{"different message", `[#42 merge-train] WorktreeManager not yet registered for acme/alpha — excluding #42 from batching this poll, will retry`, 42, false},
		{"reworded", `[#42 merge-train] cache entry for #42 not hydrated`, 42, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchNotHydrated(tt.line, tt.n); got != tt.want {
				t.Errorf("matchNotHydrated(_, %d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

func TestMatchRefusing(t *testing.T) {
	// engine/merge_train.go: e.logfRepo(repoKey, "merge-train", "REFUSING to open/reuse integration PR for %s: pinned base %q is the repository default but %d member(s) declare a contradicting base — leaving members in Queued: %s\n", …)
	const two = `2026-09-26T21:50:00Z [merge-train] REFUSING to open/reuse integration PR for acme/alpha: pinned base "main" is the repository default but 2 member(s) declare a contradicting base — leaving members in Queued: #11 (declares base:e2e-cold-base-1), #12 (declares base:e2e-cold-base-1)`
	const readFail = `[merge-train] REFUSING to open/reuse integration PR for acme/alpha: pinned base "main" is the repository default but 1 member(s) declare a contradicting base — leaving members in Queued: #12 (label read failed: boom)`
	tests := []struct {
		name    string
		line    string
		repo    string
		members []int
		want    bool
	}{
		{"first listed member", two, "acme/alpha", []int{11, 99}, true},
		{"later listed member", two, "acme/alpha", []int{99, 12}, true},
		{"label read failure entry", readFail, "acme/alpha", []int{12}, true},
		{"unrelated member", two, "acme/alpha", []int{13}, false},
		{"number only inside another entry", two, "acme/alpha", []int{1}, false},
		{"other repo", two, "acme/beta", []int{11}, false},
		{"no members", two, "acme/alpha", nil, false},
		{"different message", `[merge-train] landing complete for acme/alpha (integration PR #5, 2 members)`, "acme/alpha", []int{11}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchRefusing(tt.line, tt.repo, tt.members); got != tt.want {
				t.Errorf("matchRefusing(_, %q, %v) = %v, want %v", tt.repo, tt.members, got, tt.want)
			}
		})
	}
}

func TestTrainPRsNotOnBase(t *testing.T) {
	prs := []trainPR{
		{Number: 1, Base: "e2e-cold-base-1"},
		{Number: 2, Base: "main"},
		{Number: 3, Base: "e2e-cold-base-1"},
		{Number: 4, Base: ""},
	}
	var got []int
	for _, p := range trainPRsNotOnBase(prs, "e2e-cold-base-1") {
		got = append(got, p.Number)
	}
	if want := []int{2, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("trainPRsNotOnBase = %v, want %v", got, want)
	}
	if bad := trainPRsNotOnBase(nil, "x"); len(bad) != 0 {
		t.Errorf("trainPRsNotOnBase(nil) = %v, want none", bad)
	}
}

func TestIntersects(t *testing.T) {
	if !intersects([]int{1, 2}, []int{2, 3}) {
		t.Error("expected overlap on 2")
	}
	if intersects([]int{1}, []int{2}) || intersects(nil, []int{1}) || intersects([]int{1}, nil) {
		t.Error("unexpected overlap")
	}
}

func TestIsNotFoundOutput(t *testing.T) {
	for out, want := range map[string]bool{
		`{"message":"Not Found","documentation_url":"…","status":"404"}` + "\ngh: Not Found (HTTP 404)": true,
		"gh: Not Found (HTTP 404)":               true,
		"gh: Bad credentials (HTTP 401)":         false,
		"gh: API rate limit exceeded (HTTP 403)": false,
		"":                                       false,
	} {
		if got := isNotFoundOutput(out); got != want {
			t.Errorf("isNotFoundOutput(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestWebhooksEnabled(t *testing.T) {
	tests := []struct {
		name   string
		envVal string
		cfg    string
		want   bool
	}{
		{"nothing set", "", "", false},
		{"env true", "true", "", true},
		{"env 1", "1", "", true},
		{"env yes mixed case", "Yes", "", true},
		{"env quoted", `"true"`, "", true},
		{"env false beats config true", "false", "webhooks: true\n", false},
		{"env unset falls to config true", "", "owner: x\nwebhooks: true\n", true},
		{"config false", "", "webhooks: false\n", false},
		{"config trailing comment", "", "webhooks: true # on\n", true},
		{"indented key is not top-level", "", "nested:\n  webhooks: true\n", false},
		{"similarly named key", "", "webhooks_port: 9\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webhooksEnabled(tt.envVal, []byte(tt.cfg)); got != tt.want {
				t.Errorf("webhooksEnabled(%q, %q) = %v, want %v", tt.envVal, tt.cfg, got, tt.want)
			}
		})
	}
}

func TestParseTrainPRsDecodesBase(t *testing.T) {
	// The shape listTrainPRsSince's jq emits: base is .base.ref.
	const out = `[
	  {"number": 30, "state": "closed", "merged": false, "created_at": "2026-09-26T21:55:00Z", "base": "main", "body": "Closes #11\nCloses #12\n"},
	  {"number": 31, "state": "closed", "merged": true, "created_at": "2026-09-26T22:10:00Z", "base": "e2e-cold-base-1", "body": "Closes #12\nCloses #11\n"},
	  {"number": 32, "state": "open", "merged": false, "created_at": "2026-09-26T22:11:00Z", "base": "main", "body": "Closes #99\n"},
	  {"number": 33, "state": "closed", "merged": true, "created_at": "2026-09-01T00:00:00Z", "base": "main", "body": "Closes #11\n"}
	]`
	since := time.Date(2026, 9, 26, 21, 0, 0, 0, time.UTC)
	prs, err := parseTrainPRs(out, since, []int{11, 12})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, p := range prs {
		got[p.Number] = p.Base
	}
	// #32 carries no member; #33 predates the window.
	want := map[int]string{30: "main", 31: "e2e-cold-base-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded bases = %v, want %v", got, want)
	}
	bad := trainPRsNotOnBase(prs, "e2e-cold-base-1")
	if len(bad) != 1 || bad[0].Number != 30 {
		t.Errorf("trainPRsNotOnBase = %+v, want only #30 (the default-base PR)", bad)
	}
	if _, err := parseTrainPRs("not json", since, nil); err == nil {
		t.Error("expected a parse error for malformed JSON")
	}
}
