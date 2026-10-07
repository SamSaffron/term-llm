package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/backends"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type classifyTestServer struct {
	server   *httptest.Server
	requests []classify.Request
}

func newClassifyTestServer(t *testing.T) *classifyTestServer {
	t.Helper()
	s := &classifyTestServer{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/v1/systemone":
			var req classify.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode request: %v", err)
			}
			s.requests = append(s.requests, req)
			w.Header().Set("Content-Type", "application/json")
			answers := make(map[string]any, len(req.Questions))
			for id, q := range req.Questions {
				switch q.Type {
				case "score":
					answers[id] = map[string]any{"type": "score", "score": 1, "confidence": 0.8, "probabilities": map[string]float64{"1": 1}, "legend": map[string]any{"1": "ok"}}
				case "noul":
					answers[id] = map[string]any{"type": "noul", "noul": 0.75, "confidence": 0.8}
				default:
					answers[id] = map[string]any{"type": "choice", "choice": "yes", "confidence": 0.8, "probabilities": map[string]float64{"yes": 0.8, "no": 0.2}}
				}
			}
			body, err := json.Marshal(map[string]any{"model": "jev-test", "answers": answers, "usage": map[string]int{"input_tokens": 1, "output_tokens": 2}, "extra": true})
			if err != nil {
				t.Fatal(err)
			}
			w.Write(body)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"models":[{"name":"z-model","description":"Zed","release_date":"2026-01-02"},{"name":"a-model","description":"Aye","release_date":"2026-01-01"}],"extra":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func executeClassifyTest(t *testing.T, args []string, in string, stdinData bool, cfg *config.Config, srv *classifyTestServer) (string, error) {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key", BaseURL: srv.server.URL, Model: "jev-config", TimeoutSeconds: 3}}}}
	}
	cmd := newClassifyCmd(classifyDeps{
		loadConfig: func() (*config.Config, error) { return cfg, nil },
		newBackend: func(opts backends.Connection) (classify.Backend, error) {
			return backends.New(opts)
		},
		stdinData: func(*cobra.Command) bool { return stdinData },
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader(in))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClassifyCommandRegisteredOnRoot(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"classify"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Name() != "classify" {
		t.Fatalf("root classify command = %v", cmd)
	}
}

func TestClassifyPositionalStateAndQuestionsJSONWrapper(t *testing.T) {
	srv := newClassifyTestServer(t)
	questions := writeTemp(t, "questions.json", `{"questions":{"result":{"type":"choice","instructions":"Pick yes or no","criteria":{"yes":"Yes","no":"No"}}}}`)
	out, err := executeClassifyTest(t, []string{"hello", "world", "--questions", questions, "--model", "jev-cli"}, "", false, nil, srv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"extra":true`) {
		t.Fatalf("json output did not preserve raw response: %s", out)
	}
	if len(srv.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(srv.requests))
	}
	var state string
	if err := json.Unmarshal(srv.requests[0].State, &state); err != nil || state != "hello world" {
		t.Fatalf("state = %q (err %v), want joined positional", srv.requests[0].State, err)
	}
	if srv.requests[0].Model != "jev-cli" {
		t.Fatalf("model = %q", srv.requests[0].Model)
	}
}

func TestClassifyStateJSONAndYAMLQuestionMap(t *testing.T) {
	srv := newClassifyTestServer(t)
	state := writeTemp(t, "state.json", `{"ticket":123,"tags":["a"]}`)
	questions := writeTemp(t, "questions.yaml", `
result:
  type: choice
  instructions: Is this valid?
  criteria:
    yes: valid
    no: invalid
`)
	_, err := executeClassifyTest(t, []string{"--file", state, "--state-json", "--questions", questions, "--format", "table"}, "", false, nil, srv)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(srv.requests[0].State, &got); err != nil || got["ticket"].(float64) != 123 {
		t.Fatalf("state json = %s err %v", srv.requests[0].State, err)
	}
	if string(srv.requests[0].Questions["result"].Instructions) != `"Is this valid?"` {
		t.Fatalf("instructions = %s", srv.requests[0].Questions["result"].Instructions)
	}
}

func TestClassifyStdinStateAllowsQuestionsFromStdinOnlyWhenNotState(t *testing.T) {
	srv := newClassifyTestServer(t)
	questions := `{"result":{"type":"choice","instructions":"Pick","criteria":{"yes":"","no":""}}}`
	if _, err := executeClassifyTest(t, []string{"--file", "-", "--questions", "-"}, "state", true, nil, srv); err == nil || !strings.Contains(err.Error(), "state also uses stdin") {
		t.Fatalf("expected stdin conflict, got %v", err)
	}
	out, err := executeClassifyTest(t, []string{"state text", "--questions", "-", "--format", "value"}, questions, true, nil, srv)
	if err != nil {
		t.Fatal(err)
	}
	if out != "yes\n" {
		t.Fatalf("value output = %q", out)
	}
}

func TestClassifyRejectsAmbiguousStateBeforeConfigOrNetwork(t *testing.T) {
	calledConfig := false
	calledClient := false
	cmd := newClassifyCmd(classifyDeps{
		loadConfig: func() (*config.Config, error) { calledConfig = true; return nil, nil },
		newBackend: func(backends.Connection) (classify.Backend, error) { calledClient = true; return nil, nil },
		stdinData:  func(*cobra.Command) bool { return true },
	})
	cmd.SetIn(strings.NewReader("stdin"))
	cmd.SetArgs([]string{"positional", "--type", "choice", "--question", "Pick", "--option", "yes", "--option", "no"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous", err)
	}
	if calledConfig || calledClient {
		t.Fatalf("validation touched config/client: config=%v client=%v", calledConfig, calledClient)
	}
}

func TestClassifyRejectsMutuallyExclusiveQuestionFlagsBeforeConfigOrNetwork(t *testing.T) {
	questions := writeTemp(t, "questions.json", `{"result":{"type":"choice","instructions":"Pick","criteria":{"yes":"","no":""}}}`)
	for _, args := range [][]string{
		{"state", "--questions", questions, "--type", "choice", "--question", "Pick", "--option", "yes"},
		{"state", "--type", "choice", "--question", "Pick", "--option", "yes", "--level", "bad"},
	} {
		calledConfig := false
		cmd := newClassifyCmd(classifyDeps{
			loadConfig: func() (*config.Config, error) { calledConfig = true; return nil, nil },
			newBackend: func(backends.Connection) (classify.Backend, error) {
				t.Fatal("client should not be created")
				return nil, nil
			},
			stdinData: func(*cobra.Command) bool { return false },
		})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("%v: expected validation error", args)
		}
		if calledConfig {
			t.Fatalf("%v: validation touched config", args)
		}
	}
}

func TestClassifySugarQuestionTypes(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantType     string
		wantCriteria string
	}{
		{"choice", []string{"state", "--type", "choice", "--question", "Choose", "--option", "yes=Yes", "--option", "no"}, "choice", `{"no":null,"yes":"Yes"}`},
		{"score", []string{"state", "--type", "score", "--question", "Rate", "--level", "bad", "--level", "good"}, "score", `["bad","good"]`},
		{"noul", []string{"state", "--type", "noul", "--question", "Likely?", "--true-description", "true", "--false-description", "false"}, "noul", `{"false":"false","true":"true"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newClassifyTestServer(t)
			if _, err := executeClassifyTest(t, tt.args, "", false, nil, srv); err != nil {
				t.Fatal(err)
			}
			q := srv.requests[0].Questions["result"]
			if q.Type != tt.wantType || string(q.Criteria) != tt.wantCriteria {
				t.Fatalf("question = %#v criteria %s, want %s %s", q, q.Criteria, tt.wantType, tt.wantCriteria)
			}
		})
	}
}

func TestClassifyOutputFileAndAnswerValue(t *testing.T) {
	srv := newClassifyTestServer(t)
	questions := writeTemp(t, "questions.json", `{"result":{"type":"choice","instructions":"Pick","criteria":{"yes":"","no":""}}}`)
	outPath := filepath.Join(t.TempDir(), "out.txt")
	out, err := executeClassifyTest(t, []string{"state", "--questions", questions, "--format", "value", "--answer", "result", "--output", outPath}, "", false, nil, srv)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	data, err := os.ReadFile(outPath)
	if err != nil || string(data) != "yes\n" {
		t.Fatalf("output file = %q err %v", data, err)
	}
}

func TestClassifyModelsJSONTableAndConnectionFlags(t *testing.T) {
	srv := newClassifyTestServer(t)
	cfg := &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key", BaseURL: "http://unused.invalid", TimeoutSeconds: 99}}}}
	out, err := executeClassifyTest(t, []string{"models", "--base-url", srv.server.URL, "--timeout", "2s", "--format", "json"}, "", false, cfg, srv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"extra":true`) {
		t.Fatalf("models json did not preserve raw: %s", out)
	}
	outPath := filepath.Join(t.TempDir(), "models.txt")
	out, err = executeClassifyTest(t, []string{"models", "--base-url", srv.server.URL, "--format", "table", "--output", outPath}, "", false, cfg, srv)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("models stdout = %q, want empty with --output", out)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	out = string(data)
	if !strings.Contains(out, "MODEL") || !strings.Contains(out, "a-model") || strings.Index(out, "a-model") > strings.Index(out, "z-model") {
		t.Fatalf("models table not useful/sorted: %s", out)
	}
}

func TestClassifyPrettyPrintIndentsJSONWithoutChangingContent(t *testing.T) {
	srv := newClassifyTestServer(t)
	args := []string{"state", "--type", "choice", "--question", "Pick", "--option", "yes", "--option", "no"}
	compact, err := executeClassifyTest(t, args, "", false, nil, srv)
	if err != nil {
		t.Fatal(err)
	}
	pretty, err := executeClassifyTest(t, append(append([]string{}, args...), "--pretty-print"), "", false, nil, srv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compact, "\n  ") {
		t.Fatalf("default output should stay compact: %q", compact)
	}
	if !strings.Contains(pretty, "\n  \"answers\": {") || !strings.HasSuffix(pretty, "}\n") {
		t.Fatalf("pretty output not indented: %q", pretty)
	}
	var compactValue, prettyValue any
	if err := json.Unmarshal([]byte(compact), &compactValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(pretty), &prettyValue); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(compactValue) != fmt.Sprint(prettyValue) {
		t.Fatalf("pretty output changed content:\n%s\n%s", compact, pretty)
	}

	models, err := executeClassifyTest(t, []string{"models", "--format", "json", "--pretty-print"}, "", false, nil, srv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(models, "\n  \"models\": [") || !strings.Contains(models, `"extra": true`) {
		t.Fatalf("models pretty output = %q", models)
	}
}

func TestClassifyTimeoutFromConfigAndFlags(t *testing.T) {
	var got []time.Duration
	cmd := newClassifyCmd(classifyDeps{
		loadConfig: func() (*config.Config, error) {
			return &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key", BaseURL: "http://example.test", Model: "jev", TimeoutSeconds: 7}}}}, nil
		},
		newBackend: func(opts backends.Connection) (classify.Backend, error) {
			got = append(got, opts.Timeout)
			return fakeClassifyClient{}, nil
		},
		stdinData: func(*cobra.Command) bool { return false },
	})
	cmd.SetArgs([]string{"state", "--type", "choice", "--question", "Pick", "--option", "yes", "--option", "no", "--timeout", "250ms"})
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 250*time.Millisecond {
		t.Fatalf("timeout = %v", got)
	}
}

func TestClassifyRejectsInvalidFlagsBeforeReadingStdinOrConfig(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"zero timeout", []string{"--file", "-", "--type", "choice", "--question", "Pick", "--option", "yes", "--timeout", "0"}, "--timeout"},
		{"negative timeout", []string{"--file", "-", "--type", "choice", "--question", "Pick", "--option", "yes", "--timeout", "-1s"}, "--timeout"},
		{"empty model", []string{"--file", "-", "--type", "choice", "--question", "Pick", "--option", "yes", "--model", "   "}, "--model"},
		{"empty base url", []string{"models", "--base-url", "   "}, "--base-url"},
		{"pretty table", []string{"--file", "-", "--type", "choice", "--question", "Pick", "--option", "yes", "--format", "table", "--pretty-print"}, "--pretty-print"},
		{"pretty value", []string{"--file", "-", "--type", "choice", "--question", "Pick", "--option", "yes", "--format", "value", "--pretty-print"}, "--pretty-print"},
		{"pretty models table", []string{"models", "--format", "table", "--pretty-print"}, "--pretty-print"},
		{"answer without value format", []string{"--file", "-", "--type", "choice", "--question", "Pick", "--option", "yes", "--answer", "result"}, "--answer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calledConfig := false
			cmd := newClassifyCmd(classifyDeps{
				loadConfig: func() (*config.Config, error) { calledConfig = true; return nil, nil },
				newBackend: func(backends.Connection) (classify.Backend, error) {
					t.Fatal("client should not be created")
					return nil, nil
				},
				stdinData: func(*cobra.Command) bool { return false },
			})
			cmd.SetIn(errorReader{})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if calledConfig {
				t.Fatal("validation touched config")
			}
		})
	}
}

func TestClassifyRejectsAnswerBeforeNetwork(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"answer without value format", []string{"state", "--type", "choice", "--question", "Pick", "--option", "yes", "--answer", "result"}, "--answer may only"},
		{"answer unknown", []string{"state", "--type", "choice", "--question", "Pick", "--option", "yes", "--format", "value", "--answer", "missing"}, "not one of"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calledConfig := false
			cmd := newClassifyCmd(classifyDeps{
				loadConfig: func() (*config.Config, error) { calledConfig = true; return nil, nil },
				newBackend: func(backends.Connection) (classify.Backend, error) {
					t.Fatal("client should not be created")
					return nil, nil
				},
				stdinData: func(*cobra.Command) bool { return false },
			})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if calledConfig {
				t.Fatal("validation touched config")
			}
		})
	}
}

func TestClassifyRejectsDuplicateOptions(t *testing.T) {
	cmd := newClassifyCmd(classifyDeps{
		loadConfig: func() (*config.Config, error) { t.Fatal("config should not be loaded"); return nil, nil },
		stdinData:  func(*cobra.Command) bool { return false },
	})
	cmd.SetArgs([]string{"state", "--type", "choice", "--question", "Pick", "--option", "yes", "--option", "yes=again"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "duplicate --option") {
		t.Fatalf("error = %v, want duplicate option", err)
	}
}

func TestClassifyStateJSONPreservesNumberLexemes(t *testing.T) {
	srv := newClassifyTestServer(t)
	state := writeTemp(t, "state.json", `{"big":123456789012345678901234567890,"exp":1.2300e+10}`)
	questions := writeTemp(t, "questions.json", `{"result":{"type":"choice","instructions":"Pick","criteria":{"yes":null}}}`)
	if _, err := executeClassifyTest(t, []string{"--file", state, "--state-json", "--questions", questions}, "", false, nil, srv); err != nil {
		t.Fatal(err)
	}
	got := string(srv.requests[0].State)
	if got != `{"big":123456789012345678901234567890,"exp":1.2300e+10}` {
		t.Fatalf("state = %s", got)
	}
}

func TestClassifyQuestionParsingStrictness(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"multiple docs", "result:\n  type: choice\n  instructions: Pick\n  criteria: {yes: null}\n---\nother: true\n", "multiple YAML documents"},
		{"unknown question field", `{"result":{"type":"choice","instructions":"Pick","criteria":{"yes":null},"extra":true}}`, "unknown field"},
		{"malformed wrapper", `{"questions":[]}`, "decode questions wrapper"},
		{"wrapper extra", `{"questions":{"result":{"type":"choice","instructions":"Pick","criteria":{"yes":null}}},"extra":true}`, "wrapper may only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			questions := writeTemp(t, "questions.yaml", tt.body)
			cmd := newClassifyCmd(classifyDeps{
				loadConfig: func() (*config.Config, error) { t.Fatal("config should not be loaded"); return nil, nil },
				stdinData:  func(*cobra.Command) bool { return false },
			})
			cmd.SetArgs([]string{"state", "--questions", questions})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestClassifyYAMLNumericKeysArePreservedAsStrings(t *testing.T) {
	srv := newClassifyTestServer(t)
	questions := writeTemp(t, "questions.yaml", "123:\n  type: choice\n  instructions: Pick\n  criteria:\n    1: one\n")
	if _, err := executeClassifyTest(t, []string{"state", "--questions", questions, "--format", "value", "--answer", "123"}, "", false, nil, srv); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.requests[0].Questions["123"]; !ok {
		t.Fatalf("questions = %#v, want numeric id as string", srv.requests[0].Questions)
	}
	if string(srv.requests[0].Questions["123"].Criteria) != `{"1":"one"}` {
		t.Fatalf("criteria = %s", srv.requests[0].Questions["123"].Criteria)
	}
}

func TestClassifyTableEscapesUntrustedFields(t *testing.T) {
	resp := &classify.Response{Answers: map[string]classify.Answer{"bad\n\x1b[31m": {Type: "choice", Choice: strPtr("yes\nno"), Confidence: floatPtr(1)}}}
	out, err := formatClassifyResponse(resp, &classifyOptions{format: "table"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if strings.Contains(body, "bad\n") || strings.Contains(body, "\x1b") || !strings.Contains(body, `"bad "`) || !strings.Contains(body, `"yes no"`) {
		t.Fatalf("table output not escaped: %q", body)
	}
}

func TestClassifyRejectsNegativeConfigTimeout(t *testing.T) {
	cmd := newClassifyCmd(classifyDeps{
		loadConfig: func() (*config.Config, error) {
			return &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key", BaseURL: "http://example.test", Model: "jev", TimeoutSeconds: -1}}}}, nil
		},
		newBackend: func(backends.Connection) (classify.Backend, error) {
			t.Fatal("client should not be created")
			return nil, nil
		},
		stdinData: func(*cobra.Command) bool { return false },
	})
	cmd.SetArgs([]string{"state", "--type", "choice", "--question", "Pick", "--option", "yes"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "timeout_seconds") {
		t.Fatalf("error = %v, want config timeout error", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("stdin should not be read") }

func strPtr(s string) *string     { return &s }
func floatPtr(f float64) *float64 { return &f }

type fakeClassifyClient struct{}

func (fakeClassifyClient) Classify(context.Context, classify.Request) (*classify.Response, error) {
	choice := "yes"
	return &classify.Response{Model: "jev", Answers: map[string]classify.Answer{"result": {Type: "choice", Choice: &choice, Probabilities: map[string]float64{"yes": 1}}}}, nil
}
func (fakeClassifyClient) ListModels(context.Context) (*classify.ModelsResponse, error) {
	return &classify.ModelsResponse{Models: []classify.Model{{Name: "m", Description: "d", ReleaseDate: "2026"}}}, nil
}

func TestClassifyQuestionParserRejectsUnsafeYAML(t *testing.T) {
	for _, input := range []string{
		"q: &q {type: noul, instructions: *q}",
		"q: {type: noul, type: choice, instructions: Test}",
		"q: {type: noul, instructions: Test}\nq: {type: noul, instructions: Other}",
		"q: {type: noul, instructions: " + strings.Repeat("[", 102) + "x" + strings.Repeat("]", 102) + "}",
	} {
		if _, err := parseQuestions([]byte(input)); err == nil {
			t.Fatalf("accepted unsafe question spec %q", input)
		}
	}
}

func TestClassifyQuestionsIDCanBeQuestions(t *testing.T) {
	for _, input := range []string{
		`{"questions":{"type":"noul","instructions":"Urgent?"}}`,
		`{"questions":{"questions":{"type":"noul","instructions":"Urgent?"}}}`,
	} {
		questions, err := parseQuestions([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(questions) != 1 || questions["questions"].Type != "noul" {
			t.Fatalf("unexpected questions: %#v", questions)
		}
	}
}

func TestClassifyOutputFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not supported on Windows")
	}
	for _, models := range []bool{false, true} {
		t.Run(fmt.Sprintf("models=%t", models), func(t *testing.T) {
			srv := newClassifyTestServer(t)
			path := filepath.Join(t.TempDir(), "output.json")
			args := []string{"state", "--type", "noul", "--question", "Urgent?"}
			if models {
				args = []string{"models"}
			}
			args = append(args, "--output", path)
			out, err := executeClassifyTest(t, args, "", false, nil, srv)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 || info.Size() == 0 || out != "" {
				t.Fatalf("output: mode=%o size=%d stdout=%q", info.Mode().Perm(), info.Size(), out)
			}
		})
	}
}

func TestClassifyInheritedStdin(t *testing.T) {
	for _, source := range []string{"positional", "file", "stdin", "questions"} {
		for _, input := range []string{"", " \n\t", "piped state"} {
			t.Run(source+"/"+input, func(t *testing.T) {
				srv := newClassifyTestServer(t)
				args := []string{"--type", "noul", "--question", "Urgent?"}
				switch source {
				case "positional":
					args = append(args, "explicit state")
				case "file":
					args = append(args, "--file", writeTemp(t, "state.txt", "explicit state"))
				case "questions":
					args = []string{"explicit state", "--questions", "-"}
					input = `{"q":{"type":"noul","instructions":"Urgent?"}}`
				}
				_, err := executeClassifyTest(t, args, input, true, nil, srv)
				wantErr := ""
				if source == "stdin" && strings.TrimSpace(input) == "" {
					wantErr = "state is required"
				}
				if (source == "positional" || source == "file") && strings.TrimSpace(input) != "" {
					wantErr = "ambiguous"
				}
				if wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), wantErr) {
						t.Fatalf("error = %v, want %s", err, wantErr)
					}
					if len(srv.requests) != 0 {
						t.Fatal("invalid input reached API")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestClassifyUnquotedMultiwordQuestionExplainsAmbiguity(t *testing.T) {
	srv := newClassifyTestServer(t)
	_, err := executeClassifyTest(t, []string{"--question", "is", "it", "hello", "--type", "noul"}, "hello world\n", true, nil, srv)
	if err == nil || !strings.Contains(err.Error(), `--question "is it hello"`) {
		t.Fatalf("error = %v, want quoting guidance", err)
	}
	if len(srv.requests) != 0 {
		t.Fatal("ambiguous input reached API")
	}
}

func TestClassifyEmptyPipeWithDefaultStdinDetection(t *testing.T) {
	for _, source := range []string{"positional", "file"} {
		t.Run(source, func(t *testing.T) {
			srv := newClassifyTestServer(t)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			writer.Close()
			command := newClassifyCmd(classifyDeps{loadConfig: func() (*config.Config, error) {
				return &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key", BaseURL: srv.server.URL, Model: "jev-test"}}}}, nil
			}})
			command.SetIn(reader)
			command.SetOut(&bytes.Buffer{})
			args := []string{"--type", "noul", "--question", "Urgent?"}
			if source == "positional" {
				args = append(args, "hello")
			} else {
				args = append(args, "--file", writeTemp(t, "state.txt", "hello"))
			}
			command.SetArgs(args)
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			if len(srv.requests) != 1 {
				t.Fatalf("requests=%d", len(srv.requests))
			}
		})
	}
}

func TestClassifyValueIgnoresExtraAnswerIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"jev-test","answers":{"result":{"type":"noul","noul":0.75},"extra":{"type":"noul","noul":0}}}`)
	}))
	defer server.Close()
	cfg := &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key", BaseURL: server.URL, Model: "jev-test"}}}}
	out, err := executeClassifyTest(t, []string{"hello", "--type", "noul", "--question", "Urgent?", "--format", "value"}, "", true, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "0.75\n" {
		t.Fatalf("output=%q", out)
	}
}

