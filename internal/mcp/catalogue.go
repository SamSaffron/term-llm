package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	// MaxToolsPerServer is the defensive limit for a single complete tools/list acquisition.
	MaxToolsPerServer = 10_000
	// MaxNamespaceDescriptionBytes bounds untrusted MCP instructions copied into
	// native provider namespace declarations.
	MaxNamespaceDescriptionBytes = 4 * 1024
	maxNativeIdentityBytes       = 64
	nativeIdentityHashBytes      = 10
	toolSchemaFramingTokens      = 8
)

// ToolAnnotations preserves MCP tool behavior hints for retrieval and diagnostics.
// These are untrusted hints, not an execution-policy boundary.
type ToolAnnotations struct {
	Present     bool   `json:"present,omitempty"`
	Title       string `json:"title,omitempty"`
	ReadOnly    bool   `json:"read_only,omitempty"`
	Destructive *bool  `json:"destructive,omitempty"`
	Idempotent  bool   `json:"idempotent,omitempty"`
	OpenWorld   *bool  `json:"open_world,omitempty"`
}

// CatalogTool is an immutable catalogue entry acquired from an MCP server.
type CatalogTool struct {
	ID                   string          `json:"id"`
	Server               string          `json:"server"`
	OriginalName         string          `json:"original_name"`
	Name                 string          `json:"name"`
	Namespace            string          `json:"namespace,omitempty"`
	ChildName            string          `json:"child_name,omitempty"`
	NamespaceDescription string          `json:"namespace_description,omitempty"`
	Title                string          `json:"title,omitempty"`
	Description          string          `json:"description,omitempty"`
	InputSchema          map[string]any  `json:"input_schema"`
	OutputSchema         map[string]any  `json:"output_schema,omitempty"`
	Annotations          ToolAnnotations `json:"annotations,omitempty"`
	Metadata             map[string]any  `json:"metadata,omitempty"`
	SchemaHash           string          `json:"schema_hash"`
	EstimatedTokens      int             `json:"estimated_tokens"`
}

// ToolSnapshot is a complete immutable per-server catalogue publication.
type ToolSnapshot struct {
	Generation           uint64         `json:"generation"`
	FetchedAt            time.Time      `json:"fetched_at"`
	NamespaceDescription string         `json:"namespace_description,omitempty"`
	Tools                []CatalogTool  `json:"tools"`
	ByOriginal           map[string]int `json:"-"`
	Hash                 string         `json:"hash"`
}

// CatalogueSnapshot is a complete immutable manager-level namespaced catalogue.
type CatalogueSnapshot struct {
	Generation uint64        `json:"generation"`
	FetchedAt  time.Time     `json:"fetched_at"`
	Tools      []CatalogTool `json:"tools"`
	Hash       string        `json:"hash"`
}

// CatalogueEvent reports a committed aggregate catalogue change or a failed refresh.
type CatalogueEvent struct {
	Server   string
	Snapshot *CatalogueSnapshot
	Err      error
}

func catalogToolFromSDK(server string, tool *sdkmcp.Tool) (CatalogTool, error) {
	if tool == nil {
		return CatalogTool{}, fmt.Errorf("nil tool")
	}
	if tool.Name == "" {
		return CatalogTool{}, fmt.Errorf("tool name is required")
	}
	input, err := schemaObject(tool.InputSchema, true)
	if err != nil {
		return CatalogTool{}, fmt.Errorf("tool %q input schema: %w", tool.Name, err)
	}
	output, err := schemaObject(tool.OutputSchema, false)
	if err != nil {
		return CatalogTool{}, fmt.Errorf("tool %q output schema: %w", tool.Name, err)
	}
	ct := CatalogTool{
		ID:           tool.Name,
		Server:       server,
		OriginalName: tool.Name,
		Name:         tool.Name,
		Title:        tool.Title,
		Description:  tool.Description,
		InputSchema:  input,
		OutputSchema: output,
		Metadata:     cloneMap(map[string]any(tool.Meta)),
	}
	if tool.Annotations != nil {
		ct.Annotations = ToolAnnotations{
			Present:     true,
			Title:       tool.Annotations.Title,
			ReadOnly:    tool.Annotations.ReadOnlyHint,
			Destructive: cloneBool(tool.Annotations.DestructiveHint),
			Idempotent:  tool.Annotations.IdempotentHint,
			OpenWorld:   cloneBool(tool.Annotations.OpenWorldHint),
		}
	}
	if ct.Title == "" {
		ct.Title = ct.Annotations.Title
	}
	if err := finalizeCatalogTool(&ct); err != nil {
		return CatalogTool{}, err
	}
	return ct, nil
}

