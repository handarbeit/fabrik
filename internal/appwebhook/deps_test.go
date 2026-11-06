package appwebhook

import (
	"os/exec"
	"strings"
	"testing"
)

const modulePrefix = "github.com/handarbeit/fabrik/"

// forbiddenDeps returns every dependency in deps that is the engine or Pruefer
// (or below either). The shared receiver is imported by both programs and must
// import neither (ADR 1113; FR-002, SC-002).
func forbiddenDeps(deps []string) []string {
	var bad []string
	for _, d := range deps {
		for _, root := range []string{modulePrefix + "engine", modulePrefix + "pruefer"} {
			if d == root || strings.HasPrefix(d, root+"/") {
				bad = append(bad, d)
			}
		}
	}
	return bad
}

// tsnetDeps returns every dependency in deps that is the Tailscale adapter
// sub-package or the Tailscale library. The core package must reach neither:
// that is what lets a consumer that only wants `direct` (Pruefer) avoid linking
// the embedded node (ADR 2475).
func tsnetDeps(deps []string) []string {
	var bad []string
	for _, d := range deps {
		if d == modulePrefix+"internal/appwebhook/tsnetnode" || d == "tailscale.com" || strings.HasPrefix(d, "tailscale.com/") {
			bad = append(bad, d)
		}
	}
	return bad
}

// The dependency graph of the package and of its tsnetnode sub-package must hold
// neither the engine nor Pruefer. It fails (never skips) when `go list` cannot
// run: a skipped guard would be a silently absent one.
func TestNoEngineOrPrueferDependencies(t *testing.T) {
	for _, pkg := range []string{modulePrefix + "internal/appwebhook", modulePrefix + "internal/appwebhook/tsnetnode"} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		deps := strings.Fields(string(out))
		if len(deps) == 0 {
			t.Fatalf("go list -deps %s returned nothing", pkg)
		}
		for _, bad := range forbiddenDeps(deps) {
			t.Errorf("%s depends on %s", pkg, bad)
		}
	}
}

// The guard can fail: a synthetic graph containing the engine or Pruefer is
// reported, a clean one is not (SC-002's deliberate-violation check).
func TestForbiddenDepsDetectsViolations(t *testing.T) {
	if got := forbiddenDeps([]string{"fmt", modulePrefix + "github", modulePrefix + "internal/localapi"}); len(got) != 0 {
		t.Fatalf("clean graph reported: %v", got)
	}
	for _, dep := range []string{modulePrefix + "engine", modulePrefix + "pruefer", modulePrefix + "pruefer/tui", modulePrefix + "engine/sub"} {
		if got := forbiddenDeps([]string{"fmt", dep}); len(got) != 1 || got[0] != dep {
			t.Errorf("violation %s not reported: %v", dep, got)
		}
	}
	// A look-alike name is not the engine.
	if got := forbiddenDeps([]string{modulePrefix + "engineering"}); len(got) != 0 {
		t.Errorf("look-alike reported: %v", got)
	}
}

// The core package never links the Tailscale node: neither the tsnetnode
// sub-package nor tailscale.com is in its dependency graph. The sub-package
// itself legitimately has both, so only the core is checked.
func TestCoreDoesNotLinkTailscale(t *testing.T) {
	core := modulePrefix + "internal/appwebhook"
	out, err := exec.Command("go", "list", "-deps", core).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", core, err)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatalf("go list -deps %s returned nothing", core)
	}
	for _, bad := range tsnetDeps(deps) {
		t.Errorf("%s depends on %s", core, bad)
	}
	// Non-vacuity: the sub-package does carry them, so the matcher can fail.
	sub := modulePrefix + "internal/appwebhook/tsnetnode"
	out, err = exec.Command("go", "list", "-deps", sub).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", sub, err)
	}
	if len(tsnetDeps(strings.Fields(string(out)))) == 0 {
		t.Fatalf("%s reports no tailscale dependency: the guard cannot fail", sub)
	}
}

func TestTsnetDepsDetectsViolations(t *testing.T) {
	if got := tsnetDeps([]string{"fmt", modulePrefix + "github"}); len(got) != 0 {
		t.Fatalf("clean graph reported: %v", got)
	}
	for _, dep := range []string{modulePrefix + "internal/appwebhook/tsnetnode", "tailscale.com/tsnet", "tailscale.com"} {
		if got := tsnetDeps([]string{dep}); len(got) != 1 {
			t.Errorf("violation %s not reported: %v", dep, got)
		}
	}
}
