package githubauth

import "testing"

func TestOpenBrowser_VarIsReplaceable(t *testing.T) {
	old := openBrowser
	defer func() { openBrowser = old }()

	var gotURL string
	openBrowser = func(url string) error {
		gotURL = url
		return nil
	}

	if err := openBrowser("https://example.com/start"); err != nil {
		t.Fatalf("openBrowser: %v", err)
	}
	if gotURL != "https://example.com/start" {
		t.Errorf("gotURL = %q, want https://example.com/start", gotURL)
	}
}
