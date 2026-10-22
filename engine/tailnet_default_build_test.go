//go:build !tailscale

package engine

import (
	"strings"
	"testing"
)

// AC3: a default build really is without the embedded node, and refuses
// event_source: tailscale at startup with the build hint (New() runs the
// refusal before anything else is constructed).
func TestDefaultBuild_RefusesTailscaleWithBuildHint(t *testing.T) {
	if tailscaleBuilt {
		t.Fatal("a build without -tags tailscale must not claim the embedded node")
	}
	cfg := Config{EventSource: EventSourceTailscale, TailscaleHostname: "h"}
	err := RefuseTailscaleMisconfigured(cfg, true, "s", "k", false)
	if err == nil || !strings.Contains(err.Error(), "-tags tailscale") {
		t.Fatalf("err = %v, want the build hint", err)
	}
	if _, err := New(Config{EventSource: EventSourceTailscale, TailscaleHostname: "h", Token: "tok", Owner: "o", Repo: "r", ProjectNum: 1}); err == nil ||
		!strings.Contains(err.Error(), "-tags tailscale") {
		t.Fatalf("New() err = %v, want the build hint", err)
	}
	if _, err := newTailnetNode(tailnetNodeConfig{}); err == nil {
		t.Fatal("the stub factory must refuse")
	}
}
