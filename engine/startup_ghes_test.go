package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// ghesMetaServer returns an httptest server that serves installed_version at
// /meta, mirroring GHES's public, unauthenticated /meta REST endpoint.
func ghesMetaServer(t *testing.T, version string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{"installed_version": version})
	}))
}

// TestCheckGHESVersionFloorAgainst_AtFloor_Succeeds covers the "succeeds at
// or above it" half of the Acceptance criterion for FR-7.
func TestCheckGHESVersionFloorAgainst_AtFloor_Succeeds(t *testing.T) {
	srv := ghesMetaServer(t, "3.19.0")
	defer srv.Close()
	client := gh.NewClientWithBaseURL("tok", srv.URL)

	var warned []string
	err := checkGHESVersionFloorAgainst(client, "github.example.com", func(format string, args ...any) {
		warned = append(warned, format)
	})
	if err != nil {
		t.Errorf("checkGHESVersionFloorAgainst at floor = %v, want nil", err)
	}
	if len(warned) != 0 {
		t.Errorf("unexpected warnings at floor: %v", warned)
	}
}

// TestCheckGHESVersionFloorAgainst_AboveFloor_Succeeds covers a version above
// the floor, including a higher minor and a higher major.
func TestCheckGHESVersionFloorAgainst_AboveFloor_Succeeds(t *testing.T) {
	for _, version := range []string{"3.19.8", "3.20.0", "4.0.0"} {
		t.Run(version, func(t *testing.T) {
			srv := ghesMetaServer(t, version)
			defer srv.Close()
			client := gh.NewClientWithBaseURL("tok", srv.URL)

			err := checkGHESVersionFloorAgainst(client, "github.example.com", func(string, ...any) {})
			if err != nil {
				t.Errorf("checkGHESVersionFloorAgainst(%q) = %v, want nil", version, err)
			}
		})
	}
}

// TestCheckGHESVersionFloorAgainst_BelowFloor_FailsNamingBothVersions covers
// the "fails, naming both versions" half of the Acceptance criterion.
func TestCheckGHESVersionFloorAgainst_BelowFloor_FailsNamingBothVersions(t *testing.T) {
	srv := ghesMetaServer(t, "3.18.5")
	defer srv.Close()
	client := gh.NewClientWithBaseURL("tok", srv.URL)

	err := checkGHESVersionFloorAgainst(client, "github.example.com", func(string, ...any) {})
	if err == nil {
		t.Fatal("expected error for below-floor version, got nil")
	}
	if !strings.Contains(err.Error(), "3.18.5") {
		t.Errorf("error %q does not name the detected version", err.Error())
	}
	if !strings.Contains(err.Error(), "3.19") {
		t.Errorf("error %q does not name the required version", err.Error())
	}
}

// TestCheckGHESVersionFloorAgainst_LowerMajor_Fails covers a major-version
// regression (e.g. a hypothetical 2.x instance), not just a low minor.
func TestCheckGHESVersionFloorAgainst_LowerMajor_Fails(t *testing.T) {
	srv := ghesMetaServer(t, "2.99.99")
	defer srv.Close()
	client := gh.NewClientWithBaseURL("tok", srv.URL)

	err := checkGHESVersionFloorAgainst(client, "github.example.com", func(string, ...any) {})
	if err == nil {
		t.Fatal("expected error for lower-major-version instance, got nil")
	}
}

// TestCheckGHESVersionFloorAgainst_FetchError_WarnsNonFatally covers the
// "warn-and-continue on fetch error" decision: a network/HTTP failure must
// not block startup, since connectivity problems already surface loudly
// elsewhere (the board fetch immediately after).
func TestCheckGHESVersionFloorAgainst_FetchError_WarnsNonFatally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	client := gh.NewClientWithBaseURL("tok", srv.URL)

	var warned bool
	err := checkGHESVersionFloorAgainst(client, "github.example.com", func(string, ...any) {
		warned = true
	})
	if err != nil {
		t.Errorf("expected nil (non-fatal) error for a fetch failure, got %v", err)
	}
	if !warned {
		t.Error("expected a warning to be logged for a fetch failure")
	}
}