func TestClassifyProviderOverridesAndLazyCredentials(t *testing.T) {
	for _, models := range []bool{false, true} {
		for _, flag := range []string{"", "--provider", "-p"} {
			t.Run(fmt.Sprintf("models=%t/flag=%s", models, flag), func(t *testing.T) {
				t.Setenv("TYPESAFE_API_KEY", "env-key")
				srv := newClassifyTestServer(t)
				cfg := &config.Config{Classify: config.ClassifyConfig{DefaultProvider: "custom", Providers: map[string]config.ClassifyProviderConfig{
					"typesafe": {APIKey: "$(exit 1)", BaseURL: "$(exit 1)"},
					"custom":   {Type: "typesafe", APIKey: "test-key", BaseURL: srv.server.URL, Model: "alias-model"},
				}}}
				args := []string{"state", "--type", "choice", "--question", "Pick", "--option", "yes", "--option", "no"}
				if models {
					args = []string{"models"}
				}
				if flag != "" {
					cfg.Classify.DefaultProvider = "typesafe"
					args = append(args, flag, "custom")
				}
				if _, err := executeClassifyTest(t, args, "", false, cfg, srv); err != nil {
					t.Fatal(err)
				}
				if !models && srv.requests[0].Model != "alias-model" {
					t.Fatalf("model: %s", srv.requests[0].Model)
				}
			})
		}
	}
}

