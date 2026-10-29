package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallScriptResolvesLatestReleaseWithoutGitHubAPI(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	script := string(data)
	for _, want := range []string{
		`latest_url="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest"`,
		`-w '%{url_effective}\n'`,
		`"https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/tag/"*)`,
		`releases/download/${VERSION}/${ASSET}`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install.sh no longer resolves latest releases through the GitHub redirect; missing %q", want)
		}
	}
	if strings.Contains(script, "api.github.com") {
		t.Fatal("install.sh should resolve release redirects without the GitHub API")
	}
}

const installerTestAsset = "term-llm_1.2.3_linux_amd64.tar.gz"

// This is a local, inert fixture, never a release executable. Its marker makes
// accidental automatic execution observable without running downloaded code.
const installerTestBinary = "#!/bin/sh\nprintf invoked > \"$HOME/invoked\"\nexit 93\n"

const installerCurlStub = `#!/bin/sh
set -eu
[ "$1" = -q ] || exit 88
shift
proto= redir= size= output= url= write= redirects= connect= timeout= headers= ignore_length=
while [ "$#" -gt 0 ]; do
    case "$1" in
        -fsSL) shift ;;
        --proto) proto=$2; shift 2 ;;
        --proto-redir) redir=$2; shift 2 ;;
        --max-filesize) size=$2; shift 2 ;;
        --max-redirs) redirects=$2; shift 2 ;;
        --connect-timeout) connect=$2; shift 2 ;;
        --max-time) timeout=$2; shift 2 ;;
        --head) headers=yes; shift ;;
        --ignore-content-length) ignore_length=yes; shift ;;
        -o) output=$2; shift 2 ;;
        -w) write=$2; shift 2 ;;
        -H) shift 2 ;;
        https://*) url=$1; shift ;;
        *) echo "unsafe or unexpected curl argument: $1" >&2; exit 90 ;;
    esac
done
[ "$proto" = '=https' ] && [ "$redir" = '=https' ] || exit 91
[ "$redirects" = 5 ] && [ "$connect" = 20 ] && [ "$timeout" = 300 ] || exit 92
printf '%s\n' "$url" >> "$FIXTURES/requests"
case "$url" in
    https://github.com/samsaffron/term-llm/releases/latest)
        [ "$size" = 1048576 ] && [ "$write" = '%{url_effective}\n' ] || exit 94
        if [ "$headers" = yes ]; then
            printf 'HTTP/2 200\r\nContent-Length: %s\r\n\r\n' "${LATEST_PAGE_BYTES:-8388608}" > "$output"
        else
            # A GET regresses to downloading an HTML page above the file cap.
            head -c "${LATEST_PAGE_BYTES:-8388608}" /dev/zero > "$output"
        fi
        [ "$headers" = yes ] && [ "$ignore_length" = yes ] || exit 98
        printf '%s\n' "$LATEST_URL"
        ;;
    https://github.com/samsaffron/term-llm/releases/download/v1.2.3/checksums.txt)
        [ -z "$headers$ignore_length" ] || exit 99
        [ "$size" = 1048576 ] || exit 95
        cp "$FIXTURES/checksums.txt" "$output"
        ;;
    https://github.com/samsaffron/term-llm/releases/download/v1.2.3/term-llm_1.2.3_linux_amd64.tar.gz)
        [ -z "$headers$ignore_length" ] || exit 99
        [ "$size" = 134217728 ] || exit 96
        # Deliberately ignore max-filesize, like old curl with chunked bodies:
        # the installer's file resource limit must still bound this write.
        cp "$FIXTURES/archive.tar.gz" "$output"
        ;;
    *) echo "unexpected URL: $url" >&2; exit 97 ;;
esac
`

type installerFixture struct {
	root string
	env  []string
}

func newInstallerFixture(t *testing.T, archive []byte) *installerFixture {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"home", "tools", "tmp", "fixtures"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := &installerFixture{root: root}
	f.write(t, "tools/curl", []byte(installerCurlStub), 0o700)
	f.write(t, "tools/uname", []byte("#!/bin/sh\ncase $1 in -s) echo Linux;; -m) echo x86_64;; *) exit 1;; esac\n"), 0o700)
	f.write(t, "fixtures/archive.tar.gz", archive, 0o600)
	digest := sha256.Sum256(archive)
	f.write(t, "fixtures/checksums.txt", []byte(fmt.Sprintf("%x  %s\n", digest, installerTestAsset)), 0o600)
	f.env = []string{
		"HOME=" + filepath.Join(root, "home"),
		"PATH=" + filepath.Join(root, "tools") + ":/usr/bin:/bin",
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"FIXTURES=" + filepath.Join(root, "fixtures"),
		"LATEST_URL=https://github.com/samsaffron/term-llm/releases/tag/v1.2.3",
		"LC_ALL=C",
	}
	return f
}

