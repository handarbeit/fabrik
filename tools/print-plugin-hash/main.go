// print-plugin-hash prints the embedded plugin fingerprint for the current
// binary's embedded fabrik-workflows/ content. Run this at a tagged commit to
// record the known embedded hash for that release in KnownEmbeddedVersions.
//
// Usage: go run ./tools/print-plugin-hash/
package main

import (
	"fmt"

	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

func main() {
	fmt.Println(fabrikplugin.ComputeEmbeddedVersion())
}
