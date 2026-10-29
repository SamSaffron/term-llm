package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxReleaseArchiveBytes  = 128 << 20
	maxReleaseChecksumBytes = 1 << 20
	maxReleaseBinaryBytes   = 256 << 20
	maxReleaseUnpackedBytes = 512 << 20
)

func validReleaseTag(tag string) bool {
	if len(tag) < 2 || tag[0] != 'v' || tag[1] < '0' || tag[1] > '9' {
		return false
	}
	for _, c := range tag[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.ContainsRune("._+-", c)) {
			return false
		}
	}
	return true
}

func releaseHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many release download redirects")
			}
			return requireHTTPS(req.URL)
		},
	}
}

func requireHTTPS(u *url.URL) error {
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("release downloads require HTTPS without URL credentials")
	}
	return nil
}

// The checksum is fetched from the same release, so this checks integrity, not
// publisher identity. It cannot protect against replacement of both assets.
func downloadVerifiedArchive(ctx context.Context, client *http.Client, releaseURL, assetName, tmpDir string) (string, error) {
	checksumPath := filepath.Join(tmpDir, "checksums.txt")
	if err := downloadFile(ctx, client, releaseURL+"/checksums.txt", checksumPath, maxReleaseChecksumBytes); err != nil {
		return "", fmt.Errorf("download release checksums: %w", err)
	}
	checksums, err := os.ReadFile(checksumPath)
	if err != nil {
		return "", fmt.Errorf("read release checksums: %w", err)
	}
	want, err := releaseChecksum(string(checksums), assetName)
	if err != nil {
		return "", err
	}
	archivePath := filepath.Join(tmpDir, assetName)
	if err := downloadFile(ctx, client, releaseURL+"/"+assetName, archivePath, maxReleaseArchiveBytes); err != nil {
		return "", fmt.Errorf("download release archive: %w", err)
	}
	if err := verifyArchiveChecksum(archivePath, want); err != nil {
		os.Remove(archivePath)
		return "", err
	}
	return archivePath, nil
}

func releaseChecksum(manifest, assetName string) ([]byte, error) {
	var checksum []byte
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSuffix(line, "\r")
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.TrimPrefix(fields[len(fields)-1], "*") != assetName {
			continue
		}
		if checksum != nil {
			return nil, fmt.Errorf("duplicate checksum for %s", assetName)
		}
		if len(line) < 66 || (line[64:66] != "  " && line[64:66] != " *") || line[66:] != assetName {
			return nil, fmt.Errorf("malformed checksum for %s", assetName)
		}
		var err error
		checksum, err = hex.DecodeString(line[:64])
		if err != nil || len(checksum) != sha256.Size {
			return nil, fmt.Errorf("invalid SHA256 checksum for %s", assetName)
		}
	}
	if checksum == nil {
		return nil, fmt.Errorf("checksum missing for %s", assetName)
	}
	return checksum, nil
}

func verifyArchiveChecksum(path string, want []byte) error {
	archive, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open release archive for hashing: %w", err)
	}
	defer archive.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, archive); err != nil {
		return fmt.Errorf("hash release archive: %w", err)
	}
	if !bytes.Equal(hash.Sum(nil), want) {
		return fmt.Errorf("release archive SHA256 checksum mismatch")
	}
	return nil
}

func downloadFile(ctx context.Context, client *http.Client, source, dest string, maxBytes int64) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	if err := requireHTTPS(req.URL); err != nil {
		return err
	}
	req.Header.Set("User-Agent", updateUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed (%d) for %s", resp.StatusCode, source)
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("download exceeds %d byte limit", maxBytes)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(dest)
		}
	}()
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("download release file: %w", err)
	}
	if n > maxBytes {
		return fmt.Errorf("download exceeds %d byte limit", maxBytes)
	}
	return nil
}