func (f *installerFixture) write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, path), data, mode); err != nil {
		t.Fatal(err)
	}
}

func (f *installerFixture) run(t *testing.T, shell string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, append([]string{"install.sh"}, args...)...)
	cmd.Env = f.env
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("installer timed out: %s", output)
	}
	if _, statErr := os.Stat(filepath.Join(f.root, "home/invoked")); !os.IsNotExist(statErr) {
		t.Fatalf("installer executed the fixture binary: %v\n%s", statErr, output)
	}
	entries, readErr := os.ReadDir(filepath.Join(f.root, "tmp"))
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("installer did not clean temporary files: %v, %v\n%s", entries, readErr, output)
	}
	return string(output), err
}

func (f *installerFixture) requireFailure(t *testing.T, message string, args ...string) {
	t.Helper()
	output, err := f.run(t, "sh", args...)
	if err == nil || !strings.Contains(output, message) {
		t.Fatalf("expected failure containing %q, got %v\n%s", message, err, output)
	}
	if _, err := os.Stat(filepath.Join(f.root, "home/.local/bin/term-llm")); !os.IsNotExist(err) {
		t.Fatalf("failed install wrote a binary: %v", err)
	}
}

type installerTarMember struct {
	name string
	kind byte
	link string
	body string
	size int64 // A nonzero size streams zero bytes, without allocating a large fixture.
}

