package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// --- Per-format parse tests ---

func TestParseNvmrc(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *toolchainDeclaration // nil means "not comparable"
	}{
		{"bare major", "24", &toolchainDeclaration{Tool: "node", Source: ".nvmrc", Declared: "24", req: versionReq{granularity: 1, major: 24}}},
		{"v-prefixed full version", "v20.11.0", &toolchainDeclaration{Tool: "node", Source: ".nvmrc", Declared: "v20.11.0", req: versionReq{granularity: 1, major: 20}}},
		{"trailing newline trimmed", "24\n", &toolchainDeclaration{Tool: "node", Source: ".nvmrc", Declared: "24", req: versionReq{granularity: 1, major: 24}}},
		{"lts alias not comparable", "lts/*", nil},
		{"named alias not comparable", "lts/iron", nil},
		{"system not comparable", "system", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte(tt.content), 0644); err != nil {
				t.Fatalf("write .nvmrc: %v", err)
			}
			got := parseNvmrc(dir)
			assertDeclarationEqual(t, got, tt.want)
		})
	}
}

func TestParseNvmrc_Missing(t *testing.T) {
	dir := t.TempDir()
	if got := parseNvmrc(dir); got != nil {
		t.Errorf("parseNvmrc() = %+v, want nil for a missing file", got)
	}
}

func TestParsePackageJSONEngines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *toolchainDeclaration
	}{
		{
			"exact version",
			`{"engines":{"node":"24.0.0"}}`,
			&toolchainDeclaration{Tool: "node", Source: "package.json (engines.node)", Declared: "24.0.0", req: versionReq{granularity: 1, major: 24}},
		},
		{
			"caret range",
			`{"engines":{"node":"^24.0.0"}}`,
			&toolchainDeclaration{Tool: "node", Source: "package.json (engines.node)", Declared: "^24.0.0", req: versionReq{op: "^", granularity: 1, major: 24}},
		},
		{
			"gte range",
			`{"engines":{"node":">=24"}}`,
			&toolchainDeclaration{Tool: "node", Source: "package.json (engines.node)", Declared: ">=24", req: versionReq{op: ">=", granularity: 1, major: 24}},
		},
		{
			"complex range not comparable",
			`{"engines":{"node":">=18 <21 || >=22"}}`,
			nil,
		},
		{
			"x-range not comparable",
			`{"engines":{"node":"24.x"}}`,
			nil,
		},
		{
			"no engines key",
			`{}`,
			nil,
		},
		{
			"no engines.node key",
			`{"engines":{"npm":"10.0.0"}}`,
			nil,
		},
		{
			"malformed json",
			`{not valid json`,
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(tt.content), 0644); err != nil {
				t.Fatalf("write package.json: %v", err)
			}
			got := parsePackageJSONEngines(dir)
			assertDeclarationEqual(t, got, tt.want)
		})
	}
}

func TestParsePackageJSONEngines_Missing(t *testing.T) {
	dir := t.TempDir()
	if got := parsePackageJSONEngines(dir); got != nil {
		t.Errorf("parsePackageJSONEngines() = %+v, want nil for a missing file", got)
	}
}

func TestParseGoMod(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *toolchainDeclaration
	}{
		{
			"toolchain directive",
			"module example.com/foo\n\ngo 1.21\n\ntoolchain go1.24.3\n",
			&toolchainDeclaration{Tool: "go", Source: "go.mod (toolchain)", Declared: "go1.24", req: versionReq{granularity: 2, major: 1, minor: 24}},
		},
		{
			"go directive only",
			"module example.com/foo\n\ngo 1.24.0\n",
			&toolchainDeclaration{Tool: "go", Source: "go.mod (go directive)", Declared: "1.24", req: versionReq{granularity: 2, major: 1, minor: 24}},
		},
		{
			"go directive without patch",
			"module example.com/foo\n\ngo 1.21\n",
			&toolchainDeclaration{Tool: "go", Source: "go.mod (go directive)", Declared: "1.21", req: versionReq{granularity: 2, major: 1, minor: 21}},
		},
		{
			"neither directive present",
			"module example.com/foo\n",
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(tt.content), 0644); err != nil {
				t.Fatalf("write go.mod: %v", err)
			}
			got := parseGoMod(dir)
			assertDeclarationEqual(t, got, tt.want)
		})
	}
}

