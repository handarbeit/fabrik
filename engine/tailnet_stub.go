//go:build !tailscale

package engine

import "errors"

// tailscaleBuiltIn is false in a default build: the embedded Tailscale node is
// compiled only under -tags tailscale (#2143, ADR 2143), so the default build's
// dependency graph is unchanged.
const tailscaleBuiltIn = false

// newTailnetNode is the default build's node factory. RefuseTailscaleMisconfigured
// stops startup before it is reached; the error is the backstop.
func newTailnetNode(tailnetNodeConfig) (tailnetNode, error) {
	return nil, errors.New("this binary was built without the embedded Tailscale node (rebuild with -tags tailscale)")
}