func installerArchive(t *testing.T, members ...installerTarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, member := range members {
		size := int64(len(member.body))
		if member.size != 0 {
			size = member.size
		}
		header := &tar.Header{Name: member.name, Mode: 0o755, Typeflag: member.kind, Linkname: member.link, Size: size}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		var source io.Reader = strings.NewReader(member.body)
		if member.size != 0 {
			source = installerZeros{}
		}
		if _, err := io.CopyN(tw, source, size); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type installerZeros struct{}

func (installerZeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestInstallerValidInstallDoesNotExecuteBinary(t *testing.T) {
	for _, tarCommand := range []string{"tar", "bsdtar"} {
		t.Run(tarCommand, func(t *testing.T) {
			path, err := exec.LookPath(tarCommand)
			if err != nil {
				t.Skipf("%s unavailable", tarCommand)
			}
			archive := installerArchive(t,
				installerTarMember{name: "term-llm", body: installerTestBinary},
				installerTarMember{name: "README.md", body: "ignored"},
				installerTarMember{name: "internal/reflow/LICENSE", body: "ignored"},
				installerTarMember{name: "ignored-link", kind: tar.TypeSymlink, link: "/outside"},
			)
			f := newInstallerFixture(t, archive)
			if err := os.Symlink(path, filepath.Join(f.root, "tools/tar")); err != nil {
				t.Fatal(err)
			}
			// Accept binary-mode checksums, CRLF, and unrelated malformed entries.
			digest := sha256.Sum256(archive)
			f.write(t, "fixtures/checksums.txt", []byte(fmt.Sprintf("unrelated malformed entry\r\n%X *%s\r\n", digest, installerTestAsset)), 0o600)
			output, err := f.run(t, "sh") // Exercise the latest-release redirect too.
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, output)
			}
			binary := filepath.Join(f.root, "home/.local/bin/term-llm")
			data, err := os.ReadFile(binary)
			if err != nil || string(data) != installerTestBinary {
				t.Fatalf("installed content = %q, %v", data, err)
			}
			stat, err := os.Stat(binary)
			if err != nil || stat.Mode().Perm() != 0o755 {
				t.Fatalf("binary mode: %v, %v", stat, err)
			}
			files, err := os.ReadDir(filepath.Dir(binary))
			if err != nil || len(files) != 1 {
				t.Fatalf("installed unexpected files: %v, %v", files, err)
			}
			requests, err := os.ReadFile(filepath.Join(f.root, "fixtures/requests"))
			if err != nil || strings.Count(string(requests), "https://") != 3 {
				t.Fatalf("expected latest, checksums and asset requests: %s, %v", requests, err)
			}
		})
	}
}

func TestInstallerRejectsInvalidChecksumsBeforeDecompression(t *testing.T) {
	archive := installerArchive(t, installerTarMember{name: "term-llm", body: installerTestBinary})
	digest := sha256.Sum256(archive)
	valid := fmt.Sprintf("%x  %s\n", digest, installerTestAsset)
	cases := map[string]string{
		"missing":                  "abc  another-asset.tar.gz\n",
		"short digest":             "abc  " + installerTestAsset + "\n",
		"nonhex digest":            strings.Repeat("g", 64) + "  " + installerTestAsset + "\n",
		"bad delimiter":            fmt.Sprintf("%x\t %s\n", digest, installerTestAsset),
		"extra field":              fmt.Sprintf("%x ignored  %s\n", digest, installerTestAsset),
		"duplicate":                valid + valid,
		"malformed duplicate":      valid + "abc  " + installerTestAsset + "\n",
		"CRLF duplicate":           valid + strings.ReplaceAll(valid, "\n", "\r\n"),
		"CRLF malformed duplicate": valid + "abc  " + installerTestAsset + "\r\n",
		"mismatch":                 strings.Repeat("0", 64) + "  " + installerTestAsset + "\n",
		"wrong filename":           strings.ReplaceAll(valid, installerTestAsset, "prefix-"+installerTestAsset),
	}
	for name, checksums := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInstallerFixture(t, archive)
			f.write(t, "fixtures/checksums.txt", []byte(checksums), 0o600)
			// If verification is ordered incorrectly, this exits before extraction.
			f.write(t, "tools/gzip", []byte("#!/bin/sh\necho DECOMPRESSION_MUST_NOT_RUN >&2\nexit 89\n"), 0o700)
			f.requireFailure(t, "checksum", "--version", "1.2.3")
			if name != "mismatch" {
				requests, err := os.ReadFile(filepath.Join(f.root, "fixtures/requests"))
				if err != nil || strings.Contains(string(requests), installerTestAsset) {
					t.Fatalf("invalid manifest downloaded the archive: %s, %v", requests, err)
				}
			}
		})
	}
	t.Run("tampered archive", func(t *testing.T) {
		f := newInstallerFixture(t, archive)
		f.write(t, "fixtures/archive.tar.gz", append(append([]byte{}, archive...), 'x'), 0o600)
		f.requireFailure(t, "checksum mismatch", "--version", "v1.2.3")
	})
}

func TestInstallerRejectsUnsafeArchives(t *testing.T) {
	binary := installerTarMember{name: "term-llm", body: installerTestBinary}
	cases := map[string][]installerTarMember{
		"traversal":       {binary, {name: "../escaped", body: "unsafe"}},
		"absolute":        {binary, {name: "/absolute", body: "unsafe"}},
		"backslash":       {binary, {name: `dir\escaped`, body: "unsafe"}},
		"nested binary":   {{name: "dir/term-llm", body: installerTestBinary}},
		"dot prefix":      {{name: "./term-llm", body: installerTestBinary}},
		"duplicate":       {binary, binary},
		"symlink binary":  {{name: "term-llm", kind: tar.TypeSymlink, link: "payload"}, {name: "payload", body: installerTestBinary}},
		"hardlink binary": {{name: "payload", body: installerTestBinary}, {name: "term-llm", kind: tar.TypeLink, link: "payload"}},
		"directory":       {{name: "term-llm/", kind: tar.TypeDir}},
		"fifo":            {{name: "term-llm", kind: tar.TypeFifo}},
		"empty":           {{name: "term-llm"}},
	}
	for _, tarCommand := range []string{"tar", "bsdtar"} {
		t.Run(tarCommand, func(t *testing.T) {
			path, err := exec.LookPath(tarCommand)
			if err != nil {
				t.Skipf("%s unavailable", tarCommand)
			}
			for name, members := range cases {
				t.Run(name, func(t *testing.T) {
					f := newInstallerFixture(t, installerArchive(t, members...))
					if err := os.Symlink(path, filepath.Join(f.root, "tools/tar")); err != nil {
						t.Fatal(err)
					}
					f.requireFailure(t, "Error:", "--version", "1.2.3")
				})
			}
		})
	}
}

