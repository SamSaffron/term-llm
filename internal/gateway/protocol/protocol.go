// Package protocol defines the private, versioned inference-gateway wire
// protocol. It intentionally contains no provider credentials or local paths.
package protocol

import (
	"encoding/json"
	"time"
)

const (
	Version       = 1
	VersionHeader = "Term-LLM-Gateway-Version"
	BasePath      = "/g1"
)

type Error struct {
	Code              string `json:"code"`
	Message           string `json:"message,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
	SupportedVersions []int  `json:"supported_versions,omitempty"`
}

type InferenceRequest struct {
	Version   int             `json:"version"`
	RequestID string          `json:"request_id"`
	Provider  string          `json:"provider"`
	State     string          `json:"state,omitempty"`
	Request   json.RawMessage `json:"request"`
}

type StreamRecord struct {
	Version      int             `json:"version"`
	Type         string          `json:"type"`
	RequestID    string          `json:"request_id,omitempty"`
	RunID        string          `json:"run_id,omitempty"`
	Event        json.RawMessage `json:"event,omitempty"`
	CallbackPath string          `json:"callback_path,omitempty"`
	State        string          `json:"state,omitempty"`
	Error        *Error          `json:"error,omitempty"`
}

type ToolResultRequest struct {
	Version int             `json:"version"`
	Result  json.RawMessage `json:"result"`
}

type Health struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
}

type Catalog struct {
	Version     int             `json:"version"`
	GeneratedAt time.Time       `json:"generated_at"`
	Providers   []CatalogEntry  `json:"providers"`
	Features    CatalogFeatures `json:"features"`
}

type CatalogFeatures struct {
	Search bool `json:"search"`
	Fetch  bool `json:"fetch"`
}

type CatalogEntry struct {
	Key                 string       `json:"key"`
	Type                string       `json:"type"`
	CLI                 bool         `json:"cli,omitempty"`
	AllowUnlistedModels bool         `json:"allow_unlisted_models,omitempty"`
	Capabilities        Capabilities `json:"capabilities"`
	Models              []Model      `json:"models"`
}

type Capabilities struct {
	NativeWebSearch         bool `json:"native_web_search,omitempty"`
	NativeWebFetch          bool `json:"native_web_fetch,omitempty"`
	ToolCalls               bool `json:"tool_calls,omitempty"`
	SupportsToolChoice      bool `json:"supports_tool_choice,omitempty"`
	ManagesOwnContext       bool `json:"manages_own_context,omitempty"`
	InlineToolLoop          bool `json:"inline_tool_loop,omitempty"`
	OrderedInlineToolEvents bool `json:"ordered_inline_tool_events,omitempty"`
}

type Model struct {
	ID                     string   `json:"id"`
	DisplayName            string   `json:"display_name,omitempty"`
	Created                int64    `json:"created,omitempty"`
	OwnedBy                string   `json:"owned_by,omitempty"`
	InputLimit             int      `json:"input_limit,omitempty"`
	OutputLimit            int      `json:"output_limit,omitempty"`
	InputPrice             float64  `json:"input_price"`
	OutputPrice            float64  `json:"output_price"`
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	ReasoningModes         []string `json:"reasoning_modes,omitempty"`
}

type SearchRequest struct {
	Version    int    `json:"version"`
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

type SearchResponse struct {
	Version int            `json:"version"`
	Results []SearchResult `json:"results"`
}

type FetchRequest struct {
	Version int    `json:"version"`
	URL     string `json:"url"`
}

type FetchResponse struct {
	Version int    `json:"version"`
	Content string `json:"content"`
}

type EnrollmentRequest struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
}

type EnrollmentResponse struct {
	Version  int    `json:"version"`
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
}
