package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

// mockShellExecutor stands in for the browser's v86 Linux guest. Native tests
// never execute guest commands; they assert the frozen request/result shapes
// and the workspace effects Go applies.
type mockShellExecutor struct {
	mu       sync.Mutex
	raw      []string
	requests []map[string]any
	fn       func(request map[string]any) (string, error)
}

func (m *mockShellExecutor) RunShell(_ context.Context, requestJSON string) (string, error) {
	var request map[string]any
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.raw = append(m.raw, requestJSON)
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if m.fn == nil {
		return "", errors.New("mock executor has no result")
	}
	return m.fn(request)
}

func (m *mockShellExecutor) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *mockShellExecutor) lastRequest(t *testing.T) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		t.Fatal("mock executor was never called")
	}
	return m.requests[len(m.requests)-1]
}

func (m *mockShellExecutor) lastRaw(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.raw) == 0 {
		t.Fatal("mock executor was never called")
	}
	return m.raw[len(m.raw)-1]
}

func requestFiles(t *testing.T, request map[string]any) map[string]string {
	t.Helper()
	raw, ok := request["files"].(map[string]any)
	if !ok {
		t.Fatalf("request files = %#v, want object", request["files"])
	}
	files := make(map[string]string, len(raw))
	for name, value := range raw {
		files[name] = value.(string)
	}
	return files
}

func jsonShellResult(t *testing.T, result shellResult) string {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func executeShell(t *testing.T, app *App, arguments string) (string, bool) {
	t.Helper()
	output, err := app.shell.Execute(context.Background(), json.RawMessage(arguments))
	if err != nil {
		t.Fatalf("Execute returned transport error: %v", err)
	}
	return output.Content, output.IsError
}

func TestShellToolRunsAndSyncsWorkspace(t *testing.T) {
	app, workspace := newTestApp(t, &scriptedInference{t: t}, map[string]string{
		"README.md":           "hello workspace\n",
		"playground/run.sh":   "echo old\n",
		"playground/keep.txt": "keep me\n",
	})
	mock := &mockShellExecutor{}
	mock.fn = func(request map[string]any) (string, error) {
		if got := request["command"]; got != "sh playground/run.sh" {
			t.Fatalf("command = %#v", got)
		}
		if got := request["working_dir"]; got != "/workspace" {
			t.Fatalf("working_dir = %#v, want /workspace", got)
		}
		if got := request["timeout_seconds"]; got != float64(10) {
			t.Fatalf("timeout_seconds = %#v, want 10", got)
		}
		files := requestFiles(t, request)
		if files["README.md"] != "hello workspace\n" || files["playground/run.sh"] != "echo old\n" {
			t.Fatalf("request snapshot = %#v", files)
		}
		files["playground/run.sh"] = "echo new\n"
		files["playground/result.txt"] = "guest output\n"
		delete(files, "playground/keep.txt")
		return jsonShellResult(t, shellResult{
			Stdout: "all good\n", Stderr: "a warning\n", ExitCode: 0, Files: files,
		}), nil
	}
	app.SetShellExecutor(mock)

	content, isError := executeShell(t, app, `{"command":"sh playground/run.sh"}`)
	if isError {
		t.Fatalf("tool reported an error: %s", content)
	}
	for _, want := range []string{
		"exit_code: 0", "duration_ms:", "working_dir: /workspace",
		"2 written, 1 deleted, 1 unchanged", "all good", "a warning",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("output missing %q:\n%s", want, content)
		}
	}
	if raw := mock.lastRaw(t); strings.Contains(raw, filepath.ToSlash(workspace)) {
		t.Fatalf("host workspace path leaked to the guest request: %s", raw)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "playground", "result.txt"))
	if err != nil || string(got) != "guest output\n" {
		t.Fatalf("result.txt = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(workspace, "playground", "run.sh"))
	if err != nil || string(got) != "echo new\n" {
		t.Fatalf("run.sh = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "playground", "keep.txt")); !os.IsNotExist(err) {
		t.Fatalf("keep.txt should have been deleted, stat err = %v", err)
	}
}

func TestShellWorkingDirNormalization(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		dir     string
		want    string
		wantErr bool
	}{
		{name: "default", dir: "", want: "/workspace"},
		{name: "guest root", dir: "/workspace", want: "/workspace"},
		{name: "guest root trailing slash", dir: "/workspace/", want: "/workspace"},
		{name: "current dir", dir: ".", want: "/workspace"},
		{name: "guest subdir", dir: "/workspace/playground", want: "/workspace/playground"},
		{name: "guest nested", dir: "/workspace/a/b", want: "/workspace/a/b"},
		{name: "relative", dir: "playground", want: "/workspace/playground"},
		{name: "host root", dir: root, want: "/workspace"},
		{name: "host subdir", dir: filepath.Join(root, "playground"), want: "/workspace/playground"},
		{name: "guest traversal", dir: "/workspace/../etc", wantErr: true},
		{name: "host outside", dir: "/etc", wantErr: true},
		{name: "relative traversal", dir: "../escape", wantErr: true},
		{name: "backslash", dir: `playground\escape`, wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := normalizeShellWorkingDir(root, testCase.dir)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("normalizeShellWorkingDir(%q) = %q, want error", testCase.dir, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeShellWorkingDir(%q): %v", testCase.dir, err)
			}
			if got != testCase.want {
				t.Fatalf("normalizeShellWorkingDir(%q) = %q, want %q", testCase.dir, got, testCase.want)
			}
		})
	}
}

