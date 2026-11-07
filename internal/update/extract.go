package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
)

func extractBinaryTo(archivePath string, dest *os.File) error {
	return extractBinaryWithLimits(archivePath, dest, maxReleaseBinaryBytes, maxReleaseUnpackedBytes)
}

// Only the root regular file is copied, never archive-controlled filesystem
// paths. Scan the rest as well to reject duplicates and bound decompression.
func extractBinaryWithLimits(archivePath string, dest *os.File, maxBinary, maxUnpacked int64) error {
	return extractBinaryWithLimitsNamed(archivePath, dest, maxBinary, maxUnpacked, releaseBinaryName())
}

func extractBinaryWithLimitsNamed(archivePath string, dest *os.File, maxBinary, maxUnpacked int64, binaryName string) error {
	if _, err := dest.Seek(0, 0); err != nil {
		return err
	}
	if err := dest.Truncate(0); err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return extractZipBinary(archivePath, dest, binaryName, maxBinary, maxUnpacked)
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open release gzip: %w", err)
	}
	defer gz.Close()
	unpacked := &io.LimitedReader{R: gz, N: maxUnpacked + 1}
	if err := copyArchiveBinary(tar.NewReader(unpacked), dest, binaryName, maxBinary); err != nil {
		return err
	}
	// Read through the gzip trailer to detect truncation/CRC errors, including
	// data after the tar end marker. The same limit covers concatenated streams.
	if _, err := io.Copy(io.Discard, unpacked); err != nil {
		return fmt.Errorf("read release archive: %w", err)
	}
	if unpacked.N == 0 {
		return fmt.Errorf("unpacked archive exceeds %d byte limit", maxUnpacked)
	}
	return nil
}

func copyArchiveBinary(tr *tar.Reader, dest io.Writer, binaryName string, maxBinary int64) error {
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read release tar: %w", err)
		}
		if header.Name != binaryName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate binary %s in archive", binaryName)
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > maxBinary {
			return fmt.Errorf("release binary must be a nonempty regular file of at most %d bytes", maxBinary)
		}
		if _, err := io.Copy(dest, tr); err != nil {
			return fmt.Errorf("extract release binary: %w", err)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("binary %s not found in archive", binaryName)
	}
	return nil
}

// extractZipBinary copies the exact root-level executable member from a ZIP
// release archive. Only the named member is read; every header is counted
// against the unpacked limit and duplicate or oversized members are rejected.
func extractZipBinary(archivePath string, dest *os.File, binaryName string, maxBinary, maxUnpacked int64) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open release zip: %w", err)
	}
	defer zr.Close()
	var unpacked int64
	found := false
	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			continue
		}
		unpacked += int64(file.UncompressedSize64)
		if unpacked > maxUnpacked {
			return fmt.Errorf("unpacked archive exceeds %d byte limit", maxUnpacked)
		}
		if file.Name != binaryName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate binary %s in archive", binaryName)
		}
		if file.UncompressedSize64 <= 0 || int64(file.UncompressedSize64) > maxBinary {
			return fmt.Errorf("release binary must be a nonempty regular file of at most %d bytes", maxBinary)
		}
		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("open release binary: %w", err)
		}
		n, copyErr := io.Copy(dest, io.LimitReader(rc, maxBinary+1))
		closeErr := rc.Close()
		if copyErr != nil {
			return fmt.Errorf("extract release binary: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close release binary: %w", closeErr)
		}
		if n > maxBinary {
			return fmt.Errorf("release binary exceeds %d byte limit", maxBinary)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("binary %s not found in archive", binaryName)
	}
	return nil
}
