package update

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type probeTransport func(*http.Request) (*http.Response, error)

func (f probeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Exercise the real RunUpgrade only in a subprocess whose executable is a
// disposable copy. All responses are inert in-memory fixtures: no live releases,
// user installation, or downloaded executable is ever run by these probes.
func TestRunUpgradeSafetyProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("published self-upgrades target Linux and macOS")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	const payload = "#!/bin/sh\nprintf 'THIS PAYLOAD MUST NEVER RUN'\n"
	validArchive := testReleaseArchive(t, []archiveTestEntry{{RepoName, tar.TypeReg, payload}})
	for _, tc := range []struct {
		name     string
		archive  []byte
		checksum []byte
		wantErr  string
	}{
		{"valid", validArchive, validArchive, ""},
		{"tampered", []byte("tampered archive"), validArchive, "checksum mismatch"},
		{"corrupt but checksum matches", []byte("not gzip"), []byte("not gzip"), "unexpected EOF"},
		{"linked binary", testReleaseArchive(t, []archiveTestEntry{{RepoName, tar.TypeSymlink, ""}}), nil, "regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			probeExe := filepath.Join(dir, "probe")
			if err := os.WriteFile(probeExe, original, 0o755); err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(dir, "fixture.tar.gz")
			if err := os.WriteFile(archivePath, tc.archive, 0o600); err != nil {
				t.Fatal(err)
			}
			checksumPayload := tc.checksum
			if checksumPayload == nil {
				checksumPayload = tc.archive
			}
			manifest := fmt.Sprintf("%x  %s_1.2.3_%s_%s.tar.gz\n", sha256.Sum256(checksumPayload), RepoName, runtime.GOOS, runtime.GOARCH)
			cmd := exec.Command(probeExe, "-test.run=^TestRunUpgradeProbeProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(),
				"TERM_LLM_UPGRADE_PROBE=1", "TERM_LLM_PROBE_ARCHIVE="+archivePath,
				"TERM_LLM_PROBE_MANIFEST="+manifest, "TERM_LLM_PROBE_ERROR="+tc.wantErr,
				"HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "TMPDIR="+dir,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("probe failed: %v\n%s", err, out)
			}
			if bytes.Contains(out, []byte("THIS PAYLOAD MUST NEVER RUN")) {
				t.Fatal("updater executed downloaded payload")
			}
			got, err := os.ReadFile(probeExe)
			if err != nil {
				t.Fatal(err)
			}
			want := original
			if tc.wantErr == "" {
				want = []byte(payload)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("updater replaced the executable with unexpected/unverified bytes")
			}
			leftovers, err := filepath.Glob(filepath.Join(dir, ".term-llm-upgrade-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("left partial executable: %v, %v", leftovers, err)
			}
		})
	}
}

func TestRunUpgradeProbeProcess(t *testing.T) {
	if os.Getenv("TERM_LLM_UPGRADE_PROBE") != "1" {
		t.Skip("subprocess helper")
	}
	archive, err := os.ReadFile(os.Getenv("TERM_LLM_PROBE_ARCHIVE"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := os.Getenv("TERM_LLM_PROBE_MANIFEST")
	http.DefaultTransport = probeTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "github.com" || !strings.HasPrefix(req.URL.Path, "/samsaffron/term-llm/releases/download/v1.2.3/") {
			return nil, fmt.Errorf("unexpected probe URL %s", req.URL)
		}
		body := archive
		if strings.HasSuffix(req.URL.Path, "/checksums.txt") {
			body = []byte(manifest)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
	})
	err = RunUpgrade(context.Background(), "dev", "v1.2.3", os.Stdout, os.Stderr)
	wantErr := os.Getenv("TERM_LLM_PROBE_ERROR")
	if wantErr == "" {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("RunUpgrade() = %v, want error containing %q", err, wantErr)
	}
}
