//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// engineSource reads a file under engine/ so the string constants and format
// strings this scenario scrapes can be pinned to the engine's own source (the
// mergetrain_batchcap_parse_test.go precedent): an engine-side rewording fails
// here, not silently in a 30-minute live run.
func engineSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "engine", name))
	if err != nil {
		t.Fatalf("reading engine/%s: %v", name, err)
	}
	return string(data)
}

func TestSelfRecognitionStringsMatchEngineSource(t *testing.T) {
	deps := engineSource(t, "dependencies.go")
	item := engineSource(t, "item.go")
	reinvoke := engineSource(t, "reinvoke.go")
	reviews := engineSource(t, "reviews.go")

	for _, tc := range []struct{ src, name, want string }{
		{deps, "blocked prefix", `const blockedCommentPrefix = "` + blockedCommentPrefix + `"`},
		{deps, "blocked body format", `"%s\n\n` + blockedCommentWaitingHeader + `%s"`},
		{deps, "blocked log line", `e.logf(item.Number, "blocked", "waiting for %s to close\n"`},
		{item, "awaiting-input skip line", `e.logf(item.Number, "skip", "awaiting-input: %d new comment(s), none human-authored — still waiting\n"`},
		{item, "paused skip line", `e.logf(item.Number, "skip", "paused: %d new comment(s), none human-authored — pause retained\n"`},
		{item, "unpause line", `e.logf(item.Number, "unpause", "user commented on paused issue — unpausing\n")`},
		{item, "unblock line", `e.logf(item.Number, "unblock", "user comment received — removing awaiting-input pause\n")`},
		{reinvoke, "reinvoke dispatch line", `e.logf(item.Number, opts.tag, "re-invoking stage %q via comment processing (comments: %s)\n"`},
		{reviews, "addressed marker", `<!-- fabrik:review-ids-addressed: ([0-9,]+) -->`},
	} {
		if !strings.Contains(tc.src, tc.want) {
			t.Errorf("%s: engine source no longer contains %q — update the e2e matcher", tc.name, tc.want)
		}
	}
	if !strings.Contains(reviews, "review-body:") {
		t.Error("engine/reviews.go no longer defines the review-body: synthetic comment id")
	}
}

