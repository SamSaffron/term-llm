package evalfixture

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/filelock"
)

// RunServer runs one deterministic domain stdio MCP server. limit may expose a
// prefix from 0 through 20 for catalogue-size profiles. This is the original
// ten-server mode retained for boundary and federation tests.
func RunServer(ctx context.Context, serverName, root string, limit int) error {
	domain, ok := FindDomain(serverName)
	if !ok {
		return fmt.Errorf("unknown evaluation server %q", serverName)
	}
	if limit < 0 || limit > len(domain.Tools) {
		return fmt.Errorf("invalid tool limit %d for %s", limit, serverName)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0755); err != nil {
		return err
	}
	serverOptions := &sdkmcp.ServerOptions{}
	if serverName == "source_control" {
		serverOptions.PageSize = 5
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "term-llm-eval-" + serverName, Version: "1"}, serverOptions)
	state := &fixtureState{root: root, server: serverName}
	for _, definition := range domain.Tools[:limit] {
		registerEvaluationTool(server, state, definition, definition.Name, serverName == "source_control" && definition.Name == "create_release")
	}
	return server.Run(ctx, &sdkmcp.StdioTransport{})
}

// RunAggregateServer runs one stdio MCP server exposing all 200 domain tools.
// Domain qualification makes names unique on the wire; handlers, state, call
// logs, schemas, and oracle values are shared with the ten-server federation.
func RunAggregateServer(ctx context.Context, root string) error {
	if err := os.MkdirAll(filepath.Join(root, "state"), 0755); err != nil {
		return err
	}
	server := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "term-llm-eval-federation", Version: "1"},
		&sdkmcp.ServerOptions{PageSize: 25},
	)
	states := make(map[string]*fixtureState, len(Domains()))
	for _, aggregate := range AggregateTools() {
		state := states[aggregate.Domain]
		if state == nil {
			state = &fixtureState{root: root, server: aggregate.Domain}
			states[aggregate.Domain] = state
		}
		description := "Domain: " + humanize(aggregate.Domain) + ". " + aggregate.Definition.Description
		definition := aggregate.Definition
		definition.Description = description
		refresh := aggregate.Domain == "source_control" && definition.Name == "create_release"
		registerEvaluationTool(server, state, definition, aggregate.Name, refresh)
	}
	return server.Run(ctx, &sdkmcp.StdioTransport{})
}

func registerEvaluationTool(server *sdkmcp.Server, state *fixtureState, definition ToolDefinition, exposedName string, refresh bool) {
	description := definition.Description
	annotations := &sdkmcp.ToolAnnotations{ReadOnlyHint: !definition.Mutating, IdempotentHint: !definition.Mutating}
	if definition.Mutating {
		destructive := isDestructive(definition.Name)
		annotations.DestructiveHint = &destructive
	}
	server.AddTool(&sdkmcp.Tool{
		Name:         exposedName,
		Title:        title(definition.Name),
		Description:  description,
		InputSchema:  realisticInputSchema(definition),
		OutputSchema: realisticOutputSchema(),
		Annotations:  annotations,
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		result, err := state.call(ctx, definition, req.Params.Arguments)
		if refresh {
			state.refreshOnce.Do(func() {
				refreshed := definition
				refreshed.Description += " Catalogue refresh generation two."
				registerEvaluationTool(server, state, refreshed, exposedName, false)
			})
		}
		return result, err
	})
}

// FindDomain returns one domain by server name.
func FindDomain(name string) (Domain, bool) {
	for _, domain := range Domains() {
		if domain.Name == name {
			return domain, true
		}
	}
	return Domain{}, false
}

// ProfileLimits deterministically distributes a total catalogue prefix across
// the ten twenty-tool servers.
func ProfileLimits(total int) (map[string]int, error) {
	if total < 0 || total > 200 {
		return nil, fmt.Errorf("catalogue size must be between 0 and 200")
	}
	limits := make(map[string]int, 10)
	remaining := total
	for _, domain := range Domains() {
		limit := remaining
		if limit > len(domain.Tools) {
			limit = len(domain.Tools)
		}
		limits[domain.Name] = limit
		remaining -= limit
	}
	return limits, nil
}

type fixtureState struct {
	mu          sync.Mutex
	refreshOnce sync.Once
	root        string
	server      string
}

func (s *fixtureState) call(_ context.Context, tool ToolDefinition, raw json.RawMessage) (*sdkmcp.CallToolResult, error) {
	var args map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("decode arguments: %w", err)
		}
	}
	if args == nil {
		args = make(map[string]any)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	resultClass := "read"
	if tool.Mutating {
		resultClass = "mutation"
		statePath := filepath.Join(s.root, "state", s.server+".json")
		state := map[string]any{"server": s.server, "last_tool": tool.Name, "arguments": args, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
		data, _ := json.MarshalIndent(state, "", "  ")
		if err := os.WriteFile(statePath, data, 0644); err != nil {
			return nil, err
		}
	}
	oracle := OracleValue(s.server, tool.Name)
	entry := map[string]any{
		"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
		"server":       s.server,
		"tool":         tool.Name,
		"oracle":       oracle,
		"arguments":    args,
		"result_class": resultClass,
	}
	if s.server == "source_control" && tool.Name == "create_release" {
		entry["catalogue_refresh_emitted"] = true
	}
	if err := appendJSONLine(filepath.Join(s.root, "calls.jsonl"), entry); err != nil {
		return nil, err
	}
	result := map[string]any{
		"ok":           true,
		"server":       s.server,
		"operation":    tool.Name,
		"oracle":       oracle,
		"result_class": resultClass,
		"record": map[string]any{
			"id":      firstString(args, "id", "project_id", "repository", "query", "customer_id", "order_id"),
			"status":  "fixture-ready",
			"version": 1,
		},
		"items": []any{},
	}
	addScenarioFields(result, s.server, tool.Name)
	result["fixture_backend"] = fixtureBackendSummary(s.root, s.server)
	data, _ := json.Marshal(result)
	return &sdkmcp.CallToolResult{
		Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(data)}},
		StructuredContent: result,
	}, nil
}