func TestClassifyEnvironmentOnlyAndUnsupportedProviders(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "env-key")
	for _, models := range []bool{false, true} {
		for _, provider := range []string{"typesafe", "unsupported", "missing"} {
			cfg := &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"unsupported": {Type: "other", APIKey: "$(exit 1)"}}}}
			called := false
			c := newClassifyCmd(classifyDeps{loadConfig: func() (*config.Config, error) { return cfg, nil }, newBackend: func(o backends.Connection) (classify.Backend, error) {
				called = true
				if o.APIKey != "env-key" || o.BaseURL != config.DefaultTypeSafeBaseURL || o.Timeout != 10*time.Second {
					t.Fatalf("options: %#v", o)
				}
				return fakeClassifyClient{}, nil
			}, stdinData: func(*cobra.Command) bool { return false }})
			args := []string{"state", "--type", "choice", "--question", "Pick", "--option", "yes"}
			if models {
				args = []string{"models"}
			}
			c.SetArgs(append(args, "-p", provider))
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			err := c.Execute()
			if provider == "typesafe" {
				if err != nil || !called {
					t.Fatalf("env-only: %v", err)
				}
			} else if err == nil || called || !strings.Contains(err.Error(), provider) {
				t.Fatalf("provider %s: %v, called %t", provider, err, called)
			}
		}
	}
}