func TestBlockedCommentDeps(t *testing.T) {
	body := func(list string) string {
		return blockedCommentPrefix + "\n\n" + blockedCommentWaitingHeader + list
	}
	tests := []struct {
		name   string
		body   string
		want   []string
		wantOK bool
	}{
		{"single", body("#12"), []string{"#12"}, true},
		{"several, sorted", body("#13, #12"), []string{"#12", "#13"}, true},
		{"cross repo", body("#12, acme/other#7"), []string{"#12", "acme/other#7"}, true},
		{"trailing whitespace", body("#12, #13\n"), []string{"#12", "#13"}, true},
		{"not a blocked comment", "Waiting for the following issues to close: #12", nil, false},
		{"prefix without list", blockedCommentPrefix, nil, false},
		{"empty list", body(""), nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := blockedCommentDeps(tc.body)
			if ok != tc.wantOK || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("blockedCommentDeps = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestSameDepSet(t *testing.T) {
	if !sameDepSet([]string{"#12", "#13"}, 13, 12) {
		t.Error("order-insensitive equal sets should match")
	}
	if sameDepSet([]string{"#12"}, 1) {
		t.Error(`"#12" must not match 1 — sets are compared whole, not by substring`)
	}
	if sameDepSet([]string{"#1", "#2"}, 1) {
		t.Error("a superset must not match")
	}
	if sameDepSet([]string{"#1"}, 1, 2) {
		t.Error("a subset must not match")
	}
	if sameDepSet([]string{"acme/other#1"}, 1) {
		t.Error("a cross-repo dependency is not the same-repo #1")
	}
}

func TestNoHumanSkipLine(t *testing.T) {
	const ts = "2026-09-26T21:40:00Z "
	tests := []struct {
		line string
		n    int
		want bool
	}{
		{ts + "[#7 skip] awaiting-input: 1 new comment(s), none human-authored — still waiting", 7, true},
		{ts + "[#7 skip] paused: 2 new comment(s), none human-authored — pause retained", 7, true},
		{ts + "[#17 skip] paused: 2 new comment(s), none human-authored — pause retained", 7, false},
		{ts + "[#7 skip] paused: 1 human comment(s) predate the pause — pause retained", 7, false},
		{ts + "[#7 unpause] user commented on paused issue — unpausing", 7, false},
	}
	for _, tc := range tests {
		if got := noHumanSkipLine(tc.line, tc.n); got != tc.want {
			t.Errorf("noHumanSkipLine(%q, %d) = %v, want %v", tc.line, tc.n, got, tc.want)
		}
	}
}

func TestResumeLine(t *testing.T) {
	const ts = "2026-09-26T21:40:00Z "
	for line, want := range map[string]bool{
		ts + "[#7 unpause] user commented on paused issue — unpausing":                  true,
		ts + "[#7 unblock] user comment received — removing awaiting-input pause":       true,
		ts + "[#17 unpause] user commented on paused issue — unpausing":                 false,
		ts + "[#7 skip] paused: 1 new comment(s), none human-authored — pause retained": false,
	} {
		if got := resumeLine(line, 7); got != want {
			t.Errorf("resumeLine(%q, 7) = %v, want %v", line, got, want)
		}
	}
}

func TestDispatchesReview(t *testing.T) {
	line := `2026-09-26T21:40:00Z [#7 review-reinvoke] re-invoking stage "Review" via comment processing (comments: review-body:1234|review-body:99)`
	if !dispatchesReview(line, 1234) || !dispatchesReview(line, 99) {
		t.Error("should find both dispatched review ids")
	}
	if dispatchesReview(line, 123) {
		t.Error("review-body:123 is a prefix of review-body:1234 and must not match")
	}
	if dispatchesReview(line, 9) {
		t.Error("review-body:9 is a prefix of review-body:99 and must not match")
	}
}

func TestReviewAddressedMarkerComment(t *testing.T) {
	body := reviewAddressedMarkerComment(4242)
	if !strings.HasPrefix(body, "🏭 **Fabrik") {
		t.Errorf("marker comment must carry the Fabrik prefix, got %q", body)
	}
	if !strings.Contains(body, "<!-- fabrik:review-ids-addressed: 4242 -->") {
		t.Errorf("marker comment lacks the addressed marker: %q", body)
	}
}

func TestParseComments(t *testing.T) {
	out := `{"id":1,"body":"a","login":"fabrik-bed[bot]","type":"Bot","eyes":0,"rocket":1}` + "\n\n" +
		`{"id":2,"body":"b\nc","login":"arbeithand","type":"User","eyes":1,"rocket":0}` + "\n"
	got, err := parseComments(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []restComment{
		{ID: 1, Body: "a", Login: "fabrik-bed[bot]", Type: "Bot", Rocket: 1},
		{ID: 2, Body: "b\nc", Login: "arbeithand", Type: "User", Eyes: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, err := parseComments("not json"); err == nil {
		t.Error("garbage should fail to parse")
	}
}

func TestDecodeJSONObjectToleratesSurroundingNoise(t *testing.T) {
	type resp struct {
		ID int `json:"id"`
	}
	cases := map[string]struct {
		in      string
		want    int
		wantErr bool
	}{
		"clean":          {in: `{"id": 7}` + "\n", want: 7},
		"leading noise":  {in: "A new release of gh is available\n{\"id\": 8}", want: 8},
		"trailing noise": {in: "{\"id\": 9}\nwarning: deprecated\n", want: 9},
		"no object":      {in: "boom", wantErr: true},
		"empty":          {in: "", wantErr: true},
	}
	for name, c := range cases {
		var r resp
		err := decodeJSONObject(c.in, &r)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", name, err, c.wantErr)
			continue
		}
		if !c.wantErr && r.ID != c.want {
			t.Errorf("%s: id = %d, want %d", name, r.ID, c.want)
		}
	}
}
