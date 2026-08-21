package githubauth

import (
	"os/exec"
	"runtime"
)

// openBrowser is a package-level var (not a plain function) so tests can
// replace it with a no-op that records the URL instead of actually
// launching a browser process — mirroring appHTTPClient's pattern in
// github/app.go of making an unavoidably side-effecting dependency
// mockable via a var.
var openBrowser = defaultOpenBrowser

// defaultOpenBrowser launches the OS's default handler for url. Hand-rolled
// per-OS exec.Command rather than a dependency: this is a one-shot,
// best-effort convenience (RunManifestFlow always prints the URL too, so a
// failure here is never fatal to the manifest flow), not worth a new
// third-party import for.
func defaultOpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the launcher process once it exits so it never lingers as a
	// zombie. Started, not awaited, on purpose: the launcher (open/xdg-open/
	// rundll32) hands off to the desktop environment and returns quickly —
	// this function must not block on the browser's own lifetime — so the
	// Wait runs in the background rather than inline.
	go func() { _ = cmd.Wait() }()
	return nil
}