// classifyCompletions runs the shell completion protocol against the classify
// command tree so registration gaps surface the way <TAB> would show them.
func classifyCompletions(t *testing.T, args ...string) ([]string, string) {
	t.Helper()
	root := &cobra.Command{Use: "term-llm"}
	root.AddCommand(newClassifyCmd(classifyDeps{}))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{cobra.ShellCompNoDescRequestCmd}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("completion request failed: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatalf("completion produced no output")
	}
	directive := lines[len(lines)-1]
	if !strings.HasPrefix(directive, ":") {
		t.Fatalf("completion output missing directive: %q", out.String())
	}
	completions := lines[:len(lines)-1]
	if len(completions) == 1 && completions[0] == "" {
		completions = nil
	}
	return completions, directive
}

func assertClassifyCompletions(t *testing.T, got []string, directive string, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("completions = %v, want %v", got, want)
	}
	if wantDirective := fmt.Sprintf(":%d", cobra.ShellCompDirectiveNoFileComp); directive != wantDirective {
		t.Fatalf("directive = %q, want %q", directive, wantDirective)
	}
}

func TestClassifyEnumFlagCompletions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"type", []string{"classify", "state", "--type", ""}, []string{"choice", "noul", "score"}},
		{"type prefix", []string{"classify", "state", "--type", "s"}, []string{"score"}},
		{"format", []string{"classify", "state", "--format", ""}, []string{"json", "table", "value"}},
		{"models format", []string{"classify", "models", "--format", ""}, []string{"json", "table"}},
		{"free text", []string{"classify", "state", "--question", ""}, nil},
		{"option", []string{"classify", "state", "--option", ""}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, directive := classifyCompletions(t, tc.args...)
			assertClassifyCompletions(t, got, directive, tc.want)
		})
	}
}

