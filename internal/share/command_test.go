package share

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func commandTestPublisher(t *testing.T, mode string, timeout time.Duration) *CommandPublisher {
	t.Helper()
	publisher, err := NewCommandPublisher([]string{os.Args[0], "-test.run=^TestCommandHelperProcess$", "--", "prefix argument"}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_LLM_SHARE_HELPER", "1")
	t.Setenv("TERM_LLM_SHARE_HELPER_MODE", mode)
	return publisher
}

func commandTestRequest() Request {
	return Request{
		RequestID: "request-1", Title: "A title", Description: "A description",
		Visibility: VisibilityPrivate, Entrypoint: "index.html",
		Files: []File{
			{Name: "index.html", MediaType: "text/html; charset=utf-8", Role: "entrypoint", Content: []byte("<h1>hello</h1>")},
			{Name: "session.md", MediaType: "text/markdown; charset=utf-8", Role: "transcript", Content: []byte("# hello")},
		},
	}
}

func TestCommandPublisherCapabilitiesValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		mode string
		code ErrorCode
	}{
		{name: "valid", mode: "valid"},
		{name: "bad protocol", mode: "bad-protocol", code: ErrorProtocol},
		{name: "missing create", mode: "missing-create", code: ErrorProtocol},
		{name: "missing visibility", mode: "missing-visibility", code: ErrorProtocol},
		{name: "reserved github id", mode: "github-id", code: ErrorProtocol},
		{name: "oversized provider name", mode: "long-provider-name", code: ErrorProtocol},
		{name: "control in provider help", mode: "control-provider-help", code: ErrorProtocol},
		{name: "too many notes", mode: "too-many-notes", code: ErrorProtocol},
		{name: "oversized limits", mode: "oversized-limits", code: ErrorProtocol},
	} {
		t.Run(test.name, func(t *testing.T) {
			publisher := commandTestPublisher(t, test.mode, time.Second)
			capabilities, err := publisher.Capabilities(context.Background())
			if test.code == "" {
				if err != nil || capabilities.Provider.ID != "test-helper" || !capabilities.Supports(OperationUpdate) {
					t.Fatalf("capabilities=%+v error=%v", capabilities, err)
				}
				return
			}
			if err == nil || AsError(err).Code != test.code {
				t.Fatalf("error=%v code=%q, want %q", err, AsError(err).Code, test.code)
			}
		})
	}
}

func TestCommandPublisherReportsMissingExecutable(t *testing.T) {
	_, err := NewCommandPublisher([]string{filepath.Join(t.TempDir(), "missing-helper")}, time.Second)
	if err == nil || AsError(err).Code != ErrorDependencyMissing || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error=%v", err)
	}
}

func TestCommandPublisherStructuredFailureRoutesBoundedDiagnosticWithoutErrorExposure(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	publisher := commandTestPublisher(t, "structured-error", time.Second)
	_, err := publisher.Create(context.Background(), commandTestRequest())
	if err == nil || AsError(err).Code != ErrorAuthRequired || !strings.Contains(err.Error(), "sign in") {
		t.Fatalf("error=%v", err)
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), "SECRET-STDERR") {
			t.Fatalf("stderr leaked through error chain: %v", current)
		}
	}
	if !strings.Contains(logs.String(), "SECRET-STDERR") || logs.Len() > maxDiagnosticLogBytes+512 || !strings.Contains(logs.String(), "truncated") {
		t.Fatalf("diagnostic log was not routed with a bound: bytes=%d log=%q", logs.Len(), logs.String())
	}
}

