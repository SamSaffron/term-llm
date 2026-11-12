package agyproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func promptBody(texts ...string) []byte {
	contents := make([]any, 0, len(texts))
	for _, text := range texts {
		contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}})
	}
	body, _ := json.Marshal(map[string]any{"request": map[string]any{"contents": contents}})
	return body
}

func promptTexts(t *testing.T, body []byte) []string {
	t.Helper()
	root, err := decodeGenerationRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, raw := range root["request"].(map[string]any)["contents"].([]any) {
		for _, part := range raw.(map[string]any)["parts"].([]any) {
			texts = append(texts, part.(map[string]any)["text"].(string))
		}
	}
	return texts
}

func stageTestPrompt(t *testing.T, home, original string) string {
	t.Helper()
	wire, err := StagePrompt(home, original)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func stagedPath(home, original string) string {
	return filepath.Join(home, promptDirectory, fmt.Sprintf("%x.txt", sha256.Sum256([]byte(original))))
}

func TestStagePromptPrivatePersistentAndIdempotent(t *testing.T) {
	home, original := t.TempDir(), "exact\n用户 🌍\n\x00tail\n"
	wire := stageTestPrompt(t, home, original)
	want := fmt.Sprintf("<term_llm_prompt sha256=\"%x\">\n%s\n</term_llm_prompt>", sha256.Sum256([]byte(original)), original)
	if wire != want || stageTestPrompt(t, home, original) != wire {
		t.Fatal("unexpected or non-idempotent wire envelope")
	}
	for path, mode := range map[string]os.FileMode{filepath.Join(home, promptDirectory): 0o700, stagedPath(home, original): 0o600} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private mode for %s: info=%v err=%v", path, info, err)
		}
	}
	got, err := os.ReadFile(stagedPath(home, original))
	if err != nil || string(got) != original {
		t.Fatalf("staged prompt changed: %q err=%v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(home, promptDirectory))
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected staging debris: %v %v", entries, err)
	}
}

func TestPromptRestorationFullAndMultiMiB(t *testing.T) {
	for _, original := range []string{"", "hello\n用户🌍\n", strings.Repeat("long user prompt 🌍\n", 150000) + "TAIL INSTRUCTION"} {
		t.Run(fmt.Sprint(len(original)), func(t *testing.T) {
			home := t.TempDir()
			wire := stageTestPrompt(t, home, original)
			metadata := "\n<ADDITIONAL_METADATA>\nThe current local time is: now\n</ADDITIONAL_METADATA>"
			for _, wrapper := range []bool{false, true} {
				input, want := wire, original
				if wrapper {
					input, want = userRequestOpen+wire+userRequestClose+metadata, userRequestOpen+original+userRequestClose+metadata
				}
				out, _, err := processGenerationRequestWithPrompts(promptBody(input), "", home, true, maxRequestBody)
				if err != nil {
					t.Fatal(err)
				}
				if got := promptTexts(t, out)[0]; got != want {
					t.Fatalf("restored text mismatch: got %d bytes want %d", len(got), len(want))
				}
			}
		})
	}
}

func TestPromptRestorationTruncatedHistoryAcrossServers(t *testing.T) {
	home := t.TempDir()
	original := "<workspace>\n" + strings.Repeat("large input\n", 30000) + "TAIL INSTRUCTION"
	wire := stageTestPrompt(t, home, original)
	cut := (userRequestOpen + wire)[:192000] + "\n<truncated 129114 bytes>"
	second := stageTestPrompt(t, home, "follow-up")
	body := promptBody(cut, userRequestOpen+second+userRequestClose)
	for range 2 { // a fresh proxy instance reads the persisted history again
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		called := false
		transport.RegisterProtocol("https", roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			out, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			got := promptTexts(t, out)
			if got[0] != userRequestOpen+original+userRequestClose || got[1] != userRequestOpen+"follow-up"+userRequestClose {
				t.Fatal("historical truncated prompt or follow-up was not restored exactly")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}))
		server := &Server{transport: transport, promptRoot: home, requireMCP: true}
		recorder := httptest.NewRecorder()
		server.forward(recorder, httptest.NewRequest(http.MethodPost, "https://"+CloudCodeHost+generationPath, bytes.NewReader(body)))
		if !called || recorder.Code != http.StatusOK {
			t.Fatalf("called=%v code=%d body=%s", called, recorder.Code, recorder.Body.String())
		}
	}
}

