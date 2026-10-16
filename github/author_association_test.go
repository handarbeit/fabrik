package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// assocCases are the three wire shapes the author_association field can take.
// An absent or null field must decode to "" (unknown), which Pruefer treats as
// an outside author (#2308).
var assocCases = []struct {
	name   string
	fields map[string]interface{}
	want   string
}{
	{"present", map[string]interface{}{"author_association": "MEMBER"}, "MEMBER"},
	{"absent", map[string]interface{}{}, ""},
	{"null", map[string]interface{}{"author_association": nil}, ""},
}

func TestFetchPRDetails_AuthorAssociation(t *testing.T) {
	for _, tc := range assocCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := map[string]interface{}{
					"number": 7, "state": "open",
					"user": map[string]string{"login": "alice"},
					"head": map[string]string{"sha": "abc", "ref": "b"},
					"base": map[string]string{"ref": "main"},
				}
				for k, v := range tc.fields {
					body[k] = v
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()
			pr, err := NewClientWithBaseURL("token", srv.URL).FetchPRDetails("o", "r", 7)
			if err != nil {
				t.Fatalf("FetchPRDetails: %v", err)
			}
			if pr.AuthorAssociation != tc.want {
				t.Errorf("AuthorAssociation = %q, want %q", pr.AuthorAssociation, tc.want)
			}
		})
	}
}

func TestListOpenPRs_AuthorAssociation(t *testing.T) {
	for _, tc := range assocCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := map[string]interface{}{
					"number": 1, "state": "open",
					"user": map[string]string{"login": "alice"},
					"head": map[string]string{"sha": "s", "ref": "b"},
					"base": map[string]string{"ref": "main"},
				}
				for k, v := range tc.fields {
					body[k] = v
				}
				json.NewEncoder(w).Encode([]interface{}{body})
			}))
			defer srv.Close()
			prs, err := NewClientWithBaseURL("token", srv.URL).ListOpenPRs("o", "r")
			if err != nil {
				t.Fatalf("ListOpenPRs: %v", err)
			}
			if len(prs) != 1 || prs[0].AuthorAssociation != tc.want {
				t.Errorf("prs = %+v, want AuthorAssociation %q", prs, tc.want)
			}
		})
	}
}
