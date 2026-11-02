package acp

import "encoding/json"

const ProtocolVersion1 = 1

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type ClientCapabilities struct {
	FileSystem FileSystemCapabilities `json:"fs"`
	Terminal   bool                   `json:"terminal"`
	Meta       json.RawMessage        `json:"_meta,omitempty"`
}

type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
	Meta               json.RawMessage    `json:"_meta,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type SessionCapabilities struct {
	Resume                json.RawMessage `json:"resume,omitempty"`
	Close                 json.RawMessage `json:"close,omitempty"`
	List                  json.RawMessage `json:"list,omitempty"`
	Delete                json.RawMessage `json:"delete,omitempty"`
	AdditionalDirectories json.RawMessage `json:"additionalDirectories,omitempty"`
}

func capabilityPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null" && string(raw) != "false"
}

func (c SessionCapabilities) SupportsResume() bool { return capabilityPresent(c.Resume) }
func (c SessionCapabilities) SupportsClose() bool  { return capabilityPresent(c.Close) }

type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	MCPCapabilities     MCPCapabilities     `json:"mcpCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
	Meta                json.RawMessage     `json:"_meta,omitempty"`
}

type AuthMethod struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Type        string          `json:"type,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	Meta              json.RawMessage   `json:"_meta,omitempty"`
}

type AuthenticateRequest struct {
	MethodID string          `json:"methodId"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer represents either the baseline stdio transport or a capability-
// gated HTTP/SSE transport.
type MCPServer struct {
	Type    string        `json:"type,omitempty"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []Header      `json:"headers,omitempty"`
}

type NewSessionRequest struct {
	CWD                   string          `json:"cwd"`
	MCPServers            []MCPServer     `json:"mcpServers"`
	AdditionalDirectories []string        `json:"additionalDirectories,omitempty"`
	Meta                  json.RawMessage `json:"_meta,omitempty"`
}

type NewSessionResponse struct {
	SessionID     string          `json:"sessionId"`
	Modes         json.RawMessage `json:"modes,omitempty"`
	ConfigOptions json.RawMessage `json:"configOptions,omitempty"`
	Meta          json.RawMessage `json:"_meta,omitempty"`
}

type LoadSessionRequest struct {
	SessionID             string          `json:"sessionId"`
	CWD                   string          `json:"cwd"`
	MCPServers            []MCPServer     `json:"mcpServers"`
	AdditionalDirectories []string        `json:"additionalDirectories,omitempty"`
	Meta                  json.RawMessage `json:"_meta,omitempty"`
}

type LoadSessionResponse struct {
	Modes         json.RawMessage `json:"modes,omitempty"`
	ConfigOptions json.RawMessage `json:"configOptions,omitempty"`
	Meta          json.RawMessage `json:"_meta,omitempty"`
}

type ResumeSessionRequest = LoadSessionRequest
type ResumeSessionResponse = LoadSessionResponse

type CloseSessionRequest struct {
	SessionID string          `json:"sessionId"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type ContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     string          `json:"data,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
	URI      string          `json:"uri,omitempty"`
	Name     string          `json:"name,omitempty"`
	Resource json.RawMessage `json:"resource,omitempty"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

type PromptRequest struct {
	SessionID string          `json:"sessionId"`
	Prompt    []ContentBlock  `json:"prompt"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type PromptResponse struct {
	StopReason string `json:"stopReason"`
	// Usage is the agent's token usage report; its scope (turn or session)
	// is agent-defined.
	Usage json.RawMessage `json:"usage,omitempty"`
	Meta  json.RawMessage `json:"_meta,omitempty"`
}

type CancelSessionRequest struct {
	SessionID string          `json:"sessionId"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}
