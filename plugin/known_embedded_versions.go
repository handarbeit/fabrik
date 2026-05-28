package plugin

// KnownEmbeddedVersions is the list of all plugin fingerprints ever legitimately
// written to .installed-version by a release binary. A fingerprint appears here
// only when it was computed from embedded content at release time.
//
// The list grows by one entry when the embedded plugin fingerprint changes between
// releases (via scripts/cut-release.sh; releases without plugin changes reuse the
// same hash and add no new entry). It is
// used by CheckPluginState to distinguish a legitimate installedVer (always an
// embedded hash) from one written by the buggy v0.0.64 migration (a customised
// disk hash that is not any known embedded hash).
//
// Historical bootstrapping:
//   v0.0.64–v0.0.65: 80638a6ed85feb0e1b1516886e13cc3c803eea4ebcf77a24d537a9c977ea4ef0
//   v0.0.66–current: 72e9c60393cc1c0015c6823fde643b83da246b13585cf17604c6c8636b77aefc
var KnownEmbeddedVersions = []string{
	"80638a6ed85feb0e1b1516886e13cc3c803eea4ebcf77a24d537a9c977ea4ef0", // v0.0.64–v0.0.65
	"72e9c60393cc1c0015c6823fde643b83da246b13585cf17604c6c8636b77aefc", // v0.0.66–current
	"5ae111702183b79c8a65b73df120d0f84e4fb84e5fcc225706adf169db5d74cd", // v0.0.68
}
