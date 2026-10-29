package debuglog

import (
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const redacted = "[REDACTED]"

func (opts ExportOptions) shouldRedact() bool {
	return opts.Redact || !opts.IncludeSensitive
}

// Only known diagnostic metadata is exported. This is a schema projection, not
// a recursive key blacklist: arbitrary payloads can contain secrets in values,
// object keys, numbers, URLs, or future fields. Never descend into content,
// arguments, output, tool_info, errors, schemas, environment, or unknown fields.
// Keep these schemas export-only; the logger's hot write path is unaffected.
type exportSchema map[string]string

var exportSchemas = sync.OnceValue(func() map[string]exportSchema {
	schemas := map[string]exportSchema{}
	add := func(name, kind, fields string) {
		if schemas[name] == nil {
			schemas[name] = exportSchema{}
		}
		for _, field := range strings.Fields(fields) {
			schemas[name][field] = kind
		}
	}
	add("entry", "label", "type event_type provider model")
	add("entry", "timestamp", "timestamp")
	add("entry", "number", "turn")
	add("entry", "omit", "session_id command args cwd build name")
	add("entry", "request", "request")
	add("entry", "data", "data")
	add("request", "[]message", "messages")
	add("request", "[]tool", "tools")
	add("request", "choice", "tool_choice")
	add("request", "omit", "session_id")
	add("request", "label", "reasoning_effort")
	add("request", "number", "max_output_tokens temperature top_p reasoning_replay_parts reasoning_encrypted_parts max_turns")
	add("request", "bool", "search force_external_search disable_external_web_fetch parallel_tool_calls")
	add("message", "label", "role")
	add("message", "omit", "content")
	add("tool", "label", "name")
	add("tool", "omit", "description")
	add("choice", "label", "mode name")
	add("data", "label", "name tool_name reasoning_kind provider_error_type")
	add("data", "bool", "success reasoning_final prefer_oauth")
	add("data", "number", "input_tokens output_tokens cached_input_tokens cache_write_tokens provider_input_tokens provider_total_tokens reasoning_tokens request_context_tokens next_context_baseline reasoning_index text_len reasoning_encrypted_content_len attempt max_attempts wait_secs stdin_len exit_code duration_ms")
	add("data", "omit", "text arguments content output tool_info error env removed_env stdin stdout_tail stderr_tail cwd command_line phase id tool_call_id reasoning_item_id reasoning_encrypted_content_hash stdin_sha256")
	return schemas
})

func redactObject(input map[string]any, schema string) map[string]any {
	output := make(map[string]any)
	fields := exportSchemas()[schema]
	for key, value := range input {
		kind, known := fields[key]
		if !known {
			// Unknown keys can themselves be secrets. Do not copy their names.
			output["_redacted"] = true
			continue
		}
		output[key] = redactValue(value, kind)
	}
	return output
}

func redactValue(value any, kind string) any {
	switch kind {
	case "omit":
		return redacted
	case "label":
		if text, ok := value.(string); ok {
			return redactLabel(text)
		}
	case "timestamp":
		if text, ok := value.(string); ok {
			// Preserve only canonical timestamps, not arbitrary strings in this field.
			if ts, err := time.Parse(time.RFC3339Nano, text); err == nil {
				return ts.Format(time.RFC3339Nano)
			}
		}
	case "number":
		switch value.(type) {
		case float64, int, json.Number:
			return value
		}
	case "bool":
		if flag, ok := value.(bool); ok {
			return flag
		}
	default:
		if strings.HasPrefix(kind, "[]") {
			if items, ok := value.([]any); ok {
				result := make([]any, 0, len(items))
				for _, item := range items {
					result = append(result, redactValue(item, strings.TrimPrefix(kind, "[]")))
				}
				return result
			}
		} else if object, ok := value.(map[string]any); ok {
			return redactObject(object, kind)
		}
	}
	// Malformed types fail closed (including objects in scalar metadata fields).
	return redacted
}

// Defense in depth for retained labels, NOT a safety filter for arbitrary prose.
// Compile lazily so regular CLI startup and log writes pay no regex cost.
var exportLabelPatterns = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{16,}|gh[pousr]_[a-z0-9_]+|github_pat_[a-z0-9_]+|(?:AKIA|ASIA)[A-Z0-9]{16})`),
		regexp.MustCompile(`(?i)(eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|bearer\s+\S+|(?:api[_-]?key|password|secret|token)\s*[:=]|(?:^|[^a-z0-9/+=])[a-z0-9/+=]{40,}(?:$|[^a-z0-9/+=]))`),
		regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/() ,+@-]{0,159}$`),
	}
})

func redactLabel(text string) string {
	if text == "" {
		return ""
	}
	patterns := exportLabelPatterns()
	if patterns[0].MatchString(text) || patterns[1].MatchString(text) || !patterns[2].MatchString(text) {
		return redacted
	}
	return text
}

func redactEntry(entry map[string]any) {
	projected := redactObject(entry, "entry")
	clear(entry)
	for key, value := range projected {
		entry[key] = value
	}
}

// Make a detached view for Markdown and top-level JSON metadata. All freeform
// message content is replaced wholesale, regardless of its nested representation.
func redactSession(session *Session) *Session {
	result := *session
	result.ID = redacted // Correlation/session keys are not needed for sharing.
	result.Provider, result.Model = redactLabel(session.Provider), redactLabel(session.Model)
	result.Command, result.Cwd, result.Args = "", "", nil
	result.Entries = make([]any, 0, len(session.Entries))
	for _, entry := range session.Entries {
		switch e := entry.(type) {
		case RequestEntry:
			e.SessionID = redacted
			e.Provider, e.Model = redactLabel(e.Provider), redactLabel(e.Model)
			e.Request = redactRequest(e.Request)
			result.Entries = append(result.Entries, e)
		case EventEntry:
			e.SessionID = redacted
			e.EventType = redactLabel(e.EventType)
			e.Data = redactObject(e.Data, "data")
			result.Entries = append(result.Entries, e)
		}
	}
	return &result
}

func redactRequest(request RequestData) RequestData {
	request.SessionID = ""
	request.ReasoningEffort = redactLabel(request.ReasoningEffort)
	messages := make([]Message, len(request.Messages))
	for i, msg := range request.Messages {
		messages[i] = Message{Role: redactLabel(msg.Role), Content: redacted}
	}
	request.Messages = messages
	tools := make([]Tool, len(request.Tools))
	for i, tool := range request.Tools {
		tools[i] = Tool{Name: redactLabel(tool.Name), Description: redacted}
	}
	request.Tools = tools
	if request.ToolChoice != nil {
		request.ToolChoice = &ToolChoice{Mode: redactLabel(request.ToolChoice.Mode), Name: redactLabel(request.ToolChoice.Name)}
	}
	return request
}

func exportRedactedRaw(w io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := newDebugLogScanner(file)
	encoder := json.NewEncoder(w)
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry == nil {
			// Never echo invalid JSON, even in an error: it can be private content.
			entry = map[string]any{"_redacted": true}
		} else {
			redactEntry(entry)
		}
		if err := encoder.Encode(entry); err != nil {
			return err
		}
	}
	return scanner.Err()
}
