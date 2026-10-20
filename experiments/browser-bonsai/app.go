package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/tools"
)

// The browser demo keeps every piece of state in this process. The page's
// lifetime is the session lifetime: no database, no durable file, no network.
const (
	demoProviderName = "bonsai"
	demoModelName    = "bonsai-2-2b"
	demoModelLabel   = "Bonsai 2 2B"
	demoWorkspace    = "/workspace"
)

// Inference is the JavaScript-owned model bridge. The Go side never invents
// model output; when the browser adapter is missing or fails, the turn fails.
type Inference interface {
	Complete(ctx context.Context, requestJSON string, onDelta func(chunkJSON string)) (string, error)
	CancelInference()
}

// App wires the real llm.Engine and the real local file tools to an ephemeral
// in-memory session store and HTTP surface consumed by the genuine web UI.
type App struct {
	mu         sync.Mutex
	sessions   map[string]*demoSession
	order      []string
	nextNumber int
	nextMsgID  int64
	instanceID string

	engine    *engine.Engine
	registry  *tools.LocalToolRegistry
	inference Inference
	workspace string

	approval  *tools.ApprovalManager
	shell     *shellTool
	shellExec ShellExecutor
	responses map[string]*responseRecord
	runs      map[string]*activeRun
}

type demoSession struct {
	ID       string
	Number   int
	Name     string
	Title    string
	Created  time.Time
	Updated  time.Time
	Archived bool
	Pinned   bool

	Messages       []*demoMessage
	TranscriptRev  int64
	LastResponse   string
	ActiveResponse string
}

type demoMessage struct {
	ID              int64
	Role            string
	Parts           []demoPart
	Created         time.Time
	ResponseID      string
	ClientMessageID string
}

type demoPart struct {
	Type          string `json:"type"`
	Text          string `json:"text,omitempty"`
	ToolCallID    string `json:"tool_call_id,omitempty"`
	ToolName      string `json:"tool_name,omitempty"`
	ToolArguments string `json:"tool_arguments,omitempty"`
	Output        string `json:"output,omitempty"`
	ToolError     bool   `json:"tool_error,omitempty"`
	StartedAt     int64  `json:"started_at,omitempty"`
	EndedAt       int64  `json:"ended_at,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
}

type responseRecord struct {
	mu        sync.Mutex
	ID        string
	SessionID string
	Epoch     int
	Status    string
	Events    []recordedEvent
	Created   time.Time
}

type recordedEvent struct {
	Name    string
	Payload map[string]any
}

type activeRun struct {
	ResponseID string
	SessionID  string
	Cancel     context.CancelFunc
}

var demoSeed = map[string]string{
	"README.md": "# Bonsai workspace\n\n" +
		"Everything in this directory lives in browser memory and disappears when the page reloads.\n" +
		"The agent is the real built-in **developer** agent with YOLO approvals: it never asks for\n" +
		"permission before it reads, writes, edits, or runs a command.\n\n" +
		"## A real Linux shell and CPython, in the tab\n\n" +
		"`shell` runs commands in a real Linux guest (v86) inside this browser tab over this same\n" +
		"workspace. BusyBox provides `sh`, pipes, `awk`, `sed`, `grep`, and coreutils. CPython 3.12\n" +
		"is installed as `python` and `python3`; the tested standard-library set includes `json`, `csv`,\n" +
		"`statistics`, `unittest`, and `sqlite3`, and `base64`/`gzip` work through zlib. Optional\n" +
		"extension modules needing unbundled libraries (`ssl`, `ctypes`, `bz2`, `lzma`, `readline`) are\n" +
		"unavailable. It is a real Linux system, but it is **not the host machine** and has no network:\n" +
		"there is no pip or package installation, and `node` and Go are not installed.\n\n" +
		"## The calculator is red on purpose\n\n" +
		"`playground/calculate.sh` has a small bug and `playground/tests.sh` proves it. Try:\n\n" +
		"- \"Run sh playground/tests.sh, fix calculate.sh, then run it again.\"\n" +
		"- \"Run sh playground/bonsai.sh and explain the awk output.\"\n" +
		"- \"Write a Python script that reads playground/scores.csv with csv, computes mean and\n" +
		"  median with statistics, stores rows in sqlite3, and writes summary.json.\"\n" +
		"- \"Use the shell in playground to add a `divide` command and test it.\"\n\n" +
		"## File tools\n\n" +
		"`read_file`, `write_file`, `edit_file`, and `glob` are the other available tools. Files the\n" +
		"shell writes are synced back into the workspace, so the file tools see them immediately.\n",
	"playground/bonsai.sh": `#!/bin/sh
# A tiny shell + awk playground. Run it with the shell tool.
printf '%s\n' "  browser bonsai" "  --------------"
for n in 1 2 3 4 5; do
  echo "$n $((n * n))"
done | awk 'BEGIN { print "n  n^2" } { printf "%-2s %s\n", $1, $2 }'

printf '%s\n' "" "words:" "  shell" "  awk" "  busybox" | awk '{ print length($0) "  " $0 }'
`,
	"playground/scores.csv": "name,score\nalpha,10\nbeta,20\ngamma,30\n",
	"playground/calculate.sh": `#!/bin/sh
# Playground calculator: intentionally broken. tests.sh shows the failures.

add() {
  echo $(( $1 - $2 ))
}

multiply() {
  echo $(( $1 * $2 ))
}

average() {
  # Bug: divides by one extra item.
  echo $(( ($1 + $2 + $3) / 4 ))
}

case "$1" in
  add) add "$2" "$3" ;;
  multiply) multiply "$2" "$3" ;;
  average) average "$2" "$3" "$4" ;;
  *)
    echo "usage: $0 add|multiply|average numbers..." >&2
    exit 2
    ;;
