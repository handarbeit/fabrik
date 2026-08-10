package pruefer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDevBuildDir(t *testing.T) {
	tests := []struct {
		name      string
		fabrikDir string
		want      string
	}{
		{name: "empty defaults to cwd", fabrikDir: "", want: filepath.Join(".", "cmd", "pruefer")},
		{name: "absolute checkout root", fabrikDir: "/home/user/dev/fabrik", want: filepath.Join("/home/user/dev/fabrik", "cmd", "pruefer")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := devBuildDir(tt.fabrikDir); got != tt.want {
				t.Errorf("devBuildDir(%q) = %q, want %q", tt.fabrikDir, got, tt.want)
			}
		})
	}
}

// setVersion sets the package-level Version var for the duration of the test
// and restores it on cleanup — Version is a shared package global so tests
// must not leave it mutated for later tests.
func setVersion(t *testing.T, v string) {
	t.Helper()
	orig := Version
	Version = v
	t.Cleanup(func() { Version = orig })
}

// TestCheckAndUpgrade_DevVersion_NotSourceCheckout_NoOp verifies the dev-build
// branch is taken for a "dev"-prefixed version, and that it's a safe no-op
// (no panic, no exec) when FabrikDir doesn't point at a real fabrik checkout
// — exercising the wiring without needing a real git repo or a subprocess.
func TestCheckAndUpgrade_DevVersion_NotSourceCheckout_NoOp(t *testing.T) {
	setVersion(t, "dev(abc1234)")
	d := &Daemon{FabrikDir: t.TempDir()}
	d.checkAndUpgrade()
}

// TestCheckAndUpgrade_ReleaseVersion_UpToDate verifies that a non-"dev"
// version takes the release-check branch, using a dedicated client pointed
// at a local test server (never Pruefer's per-owner App installation
// tokens). Returning the same tag as the running version means "up to
// date" — no download is attempted.
func TestCheckAndUpgrade_ReleaseVersion_UpToDate(t *testing.T) {
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = true
		wantPath := "/repos/" + fabrikOwner + "/" + fabrikRepo + "/releases/latest"
		if r.URL.Path != wantPath {
			t.Errorf("request path = %q, want %q", r.URL.Path, wantPath)
		}
		fmt.Fprintf(w, `{"tag_name": "v0.0.1", "assets": []}`)
	}))
	defer srv.Close()

	orig := releaseAPIBaseURL
	releaseAPIBaseURL = srv.URL
	t.Cleanup(func() { releaseAPIBaseURL = orig })

	setVersion(t, "v0.0.1")
	d := &Daemon{}
	d.checkAndUpgrade()

	if !fetched {
		t.Error("release-check server was not hit — release branch was not taken")
	}
}

// TestCheckAndUpgrade_ReleaseVersion_DownloadAttempted verifies that when a
// newer release with a matching platform asset is found, the binary name
// used to construct the asset filename is "pruefer" — not "fabrik" — and
// that the download is actually attempted (it then fails gracefully, since
// the test server returns a 500, so no extraction or exec occurs).
func TestCheckAndUpgrade_ReleaseVersion_DownloadAttempted(t *testing.T) {
	downloaded := false
	matchingAsset := fmt.Sprintf("pruefer_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/asset.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		downloaded = true
		http.Error(w, "test server error", http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [{"name": %q, "browser_download_url": %q}]}`,
			matchingAsset, srv.URL+"/asset.tar.gz")
	})

	orig := releaseAPIBaseURL
	releaseAPIBaseURL = srv.URL
	t.Cleanup(func() { releaseAPIBaseURL = orig })

	setVersion(t, "v0.0.1")
	d := &Daemon{}
	d.checkAndUpgrade()

	if !downloaded {
		t.Error("download server was not hit — asset name construction (BinaryName=pruefer) may be wrong")
	}
}