func namespaceCatalogTool(server, namespaceDescription string, tool CatalogTool) CatalogTool {
	tool.Server = server
	tool.OriginalName = tool.Name
	if tool.OriginalName == "" {
		tool.OriginalName = tool.ID
	}
	tool.Name = server + "__" + tool.OriginalName
	tool.ID = tool.Name
	tool.NamespaceDescription = boundedNamespaceDescription(namespaceDescription, server)
	if tool.Description != "" {
		tool.Description = fmt.Sprintf("[%s] %s", server, tool.Description)
	} else {
		tool.Description = fmt.Sprintf("[%s] MCP tool %s", server, tool.OriginalName)
	}
	return tool
}

// normalizeCatalogueIdentities assigns explicit native namespace/child identities
// and resolves the rare flattened-name collision without relying on delimiter
// parsing. Common flattened names remain byte-for-byte compatible.
func normalizeCatalogueIdentities(tools []CatalogTool) []CatalogTool {
	namespaceIDs := make([]string, 0, len(tools))
	childrenByServer := make(map[string][]string)
	for _, tool := range tools {
		namespaceIDs = append(namespaceIDs, tool.Server)
		childrenByServer[tool.Server] = append(childrenByServer[tool.Server], tool.OriginalName)
	}
	namespaces := normalizedIdentityNames(namespaceIDs)
	children := make(map[string]map[string]string, len(childrenByServer))
	for server, names := range childrenByServer {
		children[server] = normalizedIdentityNames(names)
	}
	flattened := collisionSafeFlattenedNames(tools)
	for i := range tools {
		tool := &tools[i]
		tool.Namespace = namespaces[tool.Server]
		tool.ChildName = children[tool.Server][tool.OriginalName]
		tool.Name = flattened[catalogIdentity(*tool)]
		tool.ID = tool.Name
		_ = finalizeCatalogTool(tool)
	}
	return tools
}

func catalogIdentity(tool CatalogTool) string {
	return tool.Server + "\x00" + tool.OriginalName
}

