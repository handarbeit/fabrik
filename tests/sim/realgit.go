package sim

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// R6 (#2210): on macOS /usr/bin/git is Apple's xcrun shim, which resolves the
// developer directory and then execs the real git on every call (about 10-13 ms
// against about 6 ms for the real binary, 200-run measurement on the release
// host). The sim starts real git thousands of times, so TestMain resolves the
// real binary once and puts its directory first on PATH. Doing it in TestMain
// covers every caller: the gate's pre-gate, cut-release.sh's race suite and a
// plain `go test`. SIM_REAL_GIT=off disables it (to measure the difference). Any failure leaves PATH unchanged; it does nothing off darwin.

// realGitDir returns the directory to prepend to PATH, and whether there is one.
// pathGit is the git found on PATH ("" when none); xcrunGit runs `xcrun -f git`;
// executable reports whether a path is an executable regular file. It is pure so
// the fallback cases are unit-testable.
func realGitDir(goos, pathGit string, xcrunGit func() (string, error), executable func(string) bool) (string, bool) {
	if goos != "darwin" {
		return "", false
	}
	out, err := xcrunGit()
	if err != nil {
		return "", false // no xcrun, or it failed
	}
	real := strings.TrimSpace(out)
	if real == "" || !filepath.IsAbs(real) || !executable(real) {
		return "", false
	}
	if pathGit != "" && filepath.Clean(pathGit) == filepath.Clean(real) {
		return "", false // PATH already resolves to the real git
	}
	return filepath.Dir(real), true
}

// preferRealGit applies realGitDir to the running process and returns the git it
// chose, or "" when PATH was left unchanged.
func preferRealGit() string {
	if os.Getenv("SIM_REAL_GIT") == "off" {
		return "" // opt-out, for measuring the shim's cost
	}
	pathGit, _ := exec.LookPath("git")
	dir, ok := realGitDir(runtime.GOOS, pathGit,
		func() (string, error) {
			b, err := exec.Command("xcrun", "-f", "git").Output()
			return string(b), err
		},
		func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
		})
	if !ok {
		return ""
	}
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		return ""
	}
	return filepath.Join(dir, "git")
}
