package update

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

func extractBinaryTo(archivePath string, dest *os.File) error {
	return extractBinaryWithLimits(archivePath, dest, maxReleaseBinaryBytes, maxReleaseUnpackedBytes)
}

// Only the root regular file is copied, never archive-controlled filesystem
// paths. Scan the rest as well to reject duplicates and bound decompression.
func extractBinaryWithLimits(archivePath string, dest *os.File, maxBinary, maxUnpacked int64) error {
	if _, err := dest.Seek(0, 0); err != nil {
		return err
	}
	if err := dest.Truncate(0); err != nil {
		return err
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
	if err := copyArchiveBinary(tar.NewReader(unpacked), dest, maxBinary); err != nil {
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

func copyArchiveBinary(tr *tar.Reader, dest io.Writer, maxBinary int64) error {
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read release tar: %w", err)
		}
		if header.Name != RepoName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate binary %s in archive", RepoName)
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
		return fmt.Errorf("binary %s not found in archive", RepoName)
	}
	return nil
}