func TestClassifyProviderModelAndBaseURLCompletions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	path, err := config.GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "classify:\n  providers:\n    custom:\n      type: typesafe\n      model: custom-model\n      base_url: https://classify.example\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	got, directive := classifyCompletions(t, "classify", "state", "--provider", "")
	assertClassifyCompletions(t, got, directive, []string{"custom", "openai", "typesafe"})

	got, directive = classifyCompletions(t, "classify", "state", "--provider", "openai", "--model", "")
	assertClassifyCompletions(t, got, directive, []string{config.DefaultOpenAIDecisionsModel})

	got, directive = classifyCompletions(t, "classify", "state", "--provider", "openai", "--base-url", "")
	assertClassifyCompletions(t, got, directive, []string{config.DefaultOpenAIDecisionsBaseURL})

	got, directive = classifyCompletions(t, "classify", "state", "--provider", "custom", "--model", "")
	assertClassifyCompletions(t, got, directive, []string{"custom-model", config.DefaultTypeSafeModel})

	got, directive = classifyCompletions(t, "classify", "state", "--model", "")
	assertClassifyCompletions(t, got, directive, []string{config.DefaultTypeSafeModel})

	got, directive = classifyCompletions(t, "classify", "state", "--provider", "custom", "--base-url", "")
	assertClassifyCompletions(t, got, directive, []string{config.DefaultTypeSafeBaseURL, "https://classify.example"})
}

