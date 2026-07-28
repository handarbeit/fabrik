package selfupgrade

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ExtractBinaryFromTarball extracts the binaryName entry from a .tar.gz
// archive at tarballPath and writes it to a temp file in destDir. Returns the
// path to the temp file. The caller is responsible for renaming or removing
// it.
func ExtractBinaryFromTarball(tarballPath, destDir, binaryName string) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", fmt.Errorf("opening tarball: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("creating gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Match the binary by base name in case GoReleaser puts it in a subdir.
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		tmp, err := os.CreateTemp(destDir, binaryName+"-*")
		if err != nil {
			return "", fmt.Errorf("creating temp file: %w", err)
		}
		if err := tmp.Chmod(0755); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("chmod temp file: %w", err)
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("writing temp file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("closing temp file: %w", err)
		}
		return tmp.Name(), nil
	}
	return "", fmt.Errorf("%s binary not found in tarball", binaryName)
}