esac
`,
	"playground/tests.sh": `#!/bin/sh
# Run with: sh playground/tests.sh
set -u

fail=0
check() {
  expected=$1
  shift
  got=$("$@")
  if [ "$got" != "$expected" ]; then
    echo "FAIL: $* => $got (expected $expected)" >&2
    fail=1
  else
    echo "ok: $* => $got"
  fi
}

check 5 sh ./playground/calculate.sh add 2 3
check 20 sh ./playground/calculate.sh multiply 4 5
check 4 sh ./playground/calculate.sh average 2 4 6

if [ "$fail" -eq 0 ]; then
  echo "all calculator tests passed"
fi
exit "$fail"
`,
}

// NewApp creates the browser demo rooted at the virtual /workspace.
func NewApp(inference Inference) (*App, error) {
	return NewAppWithWorkspace(inference, demoWorkspace, true)
}

// NewAppWithWorkspace creates the app for an explicit workspace root.
//
// seedWorkspace resets the in-memory browser workspace to demoSeed. Native
// callers must pass false and create/prepare the directory themselves; the
// native workspace seam refuses recursive deletion by design.
func NewAppWithWorkspace(inference Inference, workspace string, seedWorkspace bool) (*App, error) {
	if inference == nil {
		return nil, fmt.Errorf("inference bridge is required")
	}
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("workspace is required")
	}
	if seedWorkspace {
		if err := tools.SetWorkspaceFS(workspace, demoSeed); err != nil {
			return nil, fmt.Errorf("seed workspace: %w", err)
		}
	}

	toolConfig := &tools.ToolConfig{
		Enabled:   []string{tools.ReadFileToolName, tools.WriteFileToolName, tools.EditFileToolName, tools.GlobToolName},
		ReadDirs:  []string{workspace},
		WriteDirs: []string{workspace},
		BaseDir:   workspace,
		Workspace: "auto",
	}
	permissions, err := toolConfig.BuildPermissions()
	if err != nil {
		return nil, fmt.Errorf("tool permissions: %w", err)
	}
	// The browser demo is explicitly YOLO: the user asked for a sandbox where
	// the agent can run things without approval prompts. This is the real
	// approval manager the file tools consult, not a display-only flag.
	approval := tools.NewApprovalManager(permissions)
	approval.SetApprovalMode(tools.ModeYolo)

	registry, err := tools.NewLocalToolRegistry(toolConfig, nil, approval)
	if err != nil {
		return nil, fmt.Errorf("tool registry: %w", err)
	}

	provider := &jsProvider{inference: inference, model: demoModelName}
	eng := engine.NewEngine(provider, nil)
	registry.RegisterWithEngine(eng)
	// manage_workspace is auto-registered by the real tool registry whenever a
	// path tool is enabled. The demo has exactly one in-memory workspace, so the
	// model must never see or call it.
	eng.UnregisterTool(tools.ManageWorkspaceToolName)

	app := &App{
		sessions:   map[string]*demoSession{},
		nextNumber: 1,
		nextMsgID:  1,
		instanceID: randomHex(8),
		workspace:  workspace,
		engine:     eng,
		registry:   registry,
		inference:  inference,
		approval:   approval,
		shellExec:  newShellExecutor(),
		responses:  map[string]*responseRecord{},
		runs:       map[string]*activeRun{},
	}
	app.shell = &shellTool{app: app}
	eng.RegisterTool(app.shell)
	return app, nil
}

// SetShellExecutor replaces the page Linux VM bridge. Native tests inject a
// deterministic mock; browser builds keep the real termLLMRunShell caller.
func (a *App) SetShellExecutor(executor ShellExecutor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.shellExec = executor
}

func (a *App) shellExecutor() ShellExecutor {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shellExec
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (a *App) createSession(name string) *demoSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	s := &demoSession{
		ID:      "sess_" + randomHex(8),
		Number:  a.nextNumber,
		Name:    name,
		Title:   name,
		Created: now,
		Updated: now,
	}
	a.nextNumber++
	a.sessions[s.ID] = s
	a.order = append([]string{s.ID}, a.order...)
	return s
}

func (a *App) session(id string) *demoSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (a *App) listSessions() []*demoSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*demoSession, 0, len(a.order))
	for _, id := range a.order {
		if s := a.sessions[id]; s != nil && !s.Archived {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// startResponse admits one turn. The user message is committed before the
// engine runs so a reload or cancel can never lose what the human typed.
func (a *App) startResponse(session *demoSession, userParts []demoPart, clientMessageID string, model string) (responseID string, ctx context.Context, cancel context.CancelFunc, record *responseRecord, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(userParts) == 0 {
		return "", nil, nil, nil, fmt.Errorf("input is required")
	}
	user := &demoMessage{
		ID:              a.nextMsgID,
		Role:            "user",
		Parts:           userParts,
		Created:         time.Now(),
		ClientMessageID: clientMessageID,
	}
	a.nextMsgID++
	session.Messages = append(session.Messages, user)
	if session.Name == "" || session.Name == "New chat" {
		if text := userText(user.Parts); text != "" {
			session.Name = truncateTitle(text)
			session.Title = session.Name
		}
	}
	session.Updated = time.Now()
	session.TranscriptRev++
	responseID = "resp_" + randomHex(10)
	ctx, cancel = context.WithCancel(context.Background())
	a.runs[session.ID] = &activeRun{ResponseID: responseID, SessionID: session.ID, Cancel: cancel}
	record = &responseRecord{ID: responseID, SessionID: session.ID, Epoch: 1, Status: "in_progress", Created: time.Now()}
	a.responses[responseID] = record
	session.ActiveResponse = responseID
	return responseID, ctx, cancel, record, nil
}

func truncateTitle(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if len(text) > 60 {
		text = strings.TrimSpace(text[:60]) + "…"
	}
	if text == "" {
		return "New chat"
	}
	return text
}

func userText(parts []demoPart) string {
	var texts []string
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// runTurn streams one engine invocation and commits its durable transcript
// rows. Tool execution happens inside the engine, against the virtual FS.
func (a *App) runTurn(ctx context.Context, session *demoSession, events *responseStream, model string) error {
	a.mu.Lock()
	history := make([]*demoMessage, 0, len(session.Messages))
	for _, msg := range session.Messages {
		history = append(history, msg)
	}
	a.mu.Unlock()

	messages := buildCoreMessages(history, a.workspace)
	req := core.Request{
		Model:      model,
		SessionID:  session.ID,
		WorkingDir: a.workspace,
		Messages:   messages,
		Tools:      a.toolSpecs(),
	}
	stream, err := a.engine.Stream(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	var assistantParts []demoPart
	var toolRows []*demoMessage
	var usage *core.Usage
	textSegment := -1
	ensureText := func() int {
		if textSegment < 0 {
			assistantParts = append(assistantParts, demoPart{Type: "text"})
			textSegment = len(assistantParts) - 1
		}
		return textSegment
	}
	closeText := func() { textSegment = -1 }

	for {
		event, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		switch event.Type {
		case core.EventTextDelta:
			if event.Text == "" {
				continue
			}
			idx := ensureText()
			assistantParts[idx].Text += event.Text
			if err := events.emit("response.output_text.delta", map[string]any{
				"delta":                     event.Text,
				"assistant_segment_ordinal": 0,
			}); err != nil {
				return err
			}
		case core.EventToolCall:
			if event.Tool == nil {
				continue
			}
			closeText()
			assistantParts = append(assistantParts, demoPart{
				Type:          "tool_call",
				ToolCallID:    event.Tool.ID,
				ToolName:      event.Tool.Name,
				ToolArguments: string(event.Tool.Arguments),
			})
			if err := events.emit("response.output_item.added", map[string]any{
				"output_index": len(assistantParts) - 1,
				"item": map[string]any{
					"type": "function_call", "id": event.Tool.ID, "call_id": event.Tool.ID,
					"name": event.Tool.Name, "arguments": "",
				},
			}); err != nil {
				return err
			}
			if err := events.emit("response.function_call_arguments.delta", map[string]any{
				"call_id": event.Tool.ID, "item_id": event.Tool.ID, "delta": string(event.Tool.Arguments),
			}); err != nil {
				return err
			}
			if err := events.emit("response.output_item.done", map[string]any{
				"item": map[string]any{
					"type": "function_call", "id": event.Tool.ID, "call_id": event.Tool.ID,
					"name": event.Tool.Name, "arguments": string(event.Tool.Arguments),
				},
			}); err != nil {
				return err
			}
		case core.EventToolExecStart:
			closeText()
			if err := events.emit("response.tool_exec.start", map[string]any{
				"call_id": event.ToolCallID, "tool_name": event.ToolName,
				"tool_arguments": string(event.ToolArgs),
				"started_at":     time.Now().UnixMilli(),
			}); err != nil {
				return err
			}
		case core.EventToolExecEnd:
			closeText()
			if err := events.emit("response.tool_exec.end", map[string]any{
				"call_id": event.ToolCallID, "tool_name": event.ToolName,
				"output": event.ToolOutput, "success": event.ToolSuccess,
				"ended_at": time.Now().UnixMilli(),
			}); err != nil {
				return err
			}
			toolRows = append(toolRows, &demoMessage{
				Role:       "tool",
				Created:    time.Now(),
				ResponseID: events.record.ID,
				Parts: []demoPart{{
					Type: "tool_result", ToolCallID: event.ToolCallID, ToolName: event.ToolName,
					Output: event.ToolOutput, ToolError: !event.ToolSuccess,
				}},
			})
		case core.EventUsage:
			if event.Use != nil {
				usage = event.Use
			}
		case core.EventError:
			if event.Err != nil {
				return event.Err
			}
		case core.EventPhase:
			if event.Text != "" {
				_ = events.emit("response.phase", map[string]any{"text": event.Text})
			}
		case core.EventHeartbeat:
			_ = events.emit("response.heartbeat", map[string]any{})
		}
	}

	a.mu.Lock()
	now := time.Now()
	if len(assistantParts) > 0 {
		a.nextMsgID++
		session.Messages = append(session.Messages, &demoMessage{
			ID: a.nextMsgID, Role: "assistant", Parts: assistantParts,
			Created: now, ResponseID: events.record.ID,
		})
	}
	for _, row := range toolRows {
		a.nextMsgID++
		row.ID = a.nextMsgID
		session.Messages = append(session.Messages, row)
	}
	session.TranscriptRev++
	session.Updated = now
	session.LastResponse = events.record.ID
	session.ActiveResponse = ""
	if run := a.runs[session.ID]; run != nil && run.ResponseID == events.record.ID {
		delete(a.runs, session.ID)
	}
	a.mu.Unlock()

	usagePayload := usageJSON(usage)
	return events.complete(usagePayload)
}

// toolSpecs is the provider-visible tool surface: the enabled file tools with
// the auto-registered manage_workspace control removed, plus the browser Linux
// shell.
func (a *App) toolSpecs() []core.ToolSpec {
	specs := a.registry.GetSpecs()
	filtered := make([]core.ToolSpec, 0, len(specs)+1)
	for _, spec := range specs {
		if spec.Name == tools.ManageWorkspaceToolName {
			continue
		}
		filtered = append(filtered, spec)
	}
	if a.shell != nil {
		filtered = append(filtered, a.shell.Spec())
	}
	return filtered
}

func usageJSON(usage *core.Usage) map[string]any {
	if usage == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.InputTokens + usage.OutputTokens,
	}
}

func (a *App) cancelResponse(id string) bool {
	a.mu.Lock()
	var cancel context.CancelFunc
	for _, run := range a.runs {
		if run.ResponseID == id {
			cancel = run.Cancel
			break
		}
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

// cancelAll aborts every in-flight turn; the page is going away.
func (a *App) cancelAll() {
	a.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(a.runs))
	for _, run := range a.runs {
		cancels = append(cancels, run.Cancel)
	}
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// buildCoreMessages converts the demo transcript into provider messages and
// prepends the browser-workspace system prompt.
func buildCoreMessages(history []*demoMessage, workspace string) []core.Message {
	messages := make([]core.Message, 0, len(history)+1)
	messages = append(messages, core.SystemText(systemPrompt(workspace)))
	for _, msg := range history {
		switch msg.Role {
		case "user":
			var texts []string
			for _, part := range msg.Parts {
				if part.Type == "text" && part.Text != "" {
					texts = append(texts, part.Text)
				}
			}
			messages = append(messages, core.Message{
				Role:            core.RoleUser,
				Parts:           []core.Part{{Type: core.PartText, Text: strings.Join(texts, "\n")}},
				ClientMessageID: msg.ClientMessageID,
			})
		case "assistant":
			parts := make([]core.Part, 0, len(msg.Parts))
			for _, part := range msg.Parts {
				switch part.Type {
				case "text":
					if part.Text != "" {
						parts = append(parts, core.Part{Type: core.PartText, Text: part.Text})
					}
				case "tool_call":
					parts = append(parts, core.Part{Type: core.PartToolCall, ToolCall: &core.ToolCall{
						ID: part.ToolCallID, Name: part.ToolName, Arguments: json.RawMessage(part.ToolArguments),
					}})
				}
			}
			if len(parts) > 0 {
				messages = append(messages, core.Message{Role: core.RoleAssistant, Parts: parts})
			}
		case "tool":
			for _, part := range msg.Parts {
				if part.Type != "tool_result" {
					continue
				}
				messages = append(messages, core.Message{
					Role: core.RoleTool,
					Parts: []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{
						ID: part.ToolCallID, Name: part.ToolName, Content: part.Output, IsError: part.ToolError,
					}}},
				})
			}
		}
	}
	return messages
}