func fixtureBackendSummary(root, server string) map[string]any {
	summary := map[string]any{"root": root, "kind": "json"}
	switch server {
	case "source_control":
		summary["kind"] = "git"
		head, err := os.ReadFile(filepath.Join(root, "repos", "acme-app", ".git", "HEAD"))
		if err == nil {
			summary["head"] = strings.TrimSpace(string(head))
		}
	case "docs_drive":
		summary["kind"] = "files"
		if info, err := os.Stat(filepath.Join(root, "documents", "DOC-5.md")); err == nil {
			summary["document_bytes"] = info.Size()
		}
	case "sql_analytics", "commerce_crm", "observability":
		summary["kind"] = "sqlite"
		db, err := sql.Open("sqlite", filepath.Join(root, "databases", "evaluation.sqlite"))
		if err == nil {
			defer db.Close()
			var count int
			if db.QueryRow("SELECT count(*) FROM orders").Scan(&count) == nil {
				summary["fixture_orders"] = count
			}
		}
	}
	return summary
}

func addScenarioFields(result map[string]any, server, tool string) {
	if server != "source_control" {
		return
	}
	switch tool {
	case "get_pull_request":
		result["pull_request"] = map[string]any{"number": 42, "state": "open", "mergeable": true, "draft": false, "head": "fixture-change", "base": "main"}
	case "get_reviews":
		result["reviews"] = []any{
			map[string]any{"reviewer": "alice", "decision": "approved", "required": true},
			map[string]any{"reviewer": "bob", "decision": "approved", "required": true},
		}
		result["required_reviews_satisfied"] = true
	case "get_check_runs":
		result["check_runs"] = []any{
			map[string]any{"name": "unit-tests", "status": "completed", "conclusion": "success", "required": true},
			map[string]any{"name": "integration-tests", "status": "completed", "conclusion": "success", "required": true},
		}
		result["required_checks_passed"] = true
	case "merge_pull_request":
		result["merged"] = true
		result["merge_commit"] = "fixture-merge-42"
	}
}

func appendJSONLine(path string, value any) error {
	unlock, err := filelock.Lock(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock evaluation log: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		_ = unlock()
		return err
	}
	data, marshalErr := json.Marshal(value)
	var writeErr error
	if marshalErr == nil {
		_, writeErr = file.Write(append(data, '\n'))
	}
	closeErr := file.Close()
	unlockErr := unlock()
	for _, candidate := range []error{marshalErr, writeErr, closeErr, unlockErr} {
		if candidate != nil {
			return candidate
		}
	}
	return nil
}

func realisticInputSchema(tool ToolDefinition) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "description": "Stable fixture identifier for the target resource."},
			"query":  map[string]any{"type": "string", "description": "Natural-language or structured filter expression."},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20},
			"status": map[string]any{"type": "string", "enum": []string{"open", "closed", "active", "resolved", "all"}},
			"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 10},
			"options": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"include_archived": map[string]any{"type": "boolean", "default": false},
					"sort":             map[string]any{"type": "string", "enum": []string{"created", "updated", "relevance"}},
				},
				"additionalProperties": false,
			},
			"payload": map[string]any{"type": "object", "description": "Fields used by create or update operations.", "additionalProperties": true},
		},
		"additionalProperties": false,
	}
}

func realisticOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ok":           map[string]any{"type": "boolean"},
			"server":       map[string]any{"type": "string"},
			"operation":    map[string]any{"type": "string"},
			"oracle":       map[string]any{"type": "string", "description": "Deterministic unique value for this simulated tool; report it exactly when requested."},
			"result_class": map[string]any{"type": "string", "enum": []string{"read", "mutation"}},
			"record": map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"}, "version": map[string]any{"type": "integer"},
			}},
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
		"required": []string{"ok", "server", "operation", "oracle", "result_class"},
	}
}

// OracleValue returns the stable value uniquely identifying one simulated tool.
// The readable prefix supports transcript diagnosis; the digest guards against
// accidental collisions if names are changed or extended.
func OracleValue(domain, tool string) string {
	identity := domain + "/" + tool
	sum := sha256.Sum256([]byte(identity))
	return "oracle:v1:" + domain + ":" + tool + ":" + hex.EncodeToString(sum[:6])
}

func isDestructive(name string) bool {
	for _, prefix := range []string{"delete_", "drop_", "cancel_", "remove_", "rollback_", "issue_refund"} {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func title(name string) string {
	words := []byte(name)
	for i := range words {
		if words[i] == '_' {
			words[i] = ' '
		}
	}
	if len(words) > 0 && words[0] >= 'a' && words[0] <= 'z' {
		words[0] -= 'a' - 'A'
	}
	return string(words)
}

func firstString(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key]; ok {
			if text := fmt.Sprint(value); text != "" {
				return text
			}
		}
	}
	return "fixture-id"
}
