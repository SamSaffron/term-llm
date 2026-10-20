package browserdemo

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	testReleaseID = "20261020T172300Z-0123456789ab"
	testGitSHA    = "0123456789abcdef0123456789abcdef01234567"
)

var criticalFiles = []string{
	"index.html",
	"term-llm.wasm",
	"wasm_exec.js",
	"boot.mjs",
	"worker.mjs",
	"transport.mjs",
	"inference.bundle.mjs",
	"linux.mjs",
	"linux-client.mjs",
	"linux-worker.mjs",
	"linux-assets.mjs",
	"python-runtime.mjs",
	"libv86.mjs",
	"v86.wasm",
	"runtime/python-root.tar.gz",
	"runtime/python-runtime.json",
	"assets/asset-manifest.json",
	"manifest.webmanifest",
	"icon-512.png",
}

func TestPrepareRelease(t *testing.T) {
	requireLinuxTools(t)
	repo := repoRoot(t)
	dist := writeDist(t)
	out := filepath.Join(t.TempDir(), "release")
	tarball := out + ".tar.gz"
	runPrepare(t, repo, dist, out, tarball, testReleaseID)

	data, err := os.ReadFile(filepath.Join(out, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var release map[string]any
	if err := json.Unmarshal(data, &release); err != nil {
		t.Fatalf("release.json: %v", err)
	}
	if release["id"] != testReleaseID || release["gitSha"] != testGitSHA || release["builtAt"] == "" {
		t.Fatalf("unexpected release metadata: %s", data)
	}
	if _, ok := release["pythonRuntime"].(map[string]any); !ok {
		t.Fatalf("release.json has no pythonRuntime: %s", data)
	}
	model, ok := release["model"].(map[string]any)
	if !ok || model["path"] != "models/test-model.bin" || model["url"] != "https://demo.term-llm.com/models/test-model.bin" || model["bytes"] != float64(8192) {
		t.Fatalf("release.json has unexpected model metadata: %s", data)
	}

	for _, file := range criticalFiles {
		if info, err := os.Stat(filepath.Join(out, file)); err != nil || info.Size() == 0 {
			t.Fatalf("critical file %s missing or empty: %v", file, err)
		}
	}
	for _, file := range []string{"index.html.gz", "term-llm.wasm.gz", "release.json.gz", "runtime/python-runtime.json.gz"} {
		if _, err := os.Stat(filepath.Join(out, file)); err != nil {
			t.Fatalf("expected gzip sibling %s: %v", file, err)
		}
	}
	runScript(t, repo, nil, "bash", "-c", "cd "+shellQuote(out)+" && sha256sum -c manifest.sha256 --quiet")

	listing := runScript(t, repo, nil, "tar", "-tzf", tarball)
	if !strings.Contains(listing, "./release.json") || !strings.Contains(listing, "./manifest.sha256") {
		t.Fatalf("tarball is missing release metadata:\n%s", listing)
	}
	for _, forbidden := range []string{"seabios.bin", "vgabios.bin", "linux.bin", "bzimage"} {
		if strings.Contains(listing, forbidden) {
			t.Fatalf("tarball redistributes upstream asset %s", forbidden)
		}
	}
}

func TestPrepareRejectsUpstreamKernelAssets(t *testing.T) {
	requireLinuxTools(t)
	repo := repoRoot(t)
	dist := writeDist(t)
	if err := os.WriteFile(filepath.Join(dist, "seabios.bin"), []byte("bios"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "release")
	_, err := runScriptErr(t, repo, nil, "bash", filepath.Join(repo, "ops/browser-demo/prepare-release.sh"),
		"--dist", dist,
		"--out", out,
		"--tarball", out+".tar.gz",
		"--id", testReleaseID,
		"--git-sha", testGitSHA,
		"--git-ref", "refs/heads/main",
		"--built-at", "2026-10-20T17:23:00Z",
		"--source-date-epoch", "0",
		"--python-pin", filepath.Join(dist, "python-runtime.mjs"),
		"--model-pin", writeModelPin(t),
	)
	if err == nil {
		t.Fatal("prepare-release accepted an upstream kernel asset")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("failed prepare left an output directory: %v", statErr)
	}
}

func TestCleanupReleasesBoundsOnlyOwnedDirectories(t *testing.T) {
	requireLinuxTools(t)
	repo := repoRoot(t)
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	mustMkdir(t, releases)

	current := "20261020-range-fix7"
	owned := []string{
		"20251231T000000Z-ffffffffffff",
		"20260101T000000Z-aaaaaaaaaaaa",
		"20260102T000000Z-bbbbbbbbbbbb",
		"20260103T000000Z-cccccccccccc",
		"20260104T000000Z-dddddddddddd",
	}
	for _, name := range owned {
		mustMkdir(t, filepath.Join(releases, name))
		if err := os.WriteFile(filepath.Join(releases, name, "index.html"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(t, filepath.Join(releases, current))
	mustMkdir(t, filepath.Join(releases, "20261020-python6"))
	mustMkdir(t, filepath.Join(releases, "handmade-release"))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(releases, "20260105T000000Z-eeeeeeeeeeee")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "releases/"+current, filepath.Join(root, "current"))

	env := map[string]string{"DEMO_ALLOW_CUSTOM_ROOT": "1"}
	// The immediate rollback target may be older than the newest three after a
	// manual rollback; it must be protected explicitly.
	runScript(t, repo, env, "bash", filepath.Join(repo, "ops/browser-demo/cleanup-releases.sh"),
		"--root", root, "--keep", "3", "--protect", "20260101T000000Z-aaaaaaaaaaaa")

	for _, kept := range []string{
		current,
		"20260104T000000Z-dddddddddddd",
		"20260103T000000Z-cccccccccccc",
		"20260102T000000Z-bbbbbbbbbbbb",
		"20260101T000000Z-aaaaaaaaaaaa",
		"20261020-python6",
		"handmade-release",
	} {
		if _, err := os.Stat(filepath.Join(releases, kept)); err != nil {
			t.Fatalf("expected %s to be preserved: %v", kept, err)
		}
	}
	for _, removed := range []string{"20251231T000000Z-ffffffffffff"} {
		if _, err := os.Stat(filepath.Join(releases, removed)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed: %v", removed, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(releases, "20260105T000000Z-eeeeeeeeeeee")); err != nil {
		t.Fatalf("cleanup touched a symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "index.html")); err == nil {
		t.Fatal("cleanup followed a releases symlink outside the release root")
	}

	if err := os.RemoveAll(releases); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, releases); err != nil {
		t.Fatal(err)
	}
	if _, err := runScriptErr(t, repo, env, "bash", filepath.Join(repo, "ops/browser-demo/cleanup-releases.sh"), "--root", root); err == nil {
		t.Fatal("cleanup accepted a symlinked releases directory")
	}
}

func TestRemoteDeployAtomicSwapRollbackAndRangeSmoke(t *testing.T) {
	requireLinuxTools(t)
	repo := repoRoot(t)
	dist := writeDist(t)
	tarball := filepath.Join(t.TempDir(), testReleaseID+".tar.gz")
	out := filepath.Join(t.TempDir(), "release")
	runPrepare(t, repo, dist, out, tarball, testReleaseID)

	remote := filepath.Join(repo, "ops/browser-demo/remote-deploy.sh")
	cleanup := filepath.Join(repo, "ops/browser-demo/cleanup-releases.sh")

	t.Run("successful swap and bounded cleanup", func(t *testing.T) {
		root, err := setupHost(t)
		if err != nil {
			t.Fatal(err)
		}
		fake := writeFakeCurl(t)
		fakebin, nginxLog := writeNoopNginx(t)
		incoming := filepath.Join(root, "releases", ".incoming-"+testReleaseID+".tar.gz")
		copyFile(t, tarball, incoming)
		env := smokeEnv(root, fake, fakebin, nginxLog)
		runScript(t, repo, env, "bash", remote,
			"--root", root,
			"--tarball", incoming,
			"--id", testReleaseID,
			"--keep", "3",
			"--cleanup-script", cleanup)

		assertCurrent(t, root, "releases/"+testReleaseID)
		for _, preserved := range []string{
			"20260104T000000Z-dddddddddddd",
			"20260103T000000Z-cccccccccccc",
		} {
			if _, err := os.Stat(filepath.Join(root, "releases", preserved)); err != nil {
				t.Fatalf("expected previous release %s to remain: %v", preserved, err)
			}
		}
		for _, removed := range []string{"20260102T000000Z-bbbbbbbbbbbb", "20260101T000000Z-aaaaaaaaaaaa"} {
			if _, err := os.Stat(filepath.Join(root, "releases", removed)); !os.IsNotExist(err) {
				t.Fatalf("old release %s was not cleaned up: %v", removed, err)
			}
		}
		assertPreservedHostFiles(t, root)
		if data, err := os.ReadFile(nginxLog); err == nil && len(data) > 0 {
			t.Fatalf("deployment touched nginx: %s", data)
		}
		if _, err := os.Stat(incoming); !os.IsNotExist(err) {
			t.Fatalf("incoming tarball was not removed: %v", err)
		}
	})

	t.Run("manual rollback target older than newest three is retained", func(t *testing.T) {
		root, err := setupHost(t)
		if err != nil {
			t.Fatal(err)
		}
		// Simulate a manual rollback to a release older than the newest three.
		if err := os.Remove(filepath.Join(root, "current")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("releases/20260101T000000Z-aaaaaaaaaaaa", filepath.Join(root, "current")); err != nil {
			t.Fatal(err)
		}
		fake := writeFakeCurl(t)
		fakebin, nginxLog := writeNoopNginx(t)
		incoming := filepath.Join(root, "releases", ".incoming-"+testReleaseID+".tar.gz")
		copyFile(t, tarball, incoming)
		env := smokeEnv(root, fake, fakebin, nginxLog)
		runScript(t, repo, env, "bash", remote,
			"--root", root,
			"--tarball", incoming,
			"--id", testReleaseID,
			"--keep", "3",
			"--cleanup-script", cleanup)

		assertCurrent(t, root, "releases/"+testReleaseID)
		if _, err := os.Stat(filepath.Join(root, "releases", "20260101T000000Z-aaaaaaaaaaaa")); err != nil {
			t.Fatalf("cleanup deleted the immediate rollback target: %v", err)
		}
	})

	t.Run("range request returning 200 rolls back", func(t *testing.T) {
		root, err := setupHost(t)
		if err != nil {
			t.Fatal(err)
		}
		fake := writeFakeCurl(t)
		fakebin, nginxLog := writeNoopNginx(t)
		incoming := filepath.Join(root, "releases", ".incoming-"+testReleaseID+".tar.gz")
		copyFile(t, tarball, incoming)
		env := smokeEnv(root, fake, fakebin, nginxLog)
		env["FAKE_CURL_RANGE_STATUS"] = "200"
		_, err = runScriptErr(t, repo, env, "bash", remote,
			"--root", root,
			"--tarball", incoming,
			"--id", testReleaseID,
			"--cleanup-script", cleanup)
		if err == nil {
			t.Fatal("deployment accepted a 200 response for a range request")
		}
		assertCurrent(t, root, "releases/20260104T000000Z-dddddddddddd")
		if _, statErr := os.Stat(filepath.Join(root, "releases", testReleaseID)); !os.IsNotExist(statErr) {
			t.Fatalf("failed release was not removed: %v", statErr)
		}
		assertPreservedHostFiles(t, root)
	})

	t.Run("asset hash failure rolls back", func(t *testing.T) {
		root, err := setupHost(t)
		if err != nil {
			t.Fatal(err)
		}
		fake := writeFakeCurl(t)
		fakebin, nginxLog := writeNoopNginx(t)
		incoming := filepath.Join(root, "releases", ".incoming-"+testReleaseID+".tar.gz")
		copyFile(t, tarball, incoming)
		env := smokeEnv(root, fake, fakebin, nginxLog)
		env["FAKE_CURL_FAIL"] = "term-llm.wasm"
		_, err = runScriptErr(t, repo, env, "bash", remote,
			"--root", root,
			"--tarball", incoming,
			"--id", testReleaseID,
			"--cleanup-script", cleanup)
		if err == nil {
			t.Fatal("deployment accepted a failed origin smoke")
		}
		assertCurrent(t, root, "releases/20260104T000000Z-dddddddddddd")
		if _, statErr := os.Stat(filepath.Join(root, "releases", testReleaseID)); !os.IsNotExist(statErr) {
			t.Fatalf("failed release was not removed: %v", statErr)
		}
		assertPreservedHostFiles(t, root)
	})

	t.Run("missing pinned model fails before swap", func(t *testing.T) {
		root, err := setupHost(t)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "models", "test-model.bin")); err != nil {
			t.Fatal(err)
		}
		fake := writeFakeCurl(t)
		fakebin, nginxLog := writeNoopNginx(t)
		incoming := filepath.Join(root, "releases", ".incoming-"+testReleaseID+".tar.gz")
		copyFile(t, tarball, incoming)
		env := smokeEnv(root, fake, fakebin, nginxLog)
		_, err = runScriptErr(t, repo, env, "bash", remote,
			"--root", root,
			"--tarball", incoming,
			"--id", testReleaseID,
			"--cleanup-script", cleanup)
		if err == nil {
			t.Fatal("deployment accepted a missing pinned model")
		}
		assertCurrent(t, root, "releases/20260104T000000Z-dddddddddddd")
		if _, statErr := os.Stat(filepath.Join(root, "releases", testReleaseID)); !os.IsNotExist(statErr) {
			t.Fatalf("failed release was not removed: %v", statErr)
		}
	})
}

func TestDeployScriptPinsHostKeyAndKeyFile(t *testing.T) {
	requireLinuxTools(t)
	repo := repoRoot(t)
	fakebin := t.TempDir()
	log := filepath.Join(t.TempDir(), "ssh.log")
	for _, name := range []string{"ssh", "scp"} {
		writeExecutable(t, filepath.Join(fakebin, name), "#!/usr/bin/env bash\nprintf '%s %s\\n' "+name+" \"$*\" >>\"$FAKE_SSH_LOG\"\nexit 0\n")
	}
	key := filepath.Join(t.TempDir(), "deploy-key")
	if err := os.WriteFile(key, []byte("not-a-real-private-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(t.TempDir(), "release.tar.gz")
	if err := os.WriteFile(tarball, []byte("tar"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"DEPLOY_SSH_KEY_FILE": key,
		"DEMO_SSH":            filepath.Join(fakebin, "ssh"),
		"DEMO_SCP":            filepath.Join(fakebin, "scp"),
		"FAKE_SSH_LOG":        log,
		"PATH":                fakebin + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	output := runScript(t, repo, env, "bash", filepath.Join(repo, "ops/browser-demo/deploy.sh"),
		"--tarball", tarball, "--id", testReleaseID, "--known-hosts", filepath.Join(repo, "ops/browser-demo/known_hosts"))

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(data)
	for _, want := range []string{
		"-o StrictHostKeyChecking=yes",
		"-o IdentitiesOnly=yes",
		"-o BatchMode=yes",
		"-o GlobalKnownHostsFile=/dev/null",
		"UserKnownHostsFile=" + filepath.Join(repo, "ops/browser-demo/known_hosts"),
		"releases/.incoming-" + testReleaseID + ".tar.gz",
		"DEMO_TEMP_DIR=",
		"remote-deploy.sh",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("deploy command is missing %q:\n%s", want, logged)
		}
	}
	for _, forbidden := range []string{"StrictHostKeyChecking=no", "PasswordAuthentication=yes"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("deploy command weakens SSH checks: %s", forbidden)
		}
	}
	if strings.Contains(output, "not-a-real-private-key") {
		t.Fatal("deploy printed the private key")
	}
}

func TestEnsurePythonRuntimeRejectsUnpinnedBytes(t *testing.T) {
	requireLinuxTools(t)
	repo := repoRoot(t)
	script := filepath.Join(repo, "ops/browser-demo/ensure-python-runtime.sh")

	newPin := func(t *testing.T, archive []byte) string {
		root := t.TempDir()
		web := filepath.Join(root, "experiments/browser-bonsai/web")
		mustMkdir(t, filepath.Join(web, "runtime"))
		sum := sha256.Sum256(archive)
		pin := "// test pin\nexport const pythonRuntime = {\n" +
			"  file: 'runtime/python-root.tar.gz',\n" +
			"  sha256: '" + hex.EncodeToString(sum[:]) + "',\n" +
			"  bytes: " + strconv.Itoa(len(archive)) + ",\n};\n"
		if err := os.WriteFile(filepath.Join(web, "python-runtime.mjs"), []byte(pin), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("verified public archive is installed", func(t *testing.T) {
		archive := gzipBytes(t, []byte("verified python runtime"))
		root := newPin(t, archive)
		source := t.TempDir()
		copyFileBytes(t, filepath.Join(source, "python-root.tar.gz"), archive)
		writeRuntimeMetadata(t, filepath.Join(source, "python-runtime.json"), archive)
		fakebin := writeFakeDownloader(t, source)
		env := map[string]string{
			"PATH":                    fakebin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"PYTHON_RUNTIME_BASE_URL": "https://example.test/runtime",
			"FAKE_PYTHON_SOURCE":      source,
		}
		runScript(t, repo, env, "bash", script, "--repo", root, "--no-recipe")
		installed, err := os.ReadFile(filepath.Join(root, "experiments/browser-bonsai/web/runtime/python-root.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(installed, archive) {
			t.Fatal("installed runtime differs from the verified public archive")
		}
	})

	t.Run("mismatched public archive is rejected", func(t *testing.T) {
		pinned := gzipBytes(t, []byte("pinned python runtime"))
		root := newPin(t, pinned)
		source := t.TempDir()
		copyFileBytes(t, filepath.Join(source, "python-root.tar.gz"), gzipBytes(t, []byte("different bytes")))
		writeRuntimeMetadata(t, filepath.Join(source, "python-runtime.json"), gzipBytes(t, []byte("different bytes")))
		fakebin := writeFakeDownloader(t, source)
		env := map[string]string{
			"PATH":                    fakebin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"PYTHON_RUNTIME_BASE_URL": "https://example.test/runtime",
			"FAKE_PYTHON_SOURCE":      source,
		}
		_, err := runScriptErr(t, repo, env, "bash", script, "--repo", root, "--no-recipe")
		if err == nil {
			t.Fatal("ensure-python-runtime installed bytes that did not match the repository pin")
		}
		if _, statErr := os.Stat(filepath.Join(root, "experiments/browser-bonsai/web/runtime/python-root.tar.gz")); !os.IsNotExist(statErr) {
			t.Fatalf("unverified runtime was installed: %v", statErr)
		}
	})
}

func TestWorkflowSecurityAndRangeSmokeShape(t *testing.T) {
	repo := repoRoot(t)
	workflow, err := os.ReadFile(filepath.Join(repo, ".github/workflows/deploy-browser-demo.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, want := range []string{
		"cron: '23 17 * * *'",
		"github.repository == 'SamSaffron/term-llm'",
		"github.ref == 'refs/heads/main'",
		"pull_request:",
		"secrets.DEPLOY_SSH_KEY",
		"known_hosts",
		"hashFiles('experiments/browser-bonsai/web/python-runtime.mjs')",
		"needs: build",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("workflow is missing %q", want)
		}
	}
	if strings.Contains(text, "pull_request_target") {
		t.Fatal("workflow must not use pull_request_target")
	}
	if strings.Contains(text, "StrictHostKeyChecking=no") {
		t.Fatal("workflow disables host key checking")
	}
	if deploy := strings.Index(text, "\n  deploy:"); deploy < 0 {
		t.Fatal("workflow has no deploy job")
	} else if strings.Contains(text[:deploy], "secrets.") {
		t.Fatal("build/test job references a secret")
	}

	remote, err := os.ReadFile(filepath.Join(repo, "ops/browser-demo/remote-deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	smoke := string(remote)
	if !strings.Contains(smoke, "content-range:") {
		t.Fatal("origin smoke does not validate Content-Range on 206")
	}
	if !strings.Contains(smoke, "HEAD omits Accept-Ranges (allowed)") {
		t.Fatal("origin smoke does not treat HEAD Accept-Ranges as advisory")
	}
}

func runPrepare(t *testing.T, repo, dist, out, tarball, id string) {
	t.Helper()
	runScript(t, repo, nil, "bash", filepath.Join(repo, "ops/browser-demo/prepare-release.sh"),
		"--dist", dist,
		"--out", out,
		"--tarball", tarball,
		"--id", id,
		"--git-sha", testGitSHA,
		"--git-ref", "refs/heads/main",
		"--built-at", "2026-10-20T17:23:00Z",
		"--source-date-epoch", "0",
		"--python-pin", filepath.Join(dist, "python-runtime.mjs"),
		"--model-pin", writeModelPin(t),
	)
}

func writeModelPin(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inference.mjs")
	model := "export const MODEL_URL = 'https://demo.term-llm.com/models/test-model.bin';\n" +
		"export const MODEL_BYTES = 8192;\n"
	if err := os.WriteFile(path, []byte(model), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDist(t *testing.T) string {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "dist")
	mustMkdir(t, dist)
	for _, file := range criticalFiles {
		path := filepath.Join(dist, file)
		mustMkdir(t, filepath.Dir(path))
		var body []byte
		switch file {
		case "term-llm.wasm", "v86.wasm":
			body = append([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, bytes.Repeat([]byte{0x42}, 4096)...)
		case "index.html":
			body = []byte("<!doctype html><script type=\"module\" src=\"boot.mjs\"></script>")
		case "assets/asset-manifest.json":
			body = []byte("{}\n")
		default:
			body = []byte("test " + file + "\n")
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	linuxAssets := `export const bootAssets = [
  ['bios','https://raw.githubusercontent.com/copy/v86/deadbeef/bios/seabios.bin','x'],
  ['vga_bios','https://raw.githubusercontent.com/copy/v86/deadbeef/bios/vgabios.bin','x'],
  ['bzimage','https://i.copy.sh/buildroot-bzimage68.bin','x'],
];`
	if err := os.WriteFile(filepath.Join(dist, "linux-assets.mjs"), []byte(linuxAssets), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := gzipBytes(t, []byte("pinned-"+testReleaseID))
	writeRuntimeMetadata(t, filepath.Join(dist, "runtime/python-runtime.json"), archive)
	if err := os.WriteFile(filepath.Join(dist, "runtime/python-root.tar.gz"), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	pin := "// Generated for the deployment test.\nexport const pythonRuntime = {\n" +
		"  file: 'runtime/python-root.tar.gz',\n" +
		"  sha256: '" + hex.EncodeToString(sum[:]) + "',\n" +
		"  bytes: " + strconv.Itoa(len(archive)) + ",\n" +
		"  installedBytes: 1,\n  files: 1,\n  pythonVersion: '3.12.15',\n};\n"
	if err := os.WriteFile(filepath.Join(dist, "python-runtime.mjs"), []byte(pin), 0o644); err != nil {
		t.Fatal(err)
	}
	return dist
}

func writeRuntimeMetadata(t *testing.T, path string, archive []byte) {
	t.Helper()
	sum := sha256.Sum256(archive)
	metadata := map[string]any{
		"archive": "python-root.tar.gz",
		"bytes":   len(archive),
		"sha256":  hex.EncodeToString(sum[:]),
		"files":   1,
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setupHost(t *testing.T) (string, error) {
	t.Helper()
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	mustMkdir(t, releases)
	mustMkdir(t, filepath.Join(root, "models"))
	mustMkdir(t, filepath.Join(root, "acme"))
	if err := os.WriteFile(filepath.Join(root, "models", "test-model.bin"), bytes.Repeat([]byte{0x17}, 8192), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "acme", "token"), []byte("acme"), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "config.conf"), []byte("preserve me"), 0o644); err != nil {
		return "", err
	}
	for _, name := range []string{
		"20260101T000000Z-aaaaaaaaaaaa",
		"20260102T000000Z-bbbbbbbbbbbb",
		"20260103T000000Z-cccccccccccc",
		"20260104T000000Z-dddddddddddd",
	} {
		mustMkdir(t, filepath.Join(releases, name))
		if err := os.WriteFile(filepath.Join(releases, name, "index.html"), []byte(name), 0o644); err != nil {
			return "", err
		}
	}
	if err := os.Symlink("releases/20260104T000000Z-dddddddddddd", filepath.Join(root, "current")); err != nil {
		return "", err
	}
	return root, nil
}

func assertPreservedHostFiles(t *testing.T, root string) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(root, "models", "test-model.bin")); err != nil || len(data) != 8192 {
		t.Fatalf("model was not preserved: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "acme", "token")); err != nil || string(data) != "acme" {
		t.Fatalf("ACME state was not preserved: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "config.conf")); err != nil || string(data) != "preserve me" {
		t.Fatalf("host config was not preserved: %v", err)
	}
}

func assertCurrent(t *testing.T, root, want string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("current -> %s; want %s", got, want)
	}
}

func smokeEnv(root, fakeCurl, fakebin, nginxLog string) map[string]string {
	return map[string]string{
		"DEMO_ALLOW_CUSTOM_ROOT": "1",
		"DEMO_CURL":              fakeCurl,
		"DEMO_HTTP_BASE":         "https://demo.term-llm.com",
		"DEMO_HTTP_RESOLVE":      "demo.term-llm.com:443:127.0.0.1",
		"FAKE_CURL_ROOT":         root,
		"FAKE_NGINX_LOG":         nginxLog,
		"PATH":                   fakebin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DEMO_LOCK_TIMEOUT":      "5",
	}
}

func writeFakeCurl(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "curl")
	writeExecutable(t, path, `#!/usr/bin/env bash
set -euo pipefail
out= headers= url= range= head= write_out=
while (($#)); do
  case "$1" in
    --output) out=$2; shift 2 ;;
    --dump-header) headers=$2; shift 2 ;;
    --write-out) write_out=$2; shift 2 ;;
    --range) range=$2; shift 2 ;;
    --resolve|--header|--connect-timeout|--max-time|--noproxy) shift 2 ;;
    --head) head=1; shift ;;
    --fail|--silent|--show-error) shift ;;
    *) url=$1; shift ;;
  esac
done
[[ -n $url ]]
path=${url%%\?*}
path=${path#https://demo.term-llm.com/}
if [[ -n ${FAKE_CURL_FAIL:-} && ${FAKE_CURL_FAIL} == "$path" ]]; then exit 22; fi
if [[ $path == models/* ]]; then
  src=$FAKE_CURL_ROOT/$path
else
  src=$FAKE_CURL_ROOT/$(readlink "$FAKE_CURL_ROOT/current")
  if [[ -z $path ]]; then path=index.html; fi
  src=$src/$path
fi
if [[ -n $head ]]; then
  {
    printf 'HTTP/2 200\r\n'
    printf 'content-length: %s\r\n' "$(stat -c %s "$src")"
    if [[ ${FAKE_CURL_HEAD_ACCEPT_RANGES:-0} == 1 ]]; then printf 'accept-ranges: bytes\r\n'; fi
    printf '\r\n'
  } >"$out"
  exit 0
fi
if [[ -n $range ]]; then
  status=${FAKE_CURL_RANGE_STATUS:-206}
  {
    printf 'HTTP/2 %s\r\n' "$status"
    if [[ $status == 206 ]]; then
      printf 'content-range: bytes 0-1023/%s\r\n' "$(stat -c %s "$src")"
    fi
    if [[ ${FAKE_CURL_206_ACCEPT_RANGES:-0} == 1 ]]; then printf 'accept-ranges: bytes\r\n'; fi
    printf '\r\n'
  } >"$headers"
  if [[ $status == 206 ]]; then head -c 1024 "$src" >"$out"; else cp "$src" "$out"; fi
  [[ -n $write_out ]] && printf '%s' "$status"
  exit 0
fi
if [[ -n $out ]]; then cp "$src" "$out"; else cat "$src"; fi
`)
	return path
}

func writeNoopNginx(t *testing.T) (fakebin, log string) {
	t.Helper()
	fakebin = t.TempDir()
	log = filepath.Join(t.TempDir(), "nginx.log")
	writeExecutable(t, filepath.Join(fakebin, "nginx"), "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >>\"$FAKE_NGINX_LOG\"\nexit 0\n")
	return fakebin, log
}

func writeFakeDownloader(t *testing.T, source string) string {
	t.Helper()
	fakebin := t.TempDir()
	writeExecutable(t, filepath.Join(fakebin, "curl"), `#!/usr/bin/env bash
set -euo pipefail
out= url=
while (($#)); do
  case "$1" in
    -o|--output) out=$2; shift 2 ;;
    -fsSL|-f|-s|-S|-L|--fail|--silent|--show-error) shift ;;
    --retry|--connect-timeout|--max-time) shift 2 ;;
    *) url=$1; shift ;;
  esac
done
[[ -n $out && -n $url ]]
case $url in
  */python-root.tar.gz) cp "$FAKE_PYTHON_SOURCE/python-root.tar.gz" "$out" ;;
  */python-runtime.json) cp "$FAKE_PYTHON_SOURCE/python-runtime.json" "$out" ;;
  *) exit 22 ;;
esac
`)
	return fakebin
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func requireLinuxTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("browser demo deployment scripts target Linux")
	}
	for _, name := range []string{"bash", "tar", "gzip", "sha256sum", "stat", "flock", "readlink", "realpath", "df", "find", "sed", "awk", "od", "python3", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("required command %s is unavailable: %v", name, err)
		}
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, source, dest string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	copyFileBytes(t, dest, data)
}

func copyFileBytes(t *testing.T, dest string, data []byte) {
	t.Helper()
	mustMkdir(t, filepath.Dir(dest))
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runScript(t *testing.T, dir string, env map[string]string, name string, args ...string) string {
	t.Helper()
	output, err := runScriptErr(t, dir, env, name, args...)
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return output
}

func runScriptErr(t *testing.T, dir string, env map[string]string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = mergedEnv(env)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func mergedEnv(overrides map[string]string) []string {
	values := map[string]string{}
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
