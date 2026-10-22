//go:build tailscale

package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The tagged build carries the real adapter. These tests touch no network: they
// check the build claims the node, the factory builds without starting
// anything, and Close is safe at every point of the lifecycle.
func TestTaggedBuild_ClaimsTheEmbeddedNode(t *testing.T) {
	if !tailscaleBuilt {
		t.Fatal("-tags tailscale must set tailscaleBuilt")
	}
}

func TestTsnetNode_CloseWithoutUpIsSafeAndIdempotent(t *testing.T) {
	n, err := newTailnetNode(tailnetNodeConfig{Hostname: "h", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestTsnetNode_UseAfterCloseIsRefused(t *testing.T) {
	n, err := newTailnetNode(tailnetNodeConfig{Hostname: "h", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_ = n.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := n.Up(ctx); !errors.Is(err, errTailnetClosed) {
		t.Fatalf("Up after Close = %v", err)
	}
	if _, err := n.ListenFunnel(":443"); !errors.Is(err, errTailnetClosed) {
		t.Fatalf("ListenFunnel after Close = %v", err)
	}
	if err := n.Watch(ctx, func(tailnetState) {}); !errors.Is(err, errTailnetClosed) {
		t.Fatalf("Watch after Close = %v", err)
	}
}
