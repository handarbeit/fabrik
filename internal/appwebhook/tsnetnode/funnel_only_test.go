package tsnetnode

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"tailscale.com/tsnet"
)

// The webhook listener is Funnel-only (#2553): without tsnet.FunnelOnly the
// node also listens on its tailnet address. No CI job runs the real tsnet node,
// so this pins both halves: the option set holds FunnelOnly, and the adapter's
// one ListenFunnel call passes that option set.
func TestTsnetFunnelListenerIsFunnelOnly(t *testing.T) {
	opts := tsnetFunnelOptions()
	found := false
	for _, o := range opts {
		if o == tsnet.FunnelOnly() {
			found = true
		}
	}
	if !found {
		t.Fatalf("tsnetFunnelOptions() = %#v; want it to include tsnet.FunnelOnly()", opts)
	}

	f, err := parser.ParseFile(token.NewFileSet(), "node.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls, passing := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ListenFunnel" {
			return true
		}
		if recv, ok := sel.X.(*ast.SelectorExpr); !ok || recv.Sel.Name != "srv" {
			return true // the appwebhook.Node method itself, not the tsnet call
		}
		calls++
		if call.Ellipsis.IsValid() && len(call.Args) == 3 {
			if inner, ok := call.Args[2].(*ast.CallExpr); ok {
				if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "tsnetFunnelOptions" {
					passing++
				}
			}
		}
		return true
	})
	if calls == 0 {
		t.Fatal("found no srv.ListenFunnel call in node.go")
	}
	if passing != calls {
		t.Fatalf("%d of %d srv.ListenFunnel calls pass tsnetFunnelOptions()...; every call must", passing, calls)
	}
}