func TestShellToolSendsNormalizedGuestDir(t *testing.T) {
	app, _ := newTestApp(t, &scriptedInference{t: t}, map[string]string{"playground/run.sh": "echo hi\n"})
	mock := &mockShellExecutor{}
	mock.fn = func(request map[string]any) (string, error) {
		if got := request["working_dir"]; got != "/workspace/playground" {
			t.Fatalf("working_dir = %#v", got)
		}
		return jsonShellResult(t, shellResult{ExitCode: 0, Files: requestFiles(t, request)}), nil
	}
	app.SetShellExecutor(mock)

	content, isError := executeShell(t, app, `{"command":"sh run.sh","working_dir":"playground"}`)
	if isError {
		t.Fatalf("tool reported an error: %s", content)
	}
	if !strings.Contains(content, "working_dir: /workspace/playground") {
		t.Fatalf("output missing normalized working dir:\n%s", content)
	}
}

func TestShellToolLeavesBinaryFilesUntouched(t *testing.T) {
	binary := string([]byte{0xff, 0xfe, 0x00, 0x01, 0x80})
	app, workspace := newTestApp(t, &scriptedInference{t: t}, map[string]string{
		"blob.bin": binary,
		"keep.txt": "keep\n",
	})
	mock := &mockShellExecutor{}
	mock.fn = func(request map[string]any) (string, error) {
		files := requestFiles(t, request)
		if _, ok := files["blob.bin"]; ok {
			t.Fatal("non-UTF-8 file was sent to the guest")
		}
		files["out.txt"] = "ok\n"
		return jsonShellResult(t, shellResult{ExitCode: 0, Files: files}), nil
	}
	app.SetShellExecutor(mock)

	content, isError := executeShell(t, app, `{"command":"echo ok > out.txt"}`)
	if isError {
		t.Fatalf("tool reported an error: %s", content)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "blob.bin"))
	if err != nil || string(got) != binary {
		t.Fatalf("binary file changed across the sync: %v, %v", got, err)
	}
}

func TestShellToolRejectsInvalidReturnedPathsBeforeApply(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "parent traversal", path: "../escape.txt"},
		{name: "absolute", path: "/etc/passwd"},
		{name: "nested traversal", path: "nested/../../escape.txt"},
		{name: "backslash", path: `nested\escape.txt`},
		{name: "not canonical", path: "./escape.txt"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			app, workspace := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
			mock := &mockShellExecutor{}
			mock.fn = func(request map[string]any) (string, error) {
				files := requestFiles(t, request)
				files["good.txt"] = "must not be applied\n"
				files[testCase.path] = "escape\n"
				return jsonShellResult(t, shellResult{ExitCode: 0, Files: files}), nil
			}
			app.SetShellExecutor(mock)

			content, isError := executeShell(t, app, `{"command":"echo hi"}`)
			if !isError || !strings.Contains(content, "rejected guest file map") {
				t.Fatalf("expected rejected file map, got isError=%v:\n%s", isError, content)
			}
			if _, err := os.Stat(filepath.Join(workspace, "good.txt")); !os.IsNotExist(err) {
				t.Fatal("valid sibling file was applied before the invalid path was rejected")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(workspace), "escape.txt")); !os.IsNotExist(err) {
				t.Fatal("guest escaped the workspace on the host")
			}
		})
	}
}

