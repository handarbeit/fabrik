package boardcache

import (
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// linkedPRCarrierExempt lists the gh.ProjectItem LinkedPR* fields that
// copyDeepFieldsFromState deliberately does not round-trip, each with the
// reason. Any other LinkedPR* field a deep fetch carries must survive a
// cache hit (#2344 R3); a new field fails the enumeration test until it is
// carried or exempted here.
var linkedPRCarrierExempt = map[string]string{
	"LinkedPRNumberShallow": "listing-only number from the shallow board query; the deep copy carries LinkedPRNumber",
	"LinkedPRUpdatedAt":     "freshness evidence, recorded by the Store from the listing (applyLayerEvidence), not a deep field",
}

// fullDeepItem returns a ProjectItem with every non-exempt LinkedPR* field set
// to a distinctive non-zero value, found by reflection so a new field is
// included automatically.
func fullDeepItem(t *testing.T) gh.ProjectItem {
	t.Helper()
	pi := gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"bug"}, UpdatedAt: time.Unix(1000, 0)}
	v := reflect.ValueOf(&pi).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if !strings.HasPrefix(name, "LinkedPR") {
			continue
		}
		if _, ok := linkedPRCarrierExempt[name]; ok {
			continue
		}
		f := v.Field(i)
		switch f.Interface().(type) {
		case int:
			f.SetInt(7)
		case bool:
			f.SetBool(true)
		case string:
			f.SetString("abc123")
		case []gh.ReviewRequest:
			f.Set(reflect.ValueOf([]gh.ReviewRequest{{Login: "alice"}}))
		case []gh.PRReview:
			f.Set(reflect.ValueOf([]gh.PRReview{{DatabaseID: 11, Author: "bob", State: "APPROVED"}}))
		case []gh.Comment:
			f.Set(reflect.ValueOf([]gh.Comment{{ID: "T1", Body: "thread"}}))
		case *gh.MergeQueueEntry:
			f.Set(reflect.ValueOf(&gh.MergeQueueEntry{State: "QUEUED", Position: 2, EnqueuerLogin: "carol"}))
		case time.Time:
			f.Set(reflect.ValueOf(time.Unix(2000, 0)))
		default:
			t.Fatalf("gh.ProjectItem.%s has type %s the enumeration test cannot populate: extend fullDeepItem or exempt it", name, f.Type())
		}
	}
	return pi
}

// cachedStateFor stores pi's deep facts the way a live deep fetch leaves them.
func cachedStateFor(t *testing.T, pi gh.ProjectItem) (*itemstate.Store, itemstate.Snapshot) {
	t.Helper()
	st := itemstate.NewStore(nil)
	st.Apply(itemstate.ItemDeepFetched{Repo: pi.Repo, Number: pi.Number, FreshState: pi})
	if pi.LinkedPRHeadSHA != "" {
		st.Apply(itemstate.PRHeadSHAUpdated{Repo: pi.Repo, Number: pi.Number, LinkedPRNum: pi.LinkedPRNumber, SHA: pi.LinkedPRHeadSHA})
	}
	snap, err := st.Get(pi.Repo, pi.Number)
	if err != nil {
		t.Fatal(err)
	}
	return st, snap
}

// TestCopyDeepFields_CarriesEveryLinkedPRField enumerates gh.ProjectItem's
// LinkedPR* fields and checks a cache-served copy carries each one.
func TestCopyDeepFields_CarriesEveryLinkedPRField(t *testing.T) {
	want := fullDeepItem(t)
	_, snap := cachedStateFor(t, want)
	state := snap.State()

	got := gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo"}
	copyDeepFieldsFromState(&got, state)

	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := 0; i < wv.NumField(); i++ {
		name := wv.Type().Field(i).Name
		if !strings.HasPrefix(name, "LinkedPR") {
			continue
		}
		if _, ok := linkedPRCarrierExempt[name]; ok {
			continue
		}
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			t.Errorf("cache-hit copy drops %s: got %#v, want %#v", name, gv.Field(i).Interface(), wv.Field(i).Interface())
		}
	}
	if got.LinkedPRMergeQueueEntry == state.LinkedPR.MergeQueueEntry {
		t.Error("cache-hit copy shares the cached MergeQueueEntry pointer")
	}
}

// The listing the cache serves (snapshotToProjectItem) must carry the same facts.
func TestSnapshotToProjectItem_CarriesMergeQueueFacts(t *testing.T) {
	_, snap := cachedStateFor(t, fullDeepItem(t))
	pi := snapshotToProjectItem(snap)
	if !pi.LinkedPRIsInMergeQueue || !pi.LinkedPRIsMergeQueueEnabled || pi.LinkedPRMergeQueueEntry == nil || pi.LinkedPRMergeQueueEntry.Position != 2 {
		t.Errorf("listing item lost merge-queue facts: %+v", pi)
	}
}

// An item in GitHub's merge queue stays in it after a poll served from cache,
// both in the caller's working item (what prInMergeQueue reads) and in the
// Store once the engine applies the result.
func TestFetchItemDetails_CacheHitKeepsMergeQueueMembership(t *testing.T) {
	deep := fullDeepItem(t)
	st, _ := cachedStateFor(t, deep)
	mc := &mockClient{}
	c := NewCacheImpl(mc, st, nopLog)

	// A poll served from cache: the listing item carries none of the deep facts.
	listing := gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"bug"}, UpdatedAt: time.Unix(1000, 0)}
	if err := c.FetchItemDetails(&listing); err != nil {
		t.Fatal(err)
	}
	if mc.fetchItemDetailsCount != 0 {
		t.Fatalf("poll was not served from cache (%d fallback calls); the test would not exercise the hit path", mc.fetchItemDetailsCount)
	}
	if !listing.LinkedPRIsInMergeQueue || !listing.LinkedPRIsMergeQueueEnabled || listing.LinkedPRMergeQueueEntry == nil {
		t.Fatalf("working item lost merge-queue state on a cache hit: %+v", listing)
	}
	if listing.DeepFetchLive || !listing.DeepFetchStartedAt.IsZero() {
		t.Error("a cache hit must not be marked as a live read (DeepFetchLive / DeepFetchStartedAt)")
	}
	// The engine then applies the item as ItemDeepFetched.
	st.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: listing})
	snap, _ := st.Get("owner/repo", 1)
	lp := snap.State().LinkedPR
	if lp == nil || !lp.IsInMergeQueue || !lp.IsMergeQueueEnabled || lp.MergeQueueEntry == nil {
		t.Errorf("Store lost merge-queue state after a cache-served apply: %+v", lp)
	}
}
