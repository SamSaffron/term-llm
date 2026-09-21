// Command builduisource writes the deterministic, private readable-source
// archive that backs UI authoring references. It runs from `make frontend`
// after the frontend bundles are generated.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/serveui"
)

// formatVersion must match the format uisource.Decode accepts.
const formatVersion = 1

const outputPath = "internal/uisource/assets/term-llm-ui-source.tar.gz"

// extraFiles are authoring references shipped alongside frontend sources.
var extraFiles = []string{
	"frontend/README.md",
	"internal/agents/builtin/extension-builder/system.md",
}

type fileEntry struct {
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

// manifest fields are declared in the order they are encoded, which keeps the
// generated JSON stable across builds.
type manifest struct {
	AssetVersion  string               `json:"asset_version"`
	Files         map[string]fileEntry `json:"files"`
	FormatVersion int                  `json:"format_version"`
	SourceDigest  string               `json:"source_digest"`
	Version       string               `json:"version"`
}

func main() {
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	files, err := collect(root)
	if err != nil {
		fatal(err)
	}
	archive, err := build(files, describe(root), serveui.AssetVersion())
	if err != nil {
		fatal(err)
	}
	out := filepath.Join(root, filepath.FromSlash(outputPath))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(out, archive, 0o644); err != nil {
		fatal(err)
	}
	// files does not yet include the manifest entry the archive adds.
	fmt.Printf("UI authoring source: %d files, %s compressed bytes\n", len(files)+1, group(len(archive)))
}

// repoRoot returns the module root containing this command.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// collect reads the readable authoring sources, keyed by repo-relative path.
func collect(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	srcDir := filepath.Join(root, "frontend", "src")
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !included(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collect frontend sources: %w", err)
	}
	for _, rel := range extraFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files[rel] = data
	}
	return files, nil
}

// included keeps authoring sources while dropping Hub sources and tests.
func included(rel string) bool {
	if strings.Contains(rel, "/hub/") || strings.Contains(rel, "/test/") || strings.Contains(rel, ".test.") {
		return false
	}
	switch filepath.Ext(rel) {
	case ".ts", ".tsx", ".css":
		return true
	}
	return false
}

// build returns the gzipped tar archive, including its source manifest.
func build(files map[string][]byte, version, assetVersion string) ([]byte, error) {
	entries := map[string]fileEntry{}
	sourceDigest := sha256.New()
	for _, rel := range sortedKeys(files) {
		data := files[rel]
		sum := sha256.Sum256(data)
		entries[rel] = fileEntry{SHA256: hex.EncodeToString(sum[:]), Size: len(data)}
		_, _ = sourceDigest.Write([]byte(rel))
		_, _ = sourceDigest.Write([]byte{0})
		_, _ = sourceDigest.Write(data)
		_, _ = sourceDigest.Write([]byte{0})
	}
	encoded, err := json.MarshalIndent(manifest{
		AssetVersion:  assetVersion,
		Files:         entries,
		FormatVersion: formatVersion,
		SourceDigest:  hex.EncodeToString(sourceDigest.Sum(nil)),
		Version:       version,
	}, "", "  ")
	if err != nil {
		return nil, err
	}

	archived := make(map[string][]byte, len(files)+1)
	for rel, data := range files {
		archived[rel] = data
	}
	archived["source-manifest.json"] = append(encoded, '\n')

	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	for _, rel := range sortedKeys(archived) {
		data := archived[rel]
		// Fixed metadata and a zero modification time keep the archive
		// byte-identical for identical inputs.
		if err := tw.WriteHeader(&tar.Header{
			Name:     rel,
			Size:     int64(len(data)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}); err != nil {
			return nil, fmt.Errorf("archive %s: %w", rel, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("archive %s: %w", rel, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}

	var compressed bytes.Buffer
	zw, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(body.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

// describe returns the git description used as the archive version.
func describe(root string) string {
	cmd := exec.Command("git", "describe", "--tags", "--always")
	cmd.Dir = root
	out, err := cmd.Output()
	if version := strings.TrimSpace(string(out)); err == nil && version != "" {
		return version
	}
	return "dev"
}

func sortedKeys(files map[string][]byte) []string {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// group renders n with thousands separators.
func group(n int) string {
	digits := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "build ui source:", err)
	os.Exit(1)
}