func TestClassifyAnswerCompletionUsesRequestedQuestions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	questions := writeTemp(t, "questions.yaml", "urgency:\n  type: score\n  instructions: How urgent?\n  criteria: [low, high]\ntopic:\n  type: choice\n  instructions: Which topic?\n  criteria:\n    billing: Billing\n")

	got, directive := classifyCompletions(t, "classify", "state", "--questions", questions, "--format", "value", "--answer", "")
	assertClassifyCompletions(t, got, directive, []string{"topic", "urgency"})

	got, directive = classifyCompletions(t, "classify", "state", "--type", "noul", "--question", "Happy?", "--name", "verdict", "--answer", "")
	assertClassifyCompletions(t, got, directive, []string{"verdict"})

	got, directive = classifyCompletions(t, "classify", "state", "--questions", filepath.Join(t.TempDir(), "missing.yaml"), "--answer", "")
	assertClassifyCompletions(t, got, directive, nil)

	// --questions - reads ids from stdin, which completion cannot see. It must
	// not fall back to --name, whose default would suggest a wrong id.
	got, directive = classifyCompletions(t, "classify", "state", "--questions", "-", "--answer", "")
	assertClassifyCompletions(t, got, directive, nil)

	// A directory is not readable as questions and must not suggest anything.
	got, directive = classifyCompletions(t, "classify", "state", "--questions", t.TempDir(), "--answer", "")
	assertClassifyCompletions(t, got, directive, nil)
}