// TestCheckGHESVersionFloorAgainst_UnparseableVersion_WarnsNonFatally covers
// an installed_version that doesn't parse as major.minor — also non-fatal.
func TestCheckGHESVersionFloorAgainst_UnparseableVersion_WarnsNonFatally(t *testing.T) {
	srv := ghesMetaServer(t, "not-a-version")
	defer srv.Close()
	client := gh.NewClientWithBaseURL("tok", srv.URL)

	var warned bool
	err := checkGHESVersionFloorAgainst(client, "github.example.com", func(string, ...any) {
		warned = true
	})
	if err != nil {
		t.Errorf("expected nil (non-fatal) error for an unparseable version, got %v", err)
	}
	if !warned {
		t.Error("expected a warning to be logged for an unparseable version")
	}
}

// TestParseMajorMinor covers the version-string parsing helper directly.
func TestParseMajorMinor(t *testing.T) {
	cases := []struct {
		version              string
		wantMajor, wantMinor int
		wantOK               bool
	}{
		{"3.19.8", 3, 19, true},
		{"3.19", 3, 19, true},
		{"4.0.0", 4, 0, true},
		{"", 0, 0, false},
		{"3", 0, 0, false},
		{"not-a-version", 0, 0, false},
		{"a.b.c", 0, 0, false},
	}
	for _, tc := range cases {
		major, minor, ok := parseMajorMinor(tc.version)
		if ok != tc.wantOK || major != tc.wantMajor || minor != tc.wantMinor {
			t.Errorf("parseMajorMinor(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.version, major, minor, ok, tc.wantMajor, tc.wantMinor, tc.wantOK)
		}
	}
}

// TestCheckGHESVersionFloor_NoOpWhenUnconfigured covers FR-7's scoping: the
// preflight only applies when a GHES host is configured — github.com's
// version is not in question (FR-2's no-regression requirement).
func TestCheckGHESVersionFloor_NoOpWhenUnconfigured(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.GHESHost = ""
	eng.hostClient = nil

	if err := eng.checkGHESVersionFloor(); err != nil {
		t.Errorf("checkGHESVersionFloor with no GHES host = %v, want nil", err)
	}
}

// TestCheckGHESVersionFloor_NilHostClient_NoOp covers the defensive guard:
// GHESHost configured but hostClient somehow nil (shouldn't happen outside
// unusual test construction — New() always sets it) must not panic.
func TestCheckGHESVersionFloor_NilHostClient_NoOp(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.GHESHost = "github.example.com"
	eng.hostClient = nil

	if err := eng.checkGHESVersionFloor(); err != nil {
		t.Errorf("checkGHESVersionFloor with nil hostClient = %v, want nil", err)
	}
}

// TestCheckGHESVersionFloor_Integration wires a real *gh.Client through
// Engine.hostClient and confirms the Engine-level method delegates correctly
// end to end (below-floor fails, at-floor succeeds).
func TestCheckGHESVersionFloor_Integration(t *testing.T) {
	t.Run("below floor fails", func(t *testing.T) {
		srv := ghesMetaServer(t, "3.10.0")
		defer srv.Close()
		eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
		eng.cfg.GHESHost = "github.example.com"
		eng.hostClient = gh.NewClientWithBaseURL("tok", srv.URL)

		if err := eng.checkGHESVersionFloor(); err == nil {
			t.Error("expected error for below-floor GHES instance")
		}
	})

	t.Run("at floor succeeds", func(t *testing.T) {
		srv := ghesMetaServer(t, "3.19.8")
		defer srv.Close()
		eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
		eng.cfg.GHESHost = "github.example.com"
		eng.hostClient = gh.NewClientWithBaseURL("tok", srv.URL)

		if err := eng.checkGHESVersionFloor(); err != nil {
			t.Errorf("expected nil for at-floor GHES instance, got %v", err)
		}
	})
}
