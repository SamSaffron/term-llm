// Package share defines provider-neutral transcript sharing.
package share

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/agents/gist"
)

const (
	Protocol = "term-llm-share"
	Version  = 1

	ProviderGitHub ProviderID = ProviderID(gist.ProviderID)

	MaxRequestFiles      = 32
	MaxRequestFileBytes  = 16 << 20
	MaxRequestTotalBytes = 32 << 20

	maxProviderNameBytes     = 128
	maxProviderHelpBytes     = 2048
	maxCapabilityNotes       = 16
	maxCapabilityNoteBytes   = 1024
	maxCapabilityLimitsBytes = 16 << 10
	maxStructuredErrorBytes  = 1024
	maxDiagnosticLogBytes    = 4096
)

type ProviderID string

type Visibility string

const (
	VisibilityPublic   Visibility = "public"
	VisibilityUnlisted Visibility = "unlisted"
	VisibilityPrivate  Visibility = "private"
)

type Operation string

const (
	OperationCapabilities Operation = "capabilities"
	OperationCreate       Operation = "create"
	OperationUpdate       Operation = "update"
)

type Provider struct {
	ID   ProviderID `json:"id"`
	Name string     `json:"name"`
	Help string     `json:"help,omitempty"`
}

type Capabilities struct {
	Protocol          string         `json:"protocol"`
	Version           int            `json:"version"`
	Provider          Provider       `json:"provider"`
	Operations        []Operation    `json:"operations"`
	Visibilities      []Visibility   `json:"visibilities"`
	DefaultVisibility Visibility     `json:"default_visibility"`
	Notes             []string       `json:"notes,omitempty"`
	Limits            map[string]any `json:"limits,omitempty"`
}

func (c Capabilities) Supports(operation Operation) bool {
	return slices.Contains(c.Operations, operation)
}

func (c Capabilities) SupportsVisibility(visibility Visibility) bool {
	return slices.Contains(c.Visibilities, visibility)
}

type File struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Role      string `json:"role"`
	Content   []byte `json:"-"`
}