func TestClassifyOpenAIProviderSendsImages(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-openai-key")
	t.Setenv("TYPESAFE_API_KEY", "")
	png := filepath.Join(t.TempDir(), "red.png")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		state  string
		images int
	}{
		{"image only", []string{"--image", png}, "", 1},
		{"text and images", []string{"a photo", "--image", png, "--image", "data:image/webp;base64,UklGRg=="}, `"a photo"`, 2},
		{"text only", []string{"a photo"}, `"a photo"`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got classify.Request
			client := &recordingClassifyClient{onClassify: func(req classify.Request) { got = req }}
			c := newClassifyCmd(classifyDeps{
				loadConfig: func() (*config.Config, error) { return &config.Config{}, nil },
				newBackend: func(o backends.Connection) (classify.Backend, error) {
					if o.Type != config.ClassifyProviderOpenAI || o.APIKey != "env-openai-key" || o.BaseURL != config.DefaultOpenAIDecisionsBaseURL || o.Timeout != 10*time.Second {
						t.Fatalf("options: %#v", o)
					}
					return client, nil
				},
				stdinData: func(*cobra.Command) bool { return false },
			})
			c.SetArgs(append(tc.args, "-p", "openai", "--type", "noul", "--question", "Is it red?", "--format", "value"))
			var out bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&bytes.Buffer{})
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			if got.Model != config.DefaultOpenAIDecisionsModel || string(got.State) != tc.state || len(got.Images) != tc.images {
				t.Fatalf("request: model=%q state=%s images=%d", got.Model, got.State, len(got.Images))
			}
			if tc.images > 0 && !strings.HasPrefix(got.Images[0], "data:image/png;base64,") {
				t.Fatalf("image not encoded as PNG data URL: %.40s", got.Images[0])
			}
			if out.String() != "0.9\n" {
				t.Fatalf("output = %q", out.String())
			}
		})
	}
}