func collisionSafeFlattenedNames(tools []CatalogTool) map[string]string {
	groups := make(map[string][]string)
	for _, tool := range tools {
		identity := catalogIdentity(tool)
		base := tool.Server + "__" + tool.OriginalName
		groups[base] = append(groups[base], identity)
	}
	out := make(map[string]string, len(tools))
	used := make(map[string]string, len(tools))
	bases := make([]string, 0, len(groups))
	for base := range groups {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	for _, base := range bases {
		identities := uniqueSorted(groups[base])
		for _, identity := range identities {
			candidate := base
			if len(identities) > 1 {
				candidate = base + identityHashSuffix(identity)
			}
			for attempt := 0; ; attempt++ {
				if prior, exists := used[candidate]; !exists || prior == identity {
					used[candidate] = identity
					out[identity] = candidate
					break
				}
				candidate = base + identityHashSuffix(fmt.Sprintf("%s\x00%d", identity, attempt+1))
			}
		}
	}
	return out
}

func normalizedIdentityNames(rawNames []string) map[string]string {
	identities := uniqueSorted(rawNames)
	groups := make(map[string][]string)
	for _, identity := range identities {
		base := sanitizeNativeIdentity(identity)
		groups[base] = append(groups[base], identity)
	}
	out := make(map[string]string, len(identities))
	used := make(map[string]string, len(identities))
	bases := make([]string, 0, len(groups))
	for base := range groups {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	for _, base := range bases {
		group := groups[base]
		sort.Strings(group)
		for _, identity := range group {
			candidate := base
			if len(group) > 1 {
				candidate = appendIdentityHash(base, identity)
			}
			for attempt := 0; ; attempt++ {
				if prior, exists := used[candidate]; !exists || prior == identity {
					used[candidate] = identity
					out[identity] = candidate
					break
				}
				candidate = appendIdentityHash(base, fmt.Sprintf("%s\x00%d", identity, attempt+1))
			}
		}
	}
	return out
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sanitizeNativeIdentity(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.TrimSpace(b.String())
	if name == "" {
		name = "namespace"
	}
	return truncateUTF8Bytes(name, maxNativeIdentityBytes)
}

func appendIdentityHash(base, identity string) string {
	suffix := identityHashSuffix(identity)
	return truncateUTF8Bytes(base, maxNativeIdentityBytes-len(suffix)) + suffix
}

func identityHashSuffix(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "_" + hex.EncodeToString(sum[:])[:nativeIdentityHashBytes]
}

func boundedNamespaceDescription(description, server string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		description = fmt.Sprintf("Tools provided by the %s MCP server.", server)
	}
	return truncateUTF8Bytes(description, MaxNamespaceDescriptionBytes)
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func finalizeCatalogTool(tool *CatalogTool) error {
	providerShape := struct {
		Name                 string          `json:"name"`
		Namespace            string          `json:"namespace,omitempty"`
		ChildName            string          `json:"child_name,omitempty"`
		NamespaceDescription string          `json:"namespace_description,omitempty"`
		Title                string          `json:"title,omitempty"`
		Description          string          `json:"description,omitempty"`
		InputSchema          map[string]any  `json:"input_schema"`
		OutputSchema         map[string]any  `json:"output_schema,omitempty"`
		Annotations          ToolAnnotations `json:"annotations,omitempty"`
		ExecutionMeta        map[string]any  `json:"execution_metadata,omitempty"`
	}{tool.Name, tool.Namespace, tool.ChildName, tool.NamespaceDescription, tool.Title, tool.Description, tool.InputSchema, tool.OutputSchema, tool.Annotations, tool.Metadata}
	data, err := json.Marshal(providerShape)
	if err != nil {
		return fmt.Errorf("encode tool %q schema: %w", tool.Name, err)
	}
	sum := sha256.Sum256(data)
	tool.SchemaHash = hex.EncodeToString(sum[:])
	spec := tool.ToolSpec()
	// Namespace metadata is adapter routing state, not part of the flattened
	// portable function declaration used for the existing token estimate.
	spec.Namespace = nil
	specData, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("encode tool %q provider definition: %w", tool.Name, err)
	}
	tool.EstimatedTokens = llm.EstimateTokens(string(specData)) + toolSchemaFramingTokens
	return nil
}

// ToolSpec converts a catalogue entry to the provider-neutral callable shape.
func (t CatalogTool) ToolSpec() llm.ToolSpec {
	spec := llm.ToolSpec{
		Name:         t.Name,
		Description:  t.Description,
		Schema:       t.InputSchema,
		OutputSchema: t.OutputSchema,
	}
	if t.Namespace != "" && t.ChildName != "" {
		spec.Namespace = &llm.ToolNamespaceIdentity{
			Name:        t.Namespace,
			ChildName:   t.ChildName,
			Description: t.NamespaceDescription,
		}
	}
	return spec
}

func buildToolSnapshot(server string, generation uint64, tools []*sdkmcp.Tool) (*ToolSnapshot, error) {
	return buildToolSnapshotWithDescription(server, generation, "", tools)
}

func buildToolSnapshotWithDescription(server string, generation uint64, namespaceDescription string, tools []*sdkmcp.Tool) (*ToolSnapshot, error) {
	candidate := &ToolSnapshot{
		Generation:           generation,
		FetchedAt:            time.Now().UTC(),
		NamespaceDescription: truncateUTF8Bytes(strings.TrimSpace(namespaceDescription), MaxNamespaceDescriptionBytes),
		Tools:                make([]CatalogTool, 0, len(tools)),
		ByOriginal:           make(map[string]int, len(tools)),
	}
	for _, tool := range tools {
		ct, err := catalogToolFromSDK(server, tool)
		if err != nil {
			return nil, err
		}
		if _, exists := candidate.ByOriginal[ct.OriginalName]; exists {
			return nil, fmt.Errorf("duplicate tool name %q", ct.OriginalName)
		}
		candidate.ByOriginal[ct.OriginalName] = len(candidate.Tools)
		candidate.Tools = append(candidate.Tools, ct)
	}
	// Preserve server pagination order in Tools, while hashing a deterministic identity projection.
	hashes := make([]string, 0, len(candidate.Tools))
	for _, tool := range candidate.Tools {
		hashes = append(hashes, tool.OriginalName+":"+tool.SchemaHash)
	}
	sort.Strings(hashes)
	data, _ := json.Marshal(hashes)
	sum := sha256.Sum256(data)
	candidate.Hash = hex.EncodeToString(sum[:])
	return candidate, nil
}

func schemaObject(value any, required bool) (map[string]any, error) {
	if value == nil {
		if required {
			return map[string]any{}, nil
		}
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("must be a JSON object")
	}
	if object == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}
	return object, nil
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyToolSnapshot(snapshot *ToolSnapshot) *ToolSnapshot {
	if snapshot == nil {
		return nil
	}
	copy := *snapshot
	copy.Tools = make([]CatalogTool, len(snapshot.Tools))
	for i, tool := range snapshot.Tools {
		copy.Tools[i] = cloneCatalogTool(tool)
	}
	copy.ByOriginal = make(map[string]int, len(snapshot.ByOriginal))
	for name, index := range snapshot.ByOriginal {
		copy.ByOriginal[name] = index
	}
	return &copy
}

func copyCatalogueSnapshot(snapshot *CatalogueSnapshot) *CatalogueSnapshot {
	if snapshot == nil {
		return nil
	}
	copy := *snapshot
	copy.Tools = make([]CatalogTool, len(snapshot.Tools))
	for i, tool := range snapshot.Tools {
		copy.Tools[i] = cloneCatalogTool(tool)
	}
	return &copy
}

func cloneCatalogTool(tool CatalogTool) CatalogTool {
	tool.InputSchema = cloneMap(tool.InputSchema)
	tool.OutputSchema = cloneMap(tool.OutputSchema)
	tool.Metadata = cloneMap(tool.Metadata)
	tool.Annotations.Destructive = cloneBool(tool.Annotations.Destructive)
	tool.Annotations.OpenWorld = cloneBool(tool.Annotations.OpenWorld)
	return tool
}