func TestShellToolRejectsUnboundedReturnedMaps(t *testing.T) {
	t.Run("too many files", func(t *testing.T) {
		app, _ := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
		mock := &mockShellExecutor{}
		mock.fn = func(request map[string]any) (string, error) {
			files := map[string]string{}
			for i := 0; i < maxShellFiles+1; i++ {
				files[filepath.ToSlash(filepath.Join("generated", "file-"+string(rune('a'+i%26))+string(rune('a'+i/26))))] = "x"
			}
			return jsonShellResult(t, shellResult{ExitCode: 0, Files: files}), nil
		}
		app.SetShellExecutor(mock)

		content, isError := executeShell(t, app, `{"command":"echo hi"}`)
		if !isError || !strings.Contains(content, "the limit is 256") {
			t.Fatalf("expected file-count rejection, got isError=%v:\n%s", isError, content)
		}
	})

	t.Run("too many bytes", func(t *testing.T) {
		app, _ := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
		mock := &mockShellExecutor{}
		mock.fn = func(request map[string]any) (string, error) {
			return jsonShellResult(t, shellResult{
				ExitCode: 0,
				Files:    map[string]string{"huge.txt": strings.Repeat("x", maxShellFileBytes+1)},
			}), nil
		}
		app.SetShellExecutor(mock)

		content, isError := executeShell(t, app, `{"command":"echo hi"}`)
		if !isError || !strings.Contains(content, "bytes of file content") {
			t.Fatalf("expected byte-limit rejection, got isError=%v:\n%s", isError, content)
		}
	})
}

func TestShellToolNonzeroExitIsError(t *testing.T) {
	app, _ := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
	mock := &mockShellExecutor{}
	mock.fn = func(request map[string]any) (string, error) {
		return jsonShellResult(t, shellResult{
			Stdout: "ok: add 2 3 => 5\n", Stderr: "FAIL: average => 3 (expected 4)\n", ExitCode: 7,
			Files: requestFiles(t, request),
		}), nil
	}
	app.SetShellExecutor(mock)

	content, isError := executeShell(t, app, `{"command":"sh playground/tests.sh"}`)
	if !isError {
		t.Fatalf("nonzero exit_code must mark the tool output as an error:\n%s", content)
	}
	for _, want := range []string{"exit_code: 7", "FAIL: average => 3 (expected 4)", "ok: add 2 3 => 5", "duration_ms:"} {
		if !strings.Contains(content, want) {
			t.Fatalf("output missing %q:\n%s", want, content)
		}
	}
}

func TestShellToolExecutorFailureIsError(t *testing.T) {
	app, workspace := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
	mock := &mockShellExecutor{fn: func(map[string]any) (string, error) {
		return "", errors.New("VM worker terminated")
	}}
	app.SetShellExecutor(mock)

	content, isError := executeShell(t, app, `{"command":"echo hi"}`)
	if !isError || !strings.Contains(content, "guest: VM worker terminated") {
		t.Fatalf("expected guest failure, got isError=%v:\n%s", isError, content)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "keep.txt")); err != nil || string(got) != "keep\n" {
		t.Fatalf("workspace changed after executor failure: %q, %v", got, err)
	}
}

