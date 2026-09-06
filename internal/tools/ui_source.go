package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/buildinfo"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/uisource"
)

// UIExtensionControl is bound only by a web runtime. It exposes extension-only
// status/reload/activation, not provider configuration, credentials, or arbitrary HTTP URLs.
type UIExtensionControl func(context.Context, string) (any, error)
type UIBrowserContext struct {
	AssetVersion string   `json:"asset_version"`
	Loaded       []string `json:"loaded"`
	Generation   string   `json:"generation"`
	SafeMode     bool     `json:"safe_mode"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
}
type UIExtensionsTool struct {
	activate bool
	browser  atomic.Pointer[UIBrowserContext]
	mu       sync.RWMutex
	control  UIExtensionControl
}

func (t *UIExtensionsTool) Spec() llm.ToolSpec {
	if t.activate {
		return llm.ToolSpec{Name: UIActivateToolName, Description: "Activate completed web extension edits: rescan the configured enabled list and request automatic browser reload when responses finish and drafts are clear. No user activation step is needed. Affects connected HTTP browsers of this process; safe mode, boot flags and WebRTC restrictions remain authoritative. Does not edit the enabled list. Reports a requested activation, not proof the browser rendered it.", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}
	}
	return llm.ToolSpec{
		Name: UIExtensionsToolName,
		Description: "Inspect or reload the running web process's extension directory, enabled order and config/CLI provenance. " +
			"Reload re-reads extension config and snapshots completed file edits; browser refresh applies them. " +
			"Does not bypass boot flags or edit config.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{"type": "string", "enum": []string{"status", "reload"}},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		},
	}
}

func (t *UIExtensionsTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.ToolOutput{}, err
	}
	if t.activate {
		if a.Operation != "" {
			return llm.ToolOutput{}, fmt.Errorf("activation takes no operation argument")
		}
		a.Operation = "activate"
	}
	if a.Operation != "status" && a.Operation != "reload" && !(t.activate && a.Operation == "activate") {
		return llm.ToolOutput{}, fmt.Errorf("operation must be status or reload")
	}
	t.mu.RLock()
	f := t.control
	t.mu.RUnlock()
	if f == nil {
		return llm.TextOutput("Extension control is available only in term-llm serve web. Use ui_get_source for stock references."), nil
	}
	v, err := f(ctx, a.Operation)
	if err != nil {
		return llm.TextOutput(err.Error()), nil
	}
	return jsonToolOutput(map[string]any{"process": v, "browser": t.browser.Load()})
}
func (r *LocalToolRegistry) SetUIExtensionControl(f UIExtensionControl) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range []string{UIExtensionsToolName, UIActivateToolName} {
		if tool, ok := r.tools[name].(*UIExtensionsTool); ok {
			tool.mu.Lock()
			tool.control = f
			tool.mu.Unlock()
		}
	}
}
func jsonToolOutput(v any) (llm.ToolOutput, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return llm.TextOutput(string(b)), err
}

type UIGetSourceTool struct {
	config   *ToolConfig
	approval *ApprovalManager
	browser  atomic.Pointer[UIBrowserContext]
}

func (t *UIGetSourceTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: UIGetSourceToolName,
		Description: "Inspect stock web UI source on demand. List first, then bounded reads of relevant CSS/TSX. " +
			"Prefers exact embedded readable source, falls back to published release source or current minified assets. " +
			"Use target compiled for exact shipped CSS/JS. Extraction writes a new reference directory after normal workspace approval. " +
			"For installed extensions use ui_extensions status and ordinary file tools.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation":  map[string]any{"type": "string", "enum": []string{"list", "read", "extract"}},
				"target":     map[string]any{"type": "string", "enum": []string{"stock", "compiled"}},
				"path":       map[string]any{"type": "string", "description": "Source-relative path for read; new workspace directory for extract"},
				"start_line": map[string]any{"type": "integer"},
				"end_line":   map[string]any{"type": "integer"},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		},
	}
}

func (t *UIGetSourceTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a struct {
		Operation string `json:"operation"`
		Target    string `json:"target"`
		Path      string `json:"path"`
		Start     int    `json:"start_line"`
		End       int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.ToolOutput{}, err
	}
	var ref *uisource.Reference
	var err error
	if a.Target == "compiled" {
		files := map[string][]byte{}
		for _, p := range serveui.SourceAssetPaths() {
			if b, e := serveui.StaticAsset(p); e == nil {
				files["compiled/"+p] = b
			}
		}
		ref = &uisource.Reference{Version: buildinfo.Version, AssetVersion: serveui.AssetVersion(), Quality: "compiled-only", Provenance: "embedded-assets", Files: files}
	} else if a.Target == "" || a.Target == "stock" {
		ref, err = uisource.Resolve(ctx, buildinfo.Version, true)
	} else {
		return llm.ToolOutput{}, fmt.Errorf("target must be stock or compiled")
	}
	if err != nil {
		return llm.TextOutput(err.Error()), nil
	}
	result := map[string]any{"source": ref, "requested_version": buildinfo.Version, "requested_asset_version": serveui.AssetVersion()}
	if browser := t.browser.Load(); browser != nil {
		result["browser"] = browser
		if browser.AssetVersion != "" && browser.AssetVersion != ref.AssetVersion {
			copy := *ref
			copy.Quality = "approximate"
			copy.Warning = "Browser asset version differs from this source host. Verify assumptions against the actual UI."
			result["source"] = &copy
			result["warning"] = copy.Warning
		}
	}

	switch a.Operation {
	case "list":
		result["files"] = ref.Inventory()
	case "read":
		b, ok := ref.Files[a.Path]
		if !ok {
			return llm.TextOutput("Source path not found. Use operation=list first."), nil
		}
		lines := strings.Split(string(b), "\n")
		start := a.Start
		if start < 1 {
			start = 1
		}
		end := a.End
		if end < start {
			end = start + 179
		}
		if end > start+399 {
			end = start + 399
		}
		if end > len(lines) {
			end = len(lines)
		}
		if start > len(lines) {
			return llm.TextOutput("Start line exceeds source length"), nil
		}
		var out strings.Builder
		for i := start - 1; i < end; i++ {
			line := lines[i]
			if len(line) > 24000 {
				line = line[:24000] + " … [line truncated; use extract for minified files]"
			}
			if out.Len()+len(line) > 48000 {
				break
			}
			fmt.Fprintf(&out, "%d: %s\n", i+1, line)
		}
		result["path"] = a.Path
		result["content"] = out.String()
		result["total_lines"] = len(lines)
	case "extract":
		if a.Path == "" {
			return llm.ToolOutput{}, fmt.Errorf("extract requires a new workspace path")
		}
		dest, err := resolveToolPathWithConfig(a.Path, true, t.config)
		if err != nil {
			return llm.ToolOutput{}, err
		}
		if t.approval == nil {
			return llm.ToolOutput{}, fmt.Errorf("workspace approval is unavailable")
		}
		outcome, err := t.approval.CheckPathApprovalWithContext(ctx, UIGetSourceToolName, dest, a.Path, true)
		if err != nil {
			return pathApprovalErrorOutput("", err), nil
		}
		if outcome == Cancel {
			return llm.TextOutput("Source extraction denied"), nil
		}
		if _, err = os.Lstat(dest); !os.IsNotExist(err) {
			return llm.TextOutput("Extraction requires a new directory; existing references are never overwritten"), nil
		}
		if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return llm.ToolOutput{}, err
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dest), ".ui-source-*")
		if err != nil {
			return llm.ToolOutput{}, err
		}
		defer os.RemoveAll(tmp)
		for p, b := range ref.Files {
			target := filepath.Join(tmp, filepath.FromSlash(p))
			if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return llm.ToolOutput{}, err
			}
			if err = os.WriteFile(target, b, 0644); err != nil {
				return llm.ToolOutput{}, err
			}
		}
		if err = os.Rename(tmp, dest); err != nil {
			return llm.ToolOutput{}, err
		}
		result["directory"] = dest
		result["file_count"] = len(ref.Files)
	default:
		return llm.ToolOutput{}, fmt.Errorf("operation must be list, read or extract")
	}
	return jsonToolOutput(result)
}

func (t *UIGetSourceTool) Preview(args json.RawMessage) string { return "Inspect web UI source" }
func (t *UIExtensionsTool) Preview(args json.RawMessage) string {
	if t.activate {
		return "Activate web extensions automatically"
	}
	return "Inspect or reload web extensions"
}

// SetUIBrowserContext records non-authoritative, bounded presentation metadata.
// It never grants filesystem or configuration access.
func (r *LocalToolRegistry) SetUIBrowserContext(browser *UIBrowserContext) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.tools[UIGetSourceToolName].(*UIGetSourceTool); ok {
		t.browser.Store(browser)
	}
	for _, name := range []string{UIExtensionsToolName, UIActivateToolName} {
		if t, ok := r.tools[name].(*UIExtensionsTool); ok {
			t.browser.Store(browser)
		}
	}
}
