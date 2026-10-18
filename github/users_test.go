package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchUserID_Success_EscapesBotLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is the decoded form; EscapedPath keeps the wire form.
		if r.URL.Path != "/users/my-app[bot]" {
			t.Errorf("path = %s, want /users/my-app[bot]", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 123456, "login": "my-app[bot]"})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, err := c.FetchUserID("my-app[bot]")
	if err != nil {
		t.Fatalf("FetchUserID: %v", err)
	}
	if id != 123456 {
		t.Errorf("id = %d, want 123456", id)
	}
}

func TestFetchUserID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchUserID("nobody[bot]"); err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestFetchUserID_EmptyLoginAndMissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"x"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchUserID(""); err == nil {
		t.Error("expected error for empty login")
	}
	if _, err := c.FetchUserID("x[bot]"); err == nil {
		t.Error("expected error when the response carries no id")
	}
}

func TestFetchProjectItem_FillsAuthor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"number":7,"title":"t","state":"open","node_id":"I_7","user":{"login":"my-app[bot]"},"assignees":[{"login":"alice"}]}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	pi, err := c.FetchProjectItem("o", "r", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if pi.Author != "my-app[bot]" {
		t.Errorf("Author = %q", pi.Author)
	}
	if len(pi.Assignees) != 1 || pi.Assignees[0] != "alice" {
		t.Errorf("Assignees = %v", pi.Assignees)
	}
}

// TestFetchProjectItem_IsPR: the REST issues endpoint serves pull requests too;
// the pull_request key marks them (#2385).
func TestFetchProjectItem_IsPR(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"issue", `{"number":7,"state":"open","node_id":"I_7"}`, false},
		{"null pull_request", `{"number":7,"state":"open","node_id":"I_7","pull_request":null}`, false},
		{"pull request", `{"number":7,"state":"open","node_id":"PR_7","pull_request":{"url":"u"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClientWithBaseURL("token", srv.URL)
			pi, err := c.FetchProjectItem("o", "r", 7)
			if err != nil {
				t.Fatalf("FetchProjectItem: %v", err)
			}
			if pi.IsPR != tc.want {
				t.Errorf("IsPR = %v, want %v", pi.IsPR, tc.want)
			}
		})
	}
}