func TestParseGoMod_Missing(t *testing.T) {
	dir := t.TempDir()
	if got := parseGoMod(dir); got != nil {
		t.Errorf("parseGoMod() = %+v, want nil for a missing file", got)
	}
}

func TestParseToolVersions(t *testing.T) {
	dir := t.TempDir()
	content := "# a comment\nnodejs 20.11.0\ngolang 1.24.3\nruby 3.2.0\nterraform 1.7.0\n"
	if err := os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte(content), 0644); err != nil {
		t.Fatalf("write .tool-versions: %v", err)
	}
	decls := parseToolVersions(dir)
	if len(decls) != 2 {
		t.Fatalf("parseToolVersions() returned %d declarations, want 2 (ruby/terraform ignored); got %+v", len(decls), decls)
	}
	want := map[string]toolchainDeclaration{
		"node": {Tool: "node", Source: ".tool-versions (nodejs)", Declared: "20.11.0", req: versionReq{granularity: 1, major: 20}},
		"go":   {Tool: "go", Source: ".tool-versions (golang)", Declared: "1.24.3", req: versionReq{granularity: 1, major: 1}},
	}
	for _, d := range decls {
		w, ok := want[d.Tool]
		if !ok {
			t.Fatalf("unexpected tool %q in declarations", d.Tool)
		}
		assertDeclarationEqual(t, d, &w)
	}
}

func TestParseToolVersions_UnparseableLineIgnored(t *testing.T) {
	dir := t.TempDir()
	content := "nodejs lts-iron\n"
	if err := os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte(content), 0644); err != nil {
		t.Fatalf("write .tool-versions: %v", err)
	}
	decls := parseToolVersions(dir)
	if len(decls) != 0 {
		t.Fatalf("parseToolVersions() = %+v, want none for an unparseable version token", decls)
	}
}

func TestParseToolVersions_Missing(t *testing.T) {
	dir := t.TempDir()
	if got := parseToolVersions(dir); got != nil {
		t.Errorf("parseToolVersions() = %+v, want nil for a missing file", got)
	}
}