func TestPromptRestorationUnicodeByteCuts(t *testing.T) {
	home, original := t.TempDir(), "ASCII 用户 🌍 \ufffd\ufffd🌍 tail"
	wire := stageTestPrompt(t, home, original)
	for _, wrapped := range []bool{false, true} {
		text, want := wire, original
		if wrapped {
			text, want = userRequestOpen+wire+userRequestClose, userRequestOpen+original+userRequestClose
		}
		for cut := strings.Index(text, "ASCII"); cut < len(text); cut++ {
			// Encoders replace either each invalid byte or the whole incomplete
			// UTF-8 sequence when a byte-prefix cut splits a rune.
			for _, captured := range []string{text[:cut], strings.ToValidUTF8(text[:cut], "\ufffd")} {
				input := promptBody(captured + "\n<truncated 12345 bytes>")
				out, _, err := processGenerationRequestWithPrompts(input, "", home, false, maxRequestBody)
				if err != nil {
					t.Fatalf("wrapped=%v cut=%d: %v", wrapped, cut, err)
				}
				if promptTexts(t, out)[0] != want {
					t.Fatalf("wrapped=%v cut=%d: prompt changed", wrapped, cut)
				}
			}
		}
	}
}

func TestPromptUnmarkedAndUnanchoredUnchanged(t *testing.T) {
	home := t.TempDir()
	wire := stageTestPrompt(t, home, "original")
	for _, text := range []string{"unmarked\n<truncated 123 bytes>", "quoted: " + wire, "quoted <USER_REQUEST>\n" + wire, userRequestOpen + "quoted " + wire + userRequestClose} {
		body := promptBody(text)
		out, _, err := processGenerationRequestWithPrompts(body, "", home, true, maxRequestBody)
		if err != nil || !bytes.Equal(out, body) {
			t.Fatalf("untrusted/unmarked text changed: %q err=%v", text, err)
		}
	}
	body := bytes.Replace(promptBody(wire), []byte(`"role":"user"`), []byte(`"role":"model"`), 1)
	out, _, err := processGenerationRequestWithPrompts(body, "", home, true, maxRequestBody)
	if err != nil || !bytes.Equal(out, body) {
		t.Fatal("model text changed")
	}
}

func TestPromptRestorationRejectsMismatchBeforeUpstream(t *testing.T) {
	original := strings.Repeat("known prefix 用户🌍\n", 100)
	for _, name := range []string{"missing", "corrupt", "hash", "prefix", "payload", "header", "path", "symlink file", "symlink directory", "symlink home", "permissions", "oversized", "missing root", "bad count", "suffix"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			wire := stageTestPrompt(t, home, original)
			text := userRequestOpen + wire + userRequestClose
			path := stagedPath(home, original)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "missing":
				must(os.Remove(path))
			case "corrupt":
				must(os.WriteFile(path, []byte("corrupt"), 0o600))
			case "hash":
				text = strings.Replace(text, fmt.Sprintf("%x", sha256.Sum256([]byte(original))), strings.Repeat("0", 64), 1)
			case "prefix":
				text = strings.Replace((userRequestOpen + wire)[:150], "known", "WRONG", 1) + "\n<truncated 123 bytes>"
			case "payload":
				text = strings.Replace(text, "known", "WRONG", 1)
			case "header":
				text = strings.Replace(text, `sha256="`, `sha256='`, 1)
			case "path":
				text = `<term_llm_prompt sha256="../../outside">` + "\nanything\n</term_llm_prompt>"
			case "symlink file":
				must(os.Remove(path))
				target := filepath.Join(t.TempDir(), "target")
				must(os.WriteFile(target, []byte(original), 0o600))
				must(os.Symlink(target, path))
			case "symlink directory":
				dir := filepath.Join(home, promptDirectory)
				must(os.Rename(dir, dir+"-real"))
				must(os.Symlink(dir+"-real", dir))
			case "symlink home":
				link := filepath.Join(t.TempDir(), "home")
				must(os.Symlink(home, link))
				home = link
			case "permissions":
				must(os.Chmod(path, 0o644))
			case "oversized":
				must(os.Truncate(path, maxPromptSize+1))
			case "missing root":
				home = ""
			case "bad count":
				text = (userRequestOpen + wire)[:150] + "\n<truncated 0 bytes>"
			case "suffix":
				text = wire + "unverified suffix"
			}
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			transport.RegisterProtocol("https", roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("untrusted staged prompt reached upstream")
				return nil, nil
			}))
			server := &Server{transport: transport, promptRoot: home}
			recorder := httptest.NewRecorder()
			server.forward(recorder, httptest.NewRequest(http.MethodPost, "https://"+CloudCodeHost+generationPath, bytes.NewReader(promptBody(text))))
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestStagePromptRejectsUnsafeFiles(t *testing.T) {
	home, original := t.TempDir(), "original"
	stageTestPrompt(t, home, original)
	path := stagedPath(home, original)
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StagePrompt(home, original); err == nil {
		t.Fatal("staging silently overwrote corrupt existing file")
	}
	if _, err := StagePrompt(home, "invalid\xff"); err == nil {
		t.Fatal("invalid UTF-8 prompt accepted")
	}
	if _, err := StagePrompt("", original); err == nil {
		t.Fatal("empty home accepted")
	}
}