func TestCommandPublisherRejectsMalformedAndOversizedStdout(t *testing.T) {
	for _, test := range []struct {
		mode string
		want string
	}{
		{mode: "malformed", want: "malformed JSON"},
		{mode: "oversized", want: "exceeded 1 MiB"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			publisher := commandTestPublisher(t, test.mode, time.Second)
			_, err := publisher.Capabilities(context.Background())
			if err == nil || AsError(err).Code != ErrorProtocol || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCommandPublisherRejectsInvalidHelperResult(t *testing.T) {
	for _, mode := range []string{"bad-result-id", "bad-result-url"} {
		t.Run(mode, func(t *testing.T) {
			publisher := commandTestPublisher(t, mode, time.Second)
			_, err := publisher.Create(context.Background(), commandTestRequest())
			if err == nil || AsError(err).Code != ErrorProtocol {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCommandPublisherCreatesPrivateBundleAndAlwaysCleansIt(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("TERM_LLM_SHARE_HELPER_CAPTURE", capture)
	publisher := commandTestPublisher(t, "bundle", time.Second)
	result, err := publisher.Create(context.Background(), commandTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.Provider != "test-helper" || result.Visibility != VisibilityPrivate {
		t.Fatalf("result=%+v", result)
	}
	var got struct {
		CWD        string            `json:"cwd"`
		DirMode    uint32            `json:"dir_mode"`
		FileModes  map[string]uint32 `json:"file_modes"`
		Contents   map[string]string `json:"contents"`
		Entrypoint string            `json:"entrypoint"`
		Names      []string          `json:"names"`
		Prefix     string            `json:"prefix"`
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.DirMode != 0o700 || got.FileModes["index.html"] != 0o600 || got.FileModes["session.md"] != 0o600 {
		t.Fatalf("permissions=%#o files=%#v", got.DirMode, got.FileModes)
	}
	if got.Contents["index.html"] != "<h1>hello</h1>" || got.Contents["session.md"] != "# hello" {
		t.Fatalf("contents=%#v", got.Contents)
	}
	if got.Entrypoint != "index.html" || strings.Join(got.Names, ",") != "index.html,session.md" || got.Prefix != "prefix argument" {
		t.Fatalf("manifest=%+v", got)
	}
	if _, err := os.Stat(got.CWD); !os.IsNotExist(err) {
		t.Fatalf("temporary bundle still exists: stat error=%v", err)
	}
}

func TestCommandPublisherResolvesRelativePATHExecutableBeforeBundleCWD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relative executable symlink test is Unix-specific")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	helper := filepath.Join(binDir, "term-llm-relative-share-helper")
	if err := os.Symlink(os.Args[0], helper); err != nil {
		t.Skipf("cannot create helper symlink: %v", err)
	}
	relativeBin, err := filepath.Rel(cwd, binDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", relativeBin)
	t.Setenv("TERM_LLM_SHARE_HELPER", "1")
	t.Setenv("TERM_LLM_SHARE_HELPER_MODE", "valid")
	publisher, err := NewCommandPublisher([]string{filepath.Base(helper), "-test.run=^TestCommandHelperProcess$", "--", "prefix argument"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(publisher.argv[0]) {
		t.Fatalf("resolved argv[0] = %q, want absolute", publisher.argv[0])
	}
	created, err := publisher.Create(context.Background(), commandTestRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := publisher.Update(context.Background(), created.ID, commandTestRequest())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if created.ID != "opaque-1" || updated.ID != created.ID {
		t.Fatalf("create/update results = %+v / %+v", created, updated)
	}
}

func TestCommandPublisherAcceptsZeroExitAfterWaitDelayAndParsesStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pipe-retaining descendant test is Unix-specific")
	}
	publisher := commandTestPublisher(t, "background-pipes", 3*time.Second)
	pidFile := filepath.Join(t.TempDir(), "background.pid")
	t.Setenv("TERM_LLM_SHARE_HELPER_CAPTURE", pidFile)
	started := time.Now()
	capabilities, err := publisher.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if capabilities.Provider.ID != "test-helper" || time.Since(started) >= 4*time.Second {
		t.Fatalf("capabilities=%+v elapsed=%s", capabilities, time.Since(started))
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("background descendant %d survived successful helper cleanup", pid)
	}
}

func TestCommandPublisherRejectsUnsafeStructuredErrorMessages(t *testing.T) {
	for _, mode := range []string{"control-error", "long-error"} {
		t.Run(mode, func(t *testing.T) {
			publisher := commandTestPublisher(t, mode, time.Second)
			_, err := publisher.Create(context.Background(), commandTestRequest())
			if err == nil || err.Error() != "share helper failed" || strings.Contains(err.Error(), "unsafe") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCommandPublisherRemovesBundleAfterProviderFailure(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("TERM_LLM_SHARE_HELPER_CAPTURE", capture)
	publisher := commandTestPublisher(t, "bundle-error", time.Second)
	_, err := publisher.Create(context.Background(), commandTestRequest())
	if err == nil || AsError(err).Code != ErrorProvider {
		t.Fatalf("error=%v", err)
	}
	var got struct {
		CWD string `json:"cwd"`
	}
	data, readErr := os.ReadFile(capture)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if json.Unmarshal(data, &got) != nil || got.CWD == "" {
		t.Fatalf("capture=%s", data)
	}
	if _, statErr := os.Stat(got.CWD); !os.IsNotExist(statErr) {
		t.Fatalf("temporary bundle still exists after failure: %v", statErr)
	}
}

func TestCommandPublisherTimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group assertion is Unix-specific")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("TERM_LLM_SHARE_HELPER_CAPTURE", pidFile)
	publisher := commandTestPublisher(t, "timeout", 100*time.Millisecond)
	_, err := publisher.Create(context.Background(), commandTestRequest())
	if err == nil || AsError(err).Code != ErrorTimeout {
		t.Fatalf("error=%v", err)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived timeout cleanup", pid)
}

func processAlive(pid int) bool {
	if killErr := syscall.Kill(pid, 0); killErr != nil {
		return false
	}
	// A killed orphan may remain briefly as a zombie until PID 1 reaps it; it
	// can no longer execute and therefore counts as cleaned up.
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}

func TestValidateResultURLAndIDRules(t *testing.T) {
	valid := Result{ID: "opaque-123", URL: "https://example.test/share", SourceURL: "http://127.0.0.1:8080/source", Visibility: VisibilityPrivate}
	if err := ValidateResult(valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(*Result)
	}{
		{name: "space in id", edit: func(r *Result) { r.ID = "bad id" }},
		{name: "long id", edit: func(r *Result) { r.ID = strings.Repeat("x", 257) }},
		{name: "relative url", edit: func(r *Result) { r.URL = "/share" }},
		{name: "public http", edit: func(r *Result) { r.URL = "http://example.test/share" }},
		{name: "url whitespace", edit: func(r *Result) { r.URL = "https://example.test/a b" }},
		{name: "long url", edit: func(r *Result) { r.URL = "https://example.test/" + strings.Repeat("x", 2049) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.edit(&result)
			if err := ValidateResult(result); err == nil {
				t.Fatalf("invalid result accepted: %+v", result)
			}
		})
	}
}

func TestValidateRequestBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Request)
	}{
		{name: "too many files", edit: func(r *Request) {
			for len(r.Files) <= MaxRequestFiles {
				r.Files = append(r.Files, File{Name: fmt.Sprintf("extra-%d.txt", len(r.Files)), MediaType: "text/plain", Role: "attachment"})
			}
		}},
		{name: "oversized file", edit: func(r *Request) { r.Files[0].Content = make([]byte, MaxRequestFileBytes+1) }},
		{name: "oversized bundle", edit: func(r *Request) {
			r.Files[0].Content = make([]byte, MaxRequestFileBytes)
			r.Files[1].Content = make([]byte, MaxRequestFileBytes)
			r.Files = append(r.Files, File{Name: "extra.txt", MediaType: "text/plain", Role: "attachment", Content: []byte{1}})
		}},
		{name: "control title", edit: func(r *Request) { r.Title = "bad\ntitle" }},
		{name: "control file name", edit: func(r *Request) { r.Files[0].Name = "bad\nname" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := commandTestRequest()
			test.edit(&req)
			if err := ValidateRequest(req); err == nil {
				t.Fatalf("invalid request accepted")
			}
		})
	}
}

func TestCommandHelperProcess(t *testing.T) {
	if os.Getenv("TERM_LLM_SHARE_HELPER") != "1" {
		return
	}
	operation := os.Args[len(os.Args)-1]
	mode := os.Getenv("TERM_LLM_SHARE_HELPER_MODE")
	var input struct {
		Protocol   string `json:"protocol"`
		Version    int    `json:"version"`
		RequestID  string `json:"request_id"`
		Entrypoint string `json:"entrypoint"`
		Files      []File `json:"files"`
	}
	_ = json.NewDecoder(os.Stdin).Decode(&input)
	if input.Protocol != Protocol || input.Version != Version || input.RequestID == "" {
		fmt.Fprintln(os.Stderr, "bad protocol input")
		os.Exit(3)
	}

	if operation == "capabilities" {
		switch mode {
		case "malformed":
			fmt.Print("not-json")
			os.Exit(0)
		case "oversized":
			fmt.Print(strings.Repeat("x", int(commandStdoutLimit)+1))
			os.Exit(0)
		}
		capabilities := Capabilities{
			Protocol: Protocol, Version: Version,
			Provider:     Provider{ID: "test-helper", Name: "Test Helper"},
			Operations:   []Operation{OperationCreate, OperationUpdate},
			Visibilities: []Visibility{VisibilityPrivate}, DefaultVisibility: VisibilityPrivate,
		}
		if mode == "bad-protocol" {
			capabilities.Protocol = "other"
		}
		if mode == "missing-create" {
			capabilities.Operations = []Operation{OperationUpdate}
		}
		if mode == "missing-visibility" {
			capabilities.Visibilities = nil
		}
		if mode == "github-id" {
			capabilities.Provider.ID = ProviderGitHub
		}
		if mode == "long-provider-name" {
			capabilities.Provider.Name = strings.Repeat("n", maxProviderNameBytes+1)
		}
		if mode == "control-provider-help" {
			capabilities.Provider.Help = "unsafe\nhelp"
		}
		if mode == "too-many-notes" {
			capabilities.Notes = make([]string, maxCapabilityNotes+1)
			for i := range capabilities.Notes {
				capabilities.Notes[i] = "note"
			}
		}
		if mode == "oversized-limits" {
			capabilities.Limits = map[string]any{"description": strings.Repeat("x", maxCapabilityLimitsBytes)}
		}
		if mode == "background-pipes" {
			child := exec.Command("sh", "-c", "sleep 30")
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				os.Exit(8)
			}
			_ = os.WriteFile(os.Getenv("TERM_LLM_SHARE_HELPER_CAPTURE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		_ = json.NewEncoder(os.Stdout).Encode(capabilities)
		os.Exit(0)
	}

	switch mode {
	case "structured-error":
		fmt.Fprint(os.Stderr, strings.Repeat("SECRET-STDERR", 7000))
		fmt.Print(`{"error":{"code":"auth_required","message":"sign in to the helper"}}`)
		os.Exit(7)
	case "control-error":
		fmt.Print("{\"error\":{\"code\":\"provider_error\",\"message\":\"unsafe\\nmessage\"}}")
		os.Exit(7)
	case "long-error":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": map[string]any{"code": ErrorProvider, "message": strings.Repeat("unsafe", maxStructuredErrorBytes)}})
		os.Exit(7)
	case "timeout":
		capture := os.Getenv("TERM_LLM_SHARE_HELPER_CAPTURE")
		child := exec.Command("sh", "-c", "echo $$ > \"$1\"; exec sleep 30", "share-child", capture)
		if err := child.Start(); err != nil {
			os.Exit(8)
		}
		_ = child.Wait()
		return
	case "bundle", "bundle-error":
		info, _ := os.Stat(".")
		capture := struct {
			CWD        string            `json:"cwd"`
			DirMode    uint32            `json:"dir_mode"`
			FileModes  map[string]uint32 `json:"file_modes"`
			Contents   map[string]string `json:"contents"`
			Entrypoint string            `json:"entrypoint"`
			Names      []string          `json:"names"`
			Prefix     string            `json:"prefix"`
		}{DirMode: uint32(info.Mode().Perm()), FileModes: map[string]uint32{}, Contents: map[string]string{}, Entrypoint: input.Entrypoint, Prefix: os.Args[len(os.Args)-2]}
		capture.CWD, _ = os.Getwd()
		for _, file := range input.Files {
			fileInfo, _ := os.Stat(file.Name)
			content, _ := os.ReadFile(file.Name)
			capture.FileModes[file.Name] = uint32(fileInfo.Mode().Perm())
			capture.Contents[file.Name] = string(content)
			capture.Names = append(capture.Names, file.Name)
		}
		data, _ := json.Marshal(capture)
		_ = os.WriteFile(os.Getenv("TERM_LLM_SHARE_HELPER_CAPTURE"), data, 0o600)
		if mode == "bundle-error" {
			fmt.Print(`{"error":{"code":"provider_error","message":"provider rejected bundle"}}`)
			os.Exit(9)
		}
	}
	response := helperResponse{
		Protocol: Protocol, Version: Version, ID: "opaque-1", URL: "https://example.test/share/1",
		SourceURL: "http://localhost:8080/source/1", Visibility: VisibilityPrivate,
	}
	if mode == "bad-result-id" {
		response.ID = "bad id"
	}
	if mode == "bad-result-url" {
		response.URL = "http://example.test/not-loopback"
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	os.Exit(0)
}
