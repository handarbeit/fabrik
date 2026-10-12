package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbePRFacts_Mapping(t *testing.T) {
	cases := []struct {
		name                  string
		state                 string
		draft, merged         bool
		mergeable, mergeState string
		wantKnown             bool
		wantState             string
		wantMerged            bool
		wantMergeable         string // "", "true", "false"
		wantMergeableState    string
	}{
		{"open clean", "OPEN", false, false, "MERGEABLE", "CLEAN", true, "open", false, "true", "clean"},
		{"open conflicting", "OPEN", true, false, "CONFLICTING", "DIRTY", true, "open", false, "false", "dirty"},
		{"open unknown never conflicting", "OPEN", false, false, "UNKNOWN", "UNKNOWN", true, "open", false, "", ""},
		{"merged", "MERGED", false, true, "UNKNOWN", "UNKNOWN", true, "closed", true, "", ""},
		{"closed", "CLOSED", false, false, "UNKNOWN", "UNKNOWN", true, "closed", false, "", ""},
		{"scalars absent", "", false, false, "", "", false, "", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := probePRFacts(tc.state, tc.draft, tc.merged, tc.mergeable, tc.mergeState)
			if f.Known != tc.wantKnown || f.State != tc.wantState || f.Merged != tc.wantMerged || f.MergeableState != tc.wantMergeableState || f.Draft != tc.draft && tc.wantKnown {
				t.Errorf("probePRFacts = %+v", f)
			}
			got := ""
			if f.Mergeable != nil {
				got = "false"
				if *f.Mergeable {
					got = "true"
				}
			}
			if got != tc.wantMergeable {
				t.Errorf("Mergeable = %q, want %q", got, tc.wantMergeable)
			}
		})
	}
}

func TestProbeProjectBoard_CarriesLinkedPRFacts(t *testing.T) {
	ts := "2024-01-01T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := probeItemResponse("PVTI_1", "I_abc", 10, "Issue", "OPEN", ts, ts, "owner/repo", 99, ts)
		pr := node["content"].(map[string]interface{})["closedByPullRequestsReferences"].(map[string]interface{})["nodes"].([]interface{})[0].(map[string]interface{})
		pr["headRefOid"] = "abc123"
		pr["state"] = "OPEN"
		pr["isDraft"] = true
		pr["merged"] = false
		pr["mergeable"] = "MERGEABLE"
		pr["mergeStateStatus"] = "UNSTABLE"
		json.NewEncoder(w).Encode(probeResponse("user", "PVT_123", []interface{}{node}, 1, false, ""))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil || len(items) != 1 {
		t.Fatalf("ProbeProjectBoard: items=%d err=%v", len(items), err)
	}
	f := items[0].LinkedPRFacts
	if !f.Known || f.State != "open" || !f.Draft || f.Mergeable == nil || !*f.Mergeable || f.MergeableState != "unstable" {
		t.Errorf("LinkedPRFacts = %+v", f)
	}
}