func TestPromptExpansionSizeCheckedBeforeFiltering(t *testing.T) {
	home := t.TempDir()
	wire := stageTestPrompt(t, home, strings.Repeat("<", 10000))
	truncated := wire[:150] + "\n<truncated 9999 bytes>"
	if _, _, err := processGenerationRequestWithPrompts(promptBody(truncated), "", home, false, 2000); err == nil || !strings.Contains(err.Error(), "oversized expanded") {
		t.Fatalf("expanded JSON size not enforced: %v", err)
	}
}

func TestPromptAndArtifactRootsRemainIndependent(t *testing.T) {
	home, brain := t.TempDir(), t.TempDir()
	wire := stageTestPrompt(t, home, "user prompt")
	artifact := writeArtifact(t, brain, "3", "tool output")
	body := promptBody(userRequestOpen + wire + userRequestClose)
	root, err := decodeGenerationRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	request := root["request"].(map[string]any)
	toolRoot, err := decodeGenerationRequest(generationBodyWithOutput(agySpillNotice(artifact)))
	if err != nil {
		t.Fatal(err)
	}
	request["contents"] = append(request["contents"].([]any), toolRoot["request"].(map[string]any)["contents"].([]any)...)
	body, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := processGenerationRequestWithPrompts(body, brain, home, false, maxRequestBody)
	if err != nil || !bytes.Contains(out, []byte(`"output":"tool output"`)) || strings.Contains(string(out), "term_llm_prompt") {
		t.Fatalf("independent roots did not restore prompt and tool: %s err=%v", out, err)
	}
	server := &Server{}
	server.SetArtifactRoot(brain)
	server.SetPromptRoot(home)
	if server.artifactRoot != brain || server.promptRoot != home {
		t.Fatal("root setters overwrite each other")
	}
	// A staged prompt cannot fall back to the tool artifact directory.
	if _, _, err := processGenerationRequest(body, brain, false, maxRequestBody); err == nil {
		t.Fatal("prompt expanded without a configured prompt root")
	}
}

func TestStagePromptRejectsSymlinksAndNonPrivateDirectories(t *testing.T) {
	for _, name := range []string{"home", "directory", "file", "directory permissions"} {
		t.Run(name, func(t *testing.T) {
			home, original := t.TempDir(), "original"
			stageTestPrompt(t, home, original)
			var err error
			switch name {
			case "home":
				link := filepath.Join(t.TempDir(), "home")
				err = os.Symlink(home, link)
				home = link
			case "directory":
				dir := filepath.Join(home, promptDirectory)
				if err = os.Rename(dir, dir+"-real"); err == nil {
					err = os.Symlink(dir+"-real", dir)
				}
			case "file":
				path := stagedPath(home, original)
				if err = os.Rename(path, path+"-real"); err == nil {
					err = os.Symlink(path+"-real", path)
				}
			case "directory permissions":
				err = os.Chmod(filepath.Join(home, promptDirectory), 0o755)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := StagePrompt(home, original); err == nil {
				t.Fatal("unsafe staging location accepted")
			}
		})
	}
}

func TestPromptRejectsAppendedUserContext(t *testing.T) {
	home := t.TempDir()
	wire := stageTestPrompt(t, home, "original")
	for _, tail := range []string{
		userRequestClose + "\nextra user instructions",
		userRequestClose + "\n<ADDITIONAL_METADATA>\nknown\n</ADDITIONAL_METADATA>\nextra user instructions",
		userRequestClose + "\n<ADDITIONAL_METADATA>\nunterminated",
	} {
		if _, _, err := processGenerationRequestWithPrompts(promptBody(userRequestOpen+wire+tail), "", home, false, maxRequestBody); err == nil {
			t.Fatalf("arbitrary suffix accepted: %q", tail)
		}
	}
}