func TestClassifyImageRejections(t *testing.T) {
	text := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(text, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, provider, image, want string
	}{
		{"typesafe provider", "typesafe", "data:image/png;base64,iVBO", "requires a classify provider with type openai"},
		{"not an image", "openai", text, "unsupported image type"},
		{"hosted url", "openai", "https://example.com/a.png", "hosted image URLs are not supported"},
		{"bad data url", "openai", "data:text/plain;base64,aGk=", "data URL must be"},
		{"stdin", "openai", "-", "does not read stdin"},
		{"empty data url", "openai", "data:image/png;base64,", "data URL is empty"},
		{"typesafe checked before reading", "typesafe", text, "requires a classify provider with type openai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClassifyCmd(classifyDeps{
				loadConfig: func() (*config.Config, error) { return &config.Config{}, nil },
				newBackend: func(backends.Connection) (classify.Backend, error) {
					t.Fatal("client must not be built")
					return nil, nil
				},
				stdinData: func(*cobra.Command) bool { return false },
			})
			c.SetArgs([]string{"--image", tc.image, "-p", tc.provider, "--type", "noul", "--question", "?"})
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			if err := c.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

type recordingClassifyClient struct {
	onClassify func(classify.Request)
}

func (c *recordingClassifyClient) Classify(_ context.Context, req classify.Request) (*classify.Response, error) {
	c.onClassify(req)
	value := 0.9
	return &classify.Response{Model: req.Model, Answers: map[string]classify.Answer{"result": {Type: "noul", Noul: &value}}}, nil
}

func (c *recordingClassifyClient) ListModels(context.Context) (*classify.ModelsResponse, error) {
	return &classify.ModelsResponse{}, nil
}

func TestClassifyImageDataURLNormalization(t *testing.T) {
	got, err := classifyImageDataURL("DATA:IMAGE/PNG;BASE64,iVBO\nRw0K\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "data:image/png;base64,iVBORw0K" {
		t.Fatalf("normalized = %q", got)
	}
}