func TestShellToolArgumentValidation(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{name: "missing command", arguments: `{}`, want: "command is required"},
		{name: "blank command", arguments: `{"command":"   "}`, want: "command is required"},
		{name: "over max timeout", arguments: `{"command":"echo hi","timeout_seconds":31}`, want: "timeout_seconds must be between 1 and 30"},
		{name: "negative timeout", arguments: `{"command":"echo hi","timeout_seconds":-1}`, want: "timeout_seconds must be between 1 and 30"},
		{name: "window path", arguments: `{"command":"echo hi","working_dir":"C:\\\\Windows"}`, want: "forward slashes"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			app, _ := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
			mock := &mockShellExecutor{fn: func(request map[string]any) (string, error) {
				return jsonShellResult(t, shellResult{ExitCode: 0, Files: requestFiles(t, request)}), nil
			}}
			app.SetShellExecutor(mock)

			content, isError := executeShell(t, app, testCase.arguments)
			if !isError || !strings.Contains(content, testCase.want) {
				t.Fatalf("got isError=%v:\n%s", isError, content)
			}
			if mock.count() != 0 {
				t.Fatalf("invalid arguments reached the guest %d times", mock.count())
			}
		})
	}
}

func TestShellToolTruncatesHugeOutput(t *testing.T) {
	app, _ := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
	mock := &mockShellExecutor{}
	mock.fn = func(request map[string]any) (string, error) {
		return jsonShellResult(t, shellResult{
			Stdout: strings.Repeat("a", maxShellStdoutSize+100),
			Stderr: strings.Repeat("b", maxShellStderrSize+100),
			Files:  requestFiles(t, request),
		}), nil
	}
	app.SetShellExecutor(mock)

	content, isError := executeShell(t, app, `{"command":"yes"}`)
	if isError {
		t.Fatalf("unexpected error:\n%s", content)
	}
	if !strings.Contains(content, "[truncated 100 bytes]") {
		t.Fatalf("large streams were not truncated:\n%.200s", content)
	}
	if len(content) > maxShellStdoutSize+maxShellStderrSize+2048 {
		t.Fatalf("tool output = %d bytes, caps were not applied", len(content))
	}
}

func TestBrowserUsesDeveloperPromptAndYoloApproval(t *testing.T) {
	app, workspace := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
	if workspace == "" {
		t.Fatal("missing workspace")
	}
	if got := app.approval.ApprovalMode(); got != tools.ModeYolo {
		t.Fatalf("approval mode = %v, want yolo", got)
	}
	if !app.approval.YoloMode {
		t.Fatal("approval manager YoloMode mirror is not set")
	}

	var hasShell bool
	for _, spec := range app.toolSpecs() {
		if spec.Name == tools.ShellToolName {
			hasShell = true
			for _, want := range []string{"BusyBox", "v86", "not the host shell", "CPython 3.12", "python3", "sqlite3", "no pip", "node and Go are not installed"} {
				if !strings.Contains(spec.Description, want) {
					t.Fatalf("shell description missing %q: %s", want, spec.Description)
				}
			}
		}
	}
	if !hasShell {
		t.Fatal("shell is missing from the provider tool surface")
	}

	prompt := textParts(buildCoreMessages(nil, app.workspace)[0].Parts)
	for _, want := range []string{
		"You are a developer agent focused on implementing code changes",
		"browser-bonsai",
		"BusyBox",
		"v86",
		"CPython 3.12",
		"sqlite3",
		"no pip",
		"YOLO",
		"no network",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("developer prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{"{{git_repo}}", "Available subagents you can fire", "QuickJS", "run_javascript", "require('fs')"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("developer prompt still contains %q", unwanted)
		}
	}
}

func TestSessionMetadataReportsDeveloperAgentAndYolo(t *testing.T) {
	app, _ := newTestApp(t, &scriptedInference{t: t}, map[string]string{"keep.txt": "keep\n"})
	sessionID := createSession(t, app)

	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/demo-api/v1/sessions?limit=30", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("sessions status = %d", recorder.Code)
	}
	var list struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, session := range list.Sessions {
		if session["id"] == sessionID {
			found = session
		}
	}
	if found == nil {
		t.Fatalf("session %s missing from list", sessionID)
	}
	for field, want := range map[string]string{
		"agent":                   "developer",
		"approval_default_mode":   "yolo",
		"approval_effective_mode": "yolo",
		"provider":                demoProviderName,
		"model":                   demoModelName,
	} {
		if got := found[field]; got != want {
			t.Fatalf("%s = %#v, want %q", field, got, want)
		}
	}

	state := httptest.NewRecorder()
	app.Handler().ServeHTTP(state, httptest.NewRequest(http.MethodGet, "/demo-api/v1/sessions/"+sessionID+"/state", nil))
	if !strings.Contains(state.Body.String(), `"effective_mode":"yolo"`) {
		t.Fatalf("state approval policy is not yolo: %s", state.Body.String())
	}
}

