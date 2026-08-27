package github

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchFileAtRef_DecodesFileContent(t *testing.T) {
	want := []byte("excluded_paths:\n  - vendor/**\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, wantPath := r.URL.Path, "/repos/owner/repo/contents/.pruefer/config.yaml"; got != wantPath {
			t.Errorf("request path = %q, want %q", got, wantPath)
		}
		if got, want := r.URL.Query().Get("ref"), "abc123"; got != want {
			t.Errorf("ref query param = %q, want %q", got, want)
		}
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q,"size":%d}`,
			base64.StdEncoding.EncodeToString(want), len(want))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	got, err := c.FetchFileAtRef("owner", "repo", ".pruefer/config.yaml", "abc123")
	if err != nil {
		t.Fatalf("FetchFileAtRef: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestFetchFileAtRef_NewlineWrappedBase64(t *testing.T) {
	want := []byte("hello world")
	encoded := base64.StdEncoding.EncodeToString(want)
	// Simulate GitHub's line-wrapped base64 content by inserting a newline
	// midway through — the decode must tolerate this exactly as the real
	// API's output does.
	wrapped := encoded[:4] + "\n" + encoded[4:]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q,"size":%d}`, wrapped, len(want))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	got, err := c.FetchFileAtRef("owner", "repo", ".pruefer/config.yaml", "main")
	if err != nil {
		t.Fatalf("FetchFileAtRef: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestFetchFileAtRef_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchFileAtRef("owner", "repo", ".pruefer/config.yaml", "main")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestFetchFileAtRef_RejectsDirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"type":"dir","encoding":"","content":"","size":0}`)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchFileAtRef("owner", "repo", ".pruefer", "main")
	if err == nil {
		t.Fatal("expected an error for a directory response, got nil")
	}
}

func TestFetchFileAtRef_RejectsUnsupportedEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"type":"file","encoding":"none","content":"raw text","size":8}`)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchFileAtRef("owner", "repo", ".pruefer/config.yaml", "main")
	if err == nil {
		t.Fatal("expected an error for an unsupported encoding, got nil")
	}
}