func assertDeclarationEqual(t *testing.T, got, want *toolchainDeclaration) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("got %+v, want nil (not comparable)", got)
		}
		return
	}
	if got == nil {
		t.Fatalf("got nil, want %+v", want)
	}
	if got.Tool != want.Tool || got.Source != want.Source || got.Declared != want.Declared || got.req != want.req {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// --- Comparator tests ---

func TestVersionReq_Satisfies(t *testing.T) {
	tests := []struct {
		name string
		req  versionReq
		v    resolvedVersion
		want bool
	}{
		{"major exact match", versionReq{granularity: 1, major: 24}, resolvedVersion{major: 24, minor: 0}, true},
		{"major mismatch (the reported shape)", versionReq{granularity: 1, major: 24}, resolvedVersion{major: 20, minor: 20}, false},
		{"caret same major satisfied regardless of minor", versionReq{op: "^", granularity: 1, major: 24}, resolvedVersion{major: 24, minor: 9}, true},
		{"gte satisfied by a newer major", versionReq{op: ">=", granularity: 1, major: 24}, resolvedVersion{major: 25}, true},
		{"gte not satisfied by an older major", versionReq{op: ">=", granularity: 1, major: 24}, resolvedVersion{major: 20}, false},
		{"go.mod minimum satisfied exactly", versionReq{granularity: 2, major: 1, minor: 24}, resolvedVersion{major: 1, minor: 24}, true},
		{"go.mod minimum satisfied by newer minor", versionReq{granularity: 2, major: 1, minor: 21}, resolvedVersion{major: 1, minor: 24}, true},
		{"go.mod minimum not satisfied by older minor", versionReq{granularity: 2, major: 1, minor: 24}, resolvedVersion{major: 1, minor: 20}, false},
		{"go.mod minimum satisfied by newer major", versionReq{granularity: 2, major: 1, minor: 24}, resolvedVersion{major: 2, minor: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.req.satisfies(tt.v); got != tt.want {
				t.Errorf("satisfies() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Resolution + caching ---

// writeVersionShim writes an executable script named binName into dir that
// prints output (mimicking `node --version` / `go version`), and appends one
// line to counterFile on every invocation so tests can assert the process-
// lifetime cache prevents a repeat shell-out.
func writeVersionShim(t *testing.T, dir, binName, output, counterFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim scripts are not supported on windows")
	}
	script := fmt.Sprintf("#!/bin/sh\necho x >> %q\necho %q\n", counterFile, output)
	path := filepath.Join(dir, binName)
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write %s shim: %v", binName, err)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

func TestResolveToolVersion_NodeAndCache(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "calls")
	writeVersionShim(t, binDir, "node", "v20.20.2", counter)
	t.Setenv("PATH", binDir)

	v := resolveToolVersion(context.Background(), "node")
	if v.err != nil {
		t.Fatalf("resolveToolVersion() error = %v", v.err)
	}
	if v.major != 20 || v.minor != 20 {
		t.Errorf("resolved = %+v, want major=20 minor=20", v)
	}
	if calls := countLines(t, counter); calls != 1 {
		t.Fatalf("shim invoked %d time(s) after first resolve, want 1", calls)
	}

	// Second call must be served from the process-lifetime cache — no
	// additional shell-out (R4's "no added latency worth noticing").
	v2 := resolveToolVersion(context.Background(), "node")
	if v2 != v {
		t.Errorf("second resolveToolVersion() = %+v, want identical cached %+v", v2, v)
	}
	if calls := countLines(t, counter); calls != 1 {
		t.Errorf("shim invoked %d time(s) after second resolve, want still 1 (cache not used)", calls)
	}
}

func TestResolveToolVersion_GoBinary(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "calls")
	writeVersionShim(t, binDir, "go", "go version go1.24.3 darwin/arm64", counter)
	t.Setenv("PATH", binDir)

	v := resolveToolVersion(context.Background(), "go")
	if v.err != nil {
		t.Fatalf("resolveToolVersion() error = %v", v.err)
	}
	if v.major != 1 || v.minor != 24 {
		t.Errorf("resolved = %+v, want major=1 minor=24", v)
	}
}

func TestResolveToolVersion_MissingFromPath(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	// Empty PATH — node/go cannot resolve. This must be "cannot compare",
	// never a failure or a mismatch (see the issue's Risks).
	t.Setenv("PATH", t.TempDir())

	v := resolveToolVersion(context.Background(), "node")
	if v.err == nil {
		t.Fatalf("resolveToolVersion() = %+v, want an error for a binary absent from PATH", v)
	}
}

// --- End-to-end detection ---

func TestDetectToolchainDrift_ReportedShape(t *testing.T) {
	// Reproduces #1658's reported shape: .nvmrc declares 24, the daemon's
	// inherited PATH resolves node as v20.20.2.
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	writeVersionShim(t, binDir, "node", "v20.20.2", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("PATH", binDir)

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".nvmrc"), []byte("24"), 0644); err != nil {
		t.Fatalf("write .nvmrc: %v", err)
	}

	mismatches := detectToolchainDrift(context.Background(), workDir)
	if len(mismatches) != 1 {
		t.Fatalf("detectToolchainDrift() = %+v, want exactly 1 mismatch", mismatches)
	}
	m := mismatches[0]
	if m.Source != ".nvmrc" {
		t.Errorf("Source = %q, want %q", m.Source, ".nvmrc")
	}
	if m.Declared != "24" {
		t.Errorf("Declared = %q, want %q", m.Declared, "24")
	}
	if m.Resolved != "v20.20.2" {
		t.Errorf("Resolved = %q, want %q", m.Resolved, "v20.20.2")
	}
}

func TestDetectToolchainDrift_SatisfiedProducesNoMismatch(t *testing.T) {
	// R4: a satisfied declaration must produce no output at all.
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	writeVersionShim(t, binDir, "node", "v20.11.0", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("PATH", binDir)

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".nvmrc"), []byte("20"), 0644); err != nil {
		t.Fatalf("write .nvmrc: %v", err)
	}

	mismatches := detectToolchainDrift(context.Background(), workDir)
	if len(mismatches) != 0 {
		t.Fatalf("detectToolchainDrift() = %+v, want none for a satisfied declaration", mismatches)
	}
}

func TestDetectToolchainDrift_BinaryMissingIsNotAMismatch(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	t.Setenv("PATH", t.TempDir()) // node unresolvable

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".nvmrc"), []byte("24"), 0644); err != nil {
		t.Fatalf("write .nvmrc: %v", err)
	}

	mismatches := detectToolchainDrift(context.Background(), workDir)
	if len(mismatches) != 0 {
		t.Fatalf("detectToolchainDrift() = %+v, want none when the binary is absent from PATH entirely", mismatches)
	}
}

func TestDetectToolchainDrift_NoDeclarationFiles(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	writeVersionShim(t, binDir, "node", "v20.11.0", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("PATH", binDir)

	workDir := t.TempDir()
	mismatches := detectToolchainDrift(context.Background(), workDir)
	if len(mismatches) != 0 {
		t.Fatalf("detectToolchainDrift() = %+v, want none for a worktree with no declaration files", mismatches)
	}
}

// --- Engine label/comment lifecycle ---

func TestCheckToolchainDrift_LifecycleOncePerEpisode(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	writeVersionShim(t, binDir, "node", "v20.20.2", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("PATH", binDir)

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".nvmrc"), []byte("24"), 0644); err != nil {
		t.Fatalf("write .nvmrc: %v", err)
	}

	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo"}

	eng.checkToolchainDrift(context.Background(), item, workDir)

	client.mu.Lock()
	commentCalls := len(client.addCommentCalls)
	labelCalls := len(client.addLabelCalls)
	client.mu.Unlock()
	if commentCalls != 1 {
		t.Fatalf("addCommentCalls = %d, want 1 after first detection", commentCalls)
	}
	if labelCalls != 1 || client.addLabelCalls[0].labelName != toolchainStaleLabel {
		t.Fatalf("addLabelCalls = %+v, want exactly one add of %q", client.addLabelCalls, toolchainStaleLabel)
	}

	// Simulate the label now being present on a re-fetched item (R5: the
	// warning must not repeat while the mismatch persists unresolved).
	item.Labels = []string{toolchainStaleLabel}
	eng.checkToolchainDrift(context.Background(), item, workDir)

	client.mu.Lock()
	commentCalls = len(client.addCommentCalls)
	labelCalls = len(client.addLabelCalls)
	client.mu.Unlock()
	if commentCalls != 1 {
		t.Errorf("addCommentCalls = %d after second (still-mismatched) invocation, want still 1", commentCalls)
	}
	if labelCalls != 1 {
		t.Errorf("addLabelCalls = %d after second invocation, want still 1", labelCalls)
	}
}

func TestCheckToolchainDrift_SatisfiedProducesNoLabelOrComment(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	writeVersionShim(t, binDir, "node", "v20.11.0", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("PATH", binDir)

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".nvmrc"), []byte("20"), 0644); err != nil {
		t.Fatalf("write .nvmrc: %v", err)
	}

	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo"}

	eng.checkToolchainDrift(context.Background(), item, workDir)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 0 {
		t.Errorf("addCommentCalls = %+v, want none for a satisfied declaration", client.addCommentCalls)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("addLabelCalls = %+v, want none for a satisfied declaration", client.addLabelCalls)
	}
}

func TestCheckToolchainDrift_ClearsLabelOnceResolved(t *testing.T) {
	resetToolchainVersionCacheForTest()
	t.Cleanup(resetToolchainVersionCacheForTest)

	binDir := t.TempDir()
	writeVersionShim(t, binDir, "node", "v20.11.0", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("PATH", binDir)

	// No declaration file at all — the mismatch has "resolved" (e.g. the
	// worktree's .nvmrc was fixed since the label was applied).
	workDir := t.TempDir()

	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{toolchainStaleLabel}}

	eng.checkToolchainDrift(context.Background(), item, workDir)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.removeLabelCalls) != 1 || client.removeLabelCalls[0].labelName != toolchainStaleLabel {
		t.Fatalf("removeLabelCalls = %+v, want exactly one removal of %q", client.removeLabelCalls, toolchainStaleLabel)
	}
}
