package selfupgrade

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractBinaryFromTarball verifies that ExtractBinaryFromTarball correctly
// extracts the named binary from a .tar.gz archive and returns an executable
// temp file in the specified destination directory.
func TestExtractBinaryFromTarball(t *testing.T) {
	const binaryContent = "#!/bin/sh\necho hello\n"

	// Build a minimal tar.gz containing a file named "fabrik".
	tarball, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	tarballPath := tarball.Name()

	gw := gzip.NewWriter(tarball)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name:     "fabrik",
		Typeflag: tar.TypeReg,
		Size:     int64(len(binaryContent)),
		Mode:     0755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(binaryContent)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	outPath, err := ExtractBinaryFromTarball(tarballPath, destDir, "fabrik")
	if err != nil {
		t.Fatalf("ExtractBinaryFromTarball returned error: %v", err)
	}

	// Verify the output file is inside destDir.
	if filepath.Dir(outPath) != destDir {
		t.Errorf("output path %q is not inside destDir %q", outPath, destDir)
	}

	// Verify the content matches.
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if string(got) != binaryContent {
		t.Errorf("content = %q, want %q", got, binaryContent)
	}

	// Verify the file is executable.
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat output file: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("output file mode %o is not executable", info.Mode())
	}
}

// TestExtractBinaryFromTarball_NotFound verifies that ExtractBinaryFromTarball
// returns an error when no entry matching binaryName exists in the archive.
func TestExtractBinaryFromTarball_NotFound(t *testing.T) {
	tarball, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	tarballPath := tarball.Name()

	gw := gzip.NewWriter(tarball)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name:     "other-binary",
		Typeflag: tar.TypeReg,
		Size:     4,
		Mode:     0755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	tarball.Close()

	_, err = ExtractBinaryFromTarball(tarballPath, t.TempDir(), "fabrik")
	if err == nil {
		t.Error("expected error when fabrik binary not in tarball, got nil")
	}
}

// TestExtractBinaryFromTarball_CustomBinaryName verifies the caller-supplied
// binaryName parameter is respected — a differently-named daemon (e.g.
// "pruefer") must not match a "fabrik" entry or vice versa.
func TestExtractBinaryFromTarball_CustomBinaryName(t *testing.T) {
	const binaryContent = "pruefer binary content"

	tarball, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	tarballPath := tarball.Name()

	gw := gzip.NewWriter(tarball)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: "pruefer", Typeflag: tar.TypeReg, Size: int64(len(binaryContent)), Mode: 0755}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(binaryContent)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	tarball.Close()

	if _, err := ExtractBinaryFromTarball(tarballPath, t.TempDir(), "fabrik"); err == nil {
		t.Error("expected error matching binaryName=fabrik against a tarball containing only pruefer")
	}

	outPath, err := ExtractBinaryFromTarball(tarballPath, t.TempDir(), "pruefer")
	if err != nil {
		t.Fatalf("ExtractBinaryFromTarball(binaryName=pruefer) returned error: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if string(got) != binaryContent {
		t.Errorf("content = %q, want %q", got, binaryContent)
	}
}