func TestEngineRunsShellAndFileChangesAreVisible(t *testing.T) {
	const fixed = "add() {\n  echo $(( $1 + $2 ))\n}\n"
	workspaceSeed := map[string]string{
		"playground/calculate.sh": "add() {\n  echo $(( $1 - $2 ))\n}\n",
		"playground/tests.sh":     "sh ./playground/calculate.sh add 2 3\n",
	}
	inference := &scriptedInference{t: t, turns: []scriptedTurn{
		{
			check: func(t *testing.T, request map[string]any) {
				var names []string
				for _, entry := range request["tools"].([]any) {
					fn := entry.(map[string]any)["function"].(map[string]any)
					names = append(names, fn["name"].(string))
				}
				if !containsString(names, tools.ShellToolName) {
					t.Fatalf("provider tools = %v, want shell", names)
				}
				prompt := roleMessages(request)[0]["content"].(string)
				if !strings.Contains(prompt, "YOLO") || !strings.Contains(prompt, "developer agent") {
					t.Fatalf("system prompt is not the developer/YOLO prompt: %.200s", prompt)
				}
			},
			calls: []scriptedCall{{id: "call_shell", name: tools.ShellToolName, arguments: `{"command":"sh playground/tests.sh"}`}},
		},
		{
			check: func(t *testing.T, request map[string]any) {
				messages := roleMessages(request)
				var toolContent string
				for _, message := range messages {
					if message["role"] == "tool" {
						toolContent = message["content"].(string)
					}
				}
				if !strings.Contains(toolContent, "stdout:\n5\n") {
					t.Fatalf("tool result missing guest stdout: %q", toolContent)
				}
				if !strings.Contains(toolContent, "exit_code: 0") {
					t.Fatalf("tool result missing exit code: %q", toolContent)
				}
			},
			content: "Fixed calculate.sh; the shell tests now pass.",
		},
	}}

	app, workspace := newTestApp(t, inference, workspaceSeed)
	mock := &mockShellExecutor{}
	mock.fn = func(request map[string]any) (string, error) {
		files := requestFiles(t, request)
		if got := files["playground/calculate.sh"]; !strings.Contains(got, "$1 - $2") {
			t.Fatalf("guest did not receive the broken script: %q", got)
		}
		files["playground/calculate.sh"] = fixed
		return jsonShellResult(t, shellResult{
			Stdout: "5\n", ExitCode: 0, Files: files,
		}), nil
	}
	app.SetShellExecutor(mock)

	sessionID := createSession(t, app)
	recorder := postResponse(t, app, sessionID, "Run the playground tests, fix calculate.sh, and run them again.")
	if recorder.Code != http.StatusOK {
		t.Fatalf("response status = %d: %s", recorder.Code, recorder.Body.String())
	}
	events := sseEvents(t, recorder.Body.String())
	if len(events["response.tool_exec.end"]) != 1 {
		t.Fatalf("tool_exec.end count = %d, want 1", len(events["response.tool_exec.end"]))
	}
	if success, _ := events["response.tool_exec.end"][0]["success"].(bool); !success {
		t.Fatalf("tool execution reported failure: %#v", events["response.tool_exec.end"][0])
	}
	got, err := os.ReadFile(filepath.Join(workspace, "playground", "calculate.sh"))
	if err != nil || string(got) != fixed {
		t.Fatalf("calculate.sh after shell = %q, %v", got, err)
	}
	// A subsequent real read_file call must see the guest's write.
	readTool, ok := app.registry.Get(tools.ReadFileToolName)
	if !ok {
		t.Fatal("read_file tool is not registered")
	}
	readOutput, err := readTool.Execute(context.Background(), json.RawMessage(`{"path":"playground/calculate.sh"}`))
	if err != nil || readOutput.IsError {
		t.Fatalf("read_file after shell = %#v, %v", readOutput, err)
	}
	if !strings.Contains(readOutput.Content, "$1 + $2") {
		t.Fatalf("read_file did not see the guest write:\n%s", readOutput.Content)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