func TestInstallerReportsMissingDependenciesBeforeDownload(t *testing.T) {
	for _, missing := range []string{"tar", "gzip", "awk"} {
		t.Run(missing, func(t *testing.T) {
			f := newInstallerFixture(t, nil)
			for _, dependency := range []string{"tar", "gzip", "awk"} {
				if dependency == missing {
					continue
				}
				path, err := exec.LookPath(dependency)
				if err != nil {
					t.Skipf("%s unavailable", dependency)
				}
				if err := os.Symlink(path, filepath.Join(f.root, "tools", dependency)); err != nil {
					t.Fatal(err)
				}
			}
			// Only expose the stub curl and dependencies that are not under test.
			f.env[1] = "PATH=" + filepath.Join(f.root, "tools")
			f.requireFailure(t, missing+" is required", "--version", "1.2.3")
			if _, err := os.Stat(filepath.Join(f.root, "fixtures/requests")); !os.IsNotExist(err) {
				t.Fatalf("missing dependency made a network request: %v", err)
			}
		})
	}
}

func TestInstallerRejectsUnsafeVersions(t *testing.T) {
	for _, version := range []string{"", "v", "latest", "../1.2.3", "v1/other", `v1\other`, "v1?x", "v1#x", "v1%2fother", "v1 white", "v1\nother", "v１.2.3"} {
		t.Run(version, func(t *testing.T) {
			f := newInstallerFixture(t, nil)
			f.requireFailure(t, "Error:", "--version", version)
			if _, err := os.Stat(filepath.Join(f.root, "fixtures/requests")); !os.IsNotExist(err) {
				t.Fatalf("invalid version made a network request: %v", err)
			}
		})
	}
}

func TestInstallerRejectsUnsafeLatestRedirects(t *testing.T) {
	for _, url := range []string{
		"http://github.com/samsaffron/term-llm/releases/tag/v1.2.3",
		"https://evil.example/samsaffron/term-llm/releases/tag/v1.2.3",
		"https://github.com/samsaffron/term-llm/releases/tag/v1.2.3/other",
		"https://github.com/samsaffron/term-llm/releases/tag/v1.2.3?other",
	} {
		t.Run(url, func(t *testing.T) {
			f := newInstallerFixture(t, nil)
			f.env[len(f.env)-2] = "LATEST_URL=" + url
			f.requireFailure(t, "Error:")
			requests, err := os.ReadFile(filepath.Join(f.root, "fixtures/requests"))
			if err != nil || strings.Count(string(requests), "https://") != 1 {
				t.Fatalf("unsafe redirect triggered further requests: %s, %v", requests, err)
			}
		})
	}
}

func TestInstallerEnforcesSizeBounds(t *testing.T) {
	t.Run("checksums", func(t *testing.T) {
		f := newInstallerFixture(t, nil)
		f.write(t, "fixtures/checksums.txt", bytes.Repeat([]byte{'x'}, 1048577), 0o600)
		f.requireFailure(t, "Failed to download release checksums", "--version", "1.2.3")
	})
	t.Run("compressed archive", func(t *testing.T) {
		f := newInstallerFixture(t, nil)
		if err := os.Truncate(filepath.Join(f.root, "fixtures/archive.tar.gz"), 134217729); err != nil {
			t.Fatal(err)
		}
		f.requireFailure(t, "Failed to download release asset", "--version", "1.2.3")
	})
	t.Run("decompressed archive", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := io.CopyN(gz, installerZeros{}, 536870913); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		f := newInstallerFixture(t, buf.Bytes())
		f.requireFailure(t, "oversized decompressed archive", "--version", "1.2.3")
	})
	t.Run("binary", func(t *testing.T) {
		// bash uses 1024-byte ulimit blocks, allowing the full decompressed
		// archive bound so this exercises the separate 256 MiB binary cap.
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("bash unavailable for independent binary-size limit test")
		}
		f := newInstallerFixture(t, installerArchive(t, installerTarMember{name: "term-llm", size: 268435457}))
		output, err := f.run(t, bash, "--version", "1.2.3")
		if err == nil || !strings.Contains(output, "oversized binary") {
			t.Fatalf("expected binary size rejection: %v\n%s", err, output)
		}
	})
}