type Request struct {
	RequestID   string     `json:"request_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Visibility  Visibility `json:"visibility"`
	Entrypoint  string     `json:"entrypoint"`
	Files       []File     `json:"files"`
}

type Result struct {
	Provider   ProviderID `json:"provider"`
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	SourceURL  string     `json:"source_url,omitempty"`
	Visibility Visibility `json:"visibility"`
	Ready      bool       `json:"ready"`
}

type ErrorCode string

const (
	ErrorDependencyMissing     ErrorCode = "dependency_missing"
	ErrorAuthRequired          ErrorCode = "auth_required"
	ErrorTimeout               ErrorCode = "timeout"
	ErrorProvider              ErrorCode = "provider_error"
	ErrorProtocol              ErrorCode = "protocol_error"
	ErrorUnsupportedVisibility ErrorCode = "unsupported_visibility"
)

var stableErrorCodes = []ErrorCode{
	ErrorDependencyMissing,
	ErrorAuthRequired,
	ErrorTimeout,
	ErrorProvider,
	ErrorProtocol,
	ErrorUnsupportedVisibility,
}

type Error struct {
	Code       ErrorCode `json:"code"`
	Message    string    `json:"message"`
	diagnostic string
	cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return "sharing failed"
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func NewError(code ErrorCode, message string) *Error {
	if !slices.Contains(stableErrorCodes, code) {
		code = ErrorProvider
	}
	return &Error{Code: code, Message: strings.TrimSpace(message)}
}

func errorWithDiagnostic(code ErrorCode, message, diagnostic string, cause error) *Error {
	err := NewError(code, message)
	err.diagnostic = boundedDiagnostic(diagnostic)
	err.cause = cause
	if err.diagnostic != "" {
		// Diagnostics stay on the operator log surface. Quoting prevents helper
		// control bytes from affecting terminals, and Error() never exposes them
		// to Web or TUI users.
		log.Printf("[share] provider diagnostic: %q", err.diagnostic)
	}
	return err
}

func boundedDiagnostic(value string) string {
	if len(value) <= maxDiagnosticLogBytes {
		return value
	}
	return value[:maxDiagnosticLogBytes] + "…(truncated)"
}

func AsError(err error) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	return errorWithDiagnostic(ErrorProvider, "sharing provider failed", "", err)
}

type Publisher interface {
	Capabilities(context.Context) (Capabilities, error)
	Create(context.Context, Request) (Result, error)
}

// Updater is implemented only by publishers that can replace an existing share.
// Callers must also require the update operation in Capabilities.
type Updater interface {
	Update(context.Context, string, Request) (Result, error)
}

func ValidateCapabilities(c Capabilities) error {
	if c.Protocol != Protocol {
		return fmt.Errorf("protocol must be %q", Protocol)
	}
	if c.Version != Version {
		return fmt.Errorf("version must be %d", Version)
	}
	if !validProviderID(c.Provider.ID) {
		return fmt.Errorf("provider.id must be 1 to 64 printable ASCII bytes without whitespace")
	}
	if err := validateDisplayText("provider.name", c.Provider.Name, maxProviderNameBytes, false); err != nil {
		return err
	}
	if err := validateDisplayText("provider.help", c.Provider.Help, maxProviderHelpBytes, true); err != nil {
		return err
	}
	if len(c.Operations) > 2 {
		return fmt.Errorf("operations must contain at most create and update")
	}
	if !c.Supports(OperationCreate) {
		return fmt.Errorf("operations must include create")
	}
	if len(c.Visibilities) == 0 {
		return fmt.Errorf("visibilities cannot be empty")
	}
	seen := make(map[Visibility]bool, len(c.Visibilities))
	for _, visibility := range c.Visibilities {
		if !ValidVisibility(visibility) {
			return fmt.Errorf("unsupported visibility %q", visibility)
		}
		if seen[visibility] {
			return fmt.Errorf("duplicate visibility %q", visibility)
		}
		seen[visibility] = true
	}
	if !seen[c.DefaultVisibility] {
		return fmt.Errorf("default_visibility must be one of visibilities")
	}
	operationSeen := make(map[Operation]bool, len(c.Operations))
	for _, operation := range c.Operations {
		if operation != OperationCreate && operation != OperationUpdate {
			return fmt.Errorf("unsupported operation %q", operation)
		}
		if operationSeen[operation] {
			return fmt.Errorf("duplicate operation %q", operation)
		}
		operationSeen[operation] = true
	}
	if len(c.Notes) > maxCapabilityNotes {
		return fmt.Errorf("notes must contain at most %d entries", maxCapabilityNotes)
	}
	for i, note := range c.Notes {
		if err := validateDisplayText(fmt.Sprintf("notes[%d]", i), note, maxCapabilityNoteBytes, false); err != nil {
			return err
		}
	}
	if err := validateLimits(c.Limits); err != nil {
		return err
	}
	return nil
}

// ValidateHelperCapabilities applies the protocol's generic validation and
// prevents external helpers from claiming a built-in provider identity.
func ValidateHelperCapabilities(c Capabilities) error {
	if err := ValidateCapabilities(c); err != nil {
		return err
	}
	if c.Provider.ID == ProviderGitHub {
		return fmt.Errorf("provider.id %q is reserved for the built-in GitHub provider", ProviderGitHub)
	}
	return nil
}

func validateDisplayText(field, value string, maxBytes int, allowEmpty bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s must be at most %d bytes", field, maxBytes)
	}
	if !allowEmpty && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s cannot be empty", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s must not contain control characters", field)
		}
	}
	return nil
}

func validateStructuredErrorMessage(value string) error {
	return validateDisplayText("error.message", value, maxStructuredErrorBytes, false)
}

func validateLimits(limits map[string]any) error {
	if len(limits) == 0 {
		return nil
	}
	if len(limits) > 32 {
		return fmt.Errorf("limits must contain at most 32 entries")
	}
	encoded, err := json.Marshal(limits)
	if err != nil {
		return fmt.Errorf("limits must be valid JSON: %w", err)
	}
	if len(encoded) > maxCapabilityLimitsBytes {
		return fmt.Errorf("limits must encode to at most %d bytes", maxCapabilityLimitsBytes)
	}
	return validateLimitValue("limits", limits, 0)
}

func validateLimitValue(field string, value any, depth int) error {
	if depth > 4 {
		return fmt.Errorf("%s exceeds maximum nesting depth", field)
	}
	switch typed := value.(type) {
	case nil, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return nil
	case string:
		return validateDisplayText(field, typed, maxCapabilityNoteBytes, true)
	case []any:
		if len(typed) > 32 {
			return fmt.Errorf("%s array must contain at most 32 entries", field)
		}
		for i, item := range typed {
			if err := validateLimitValue(fmt.Sprintf("%s[%d]", field, i), item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if len(typed) > 32 {
			return fmt.Errorf("%s object must contain at most 32 entries", field)
		}
		for key, item := range typed {
			if err := validateDisplayText(field+" key", key, 64, false); err != nil {
				return err
			}
			if err := validateLimitValue(field+"."+key, item, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s contains unsupported value type %T", field, value)
	}
}

func validProviderID(id ProviderID) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, b := range []byte(id) {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

func ValidVisibility(visibility Visibility) bool {
	switch visibility {
	case VisibilityPublic, VisibilityUnlisted, VisibilityPrivate:
		return true
	default:
		return false
	}
}

func ValidateRequest(req Request) error {
	if err := validateDisplayText("request_id", req.RequestID, 128, false); err != nil {
		return err
	}
	if err := validateDisplayText("title", req.Title, 512, true); err != nil {
		return err
	}
	if err := validateDisplayText("description", req.Description, 2048, true); err != nil {
		return err
	}
	if !ValidVisibility(req.Visibility) {
		return fmt.Errorf("visibility is invalid")
	}
	if req.Entrypoint != "index.html" {
		return fmt.Errorf("entrypoint must be index.html")
	}
	if len(req.Files) == 0 {
		return fmt.Errorf("files cannot be empty")
	}
	if len(req.Files) > MaxRequestFiles {
		return fmt.Errorf("files must contain at most %d entries", MaxRequestFiles)
	}
	entrypointFound := false
	seen := make(map[string]bool, len(req.Files))
	totalBytes := 0
	for _, file := range req.Files {
		if !validRelativeName(file.Name) || len(file.Name) > 255 {
			return fmt.Errorf("file name %q must be a relative path of at most 255 bytes", file.Name)
		}
		if seen[file.Name] {
			return fmt.Errorf("duplicate file name %q", file.Name)
		}
		seen[file.Name] = true
		if file.Name == req.Entrypoint {
			entrypointFound = true
		}
		if err := validateDisplayText(fmt.Sprintf("file %q media_type", file.Name), file.MediaType, 128, false); err != nil {
			return err
		}
		if err := validateDisplayText(fmt.Sprintf("file %q role", file.Name), file.Role, 128, false); err != nil {
			return err
		}
		if len(file.Content) > MaxRequestFileBytes {
			return fmt.Errorf("file %q exceeds the %d byte limit", file.Name, MaxRequestFileBytes)
		}
		totalBytes += len(file.Content)
		if totalBytes > MaxRequestTotalBytes {
			return fmt.Errorf("share bundle exceeds the %d byte limit", MaxRequestTotalBytes)
		}
	}
	if !entrypointFound {
		return fmt.Errorf("entrypoint is not present in files")
	}
	if !seen["session.md"] {
		return fmt.Errorf("files must include session.md")
	}
	return nil
}

func validRelativeName(name string) bool {
	if name == "" || !utf8.ValidString(name) || filepath.IsAbs(name) || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	clean := filepath.Clean(name)
	return clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func ValidateResult(result Result) error {
	if !validID(result.ID) {
		return fmt.Errorf("id must be 1 to 256 printable ASCII bytes without whitespace")
	}
	if err := validateURL(result.URL); err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if result.SourceURL != "" {
		if err := validateURL(result.SourceURL); err != nil {
			return fmt.Errorf("source_url: %w", err)
		}
	}
	if !ValidVisibility(result.Visibility) {
		return fmt.Errorf("visibility is invalid")
	}
	return nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, b := range []byte(id) {
		if b < 0x21 || b > 0x7e || unicode.IsSpace(rune(b)) {
			return false
		}
	}
	return true
}

func validateURL(raw string) error {
	if len(raw) == 0 || len(raw) > 2048 {
		return fmt.Errorf("must be 1 to 2048 bytes")
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("must not contain whitespace or control characters")
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("must be an absolute HTTP(S) URL without user information")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" || !loopbackHost(parsed.Hostname()) {
		return fmt.Errorf("must use HTTPS (HTTP is allowed only for loopback hosts)")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var fallbackRequestID atomic.Uint64

func NewRequestID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), fallbackRequestID.Add(1))
}

func TranscriptFiles(files map[string]string) []File {
	result := make([]File, 0, len(files))
	for name, content := range files {
		mediaType, role := "text/plain; charset=utf-8", "attachment"
		switch name {
		case "index.html":
			mediaType, role = "text/html; charset=utf-8", "entrypoint"
		case "session.md":
			mediaType, role = "text/markdown; charset=utf-8", "transcript"
		}
		result = append(result, File{Name: name, MediaType: mediaType, Role: role, Content: []byte(content)})
	}
	slices.SortFunc(result, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	return result
}
