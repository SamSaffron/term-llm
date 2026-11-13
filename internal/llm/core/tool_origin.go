package core

import (
	"reflect"
	"strings"
)

// ToolOriginClass labels the trusted local subsystem that produced a tool.
// Provenance is diagnostics and collision metadata only: it never grants
// permission, script exposure, or a runtime binding.
type ToolOriginClass string

const (
	// ToolOriginUnclassified is the fail-safe family identity for a tool that
	// does not declare provenance. It is derived from the concrete Go type, not
	// from the provider-visible tool name.
	ToolOriginUnclassified ToolOriginClass = "unclassified"
	ToolOriginBuiltin      ToolOriginClass = "builtin"
	ToolOriginCustomAgent  ToolOriginClass = "custom_agent"
	ToolOriginSkill        ToolOriginClass = "skill"
	ToolOriginMCP          ToolOriginClass = "mcp"
	ToolOriginWeb          ToolOriginClass = "web"
	ToolOriginPlanner      ToolOriginClass = "planner"
	ToolOriginHost         ToolOriginClass = "host"
)

// ToolOrigin identifies the trusted origin instance that owns an execution name.
// ID distinguishes instances inside one class, for example an MCP server name, an
// agent definition directory, or a skill source directory. An empty ID means the
// class itself is the full identity.
type ToolOrigin struct {
	Class ToolOriginClass
	ID    string
}

// IsZero reports whether no provenance was supplied.
func (o ToolOrigin) IsZero() bool { return o.Class == "" && o.ID == "" }

// String renders a stable diagnostic label. It is not used for routing.
func (o ToolOrigin) String() string {
	if o.Class == "" {
		if o.ID == "" {
			return "unspecified"
		}
		return strings.ReplaceAll(o.ID, "\x00", "/")
	}
	if o.ID == "" {
		return string(o.Class)
	}
	return string(o.Class) + ":" + strings.ReplaceAll(o.ID, "\x00", "/")
}

// SameOrigin reports whether two origins identify the same trusted owner.
// Unclassified origins retain their concrete-type identity; deliberate
// cross-type replacement requires an explicit registry override.
func (o ToolOrigin) SameOrigin(other ToolOrigin) bool { return o == other }

// ToolOriginProvider is an optional declaration on the registered tool instance.
// Wrappers that embed core.Tool must forward this method explicitly; embedding an
// interface does not promote optional methods of the wrapped dynamic value.
type ToolOriginProvider interface {
	ToolOrigin() ToolOrigin
}

// ToolBindingIDProvider optionally supplies an opaque identity for a registered
// execution binding. The registry keeps a stable binding generation while this
// ID is unchanged, which lets an identical MCP catalogue refresh remain
// idempotent while a same-schema replacement with a new binding invalidates old
// holders. Empty IDs fall back to instance identity.
type ToolBindingIDProvider interface {
	ToolBindingID() string
}

// ToolOriginOf resolves declared provenance, falling back to the concrete Go type
// so unrelated local implementations cannot silently replace each other under one
// execution name. The fallback deliberately ignores Spec().Name.
func ToolOriginOf(tool Tool) ToolOrigin {
	if tool == nil {
		return ToolOrigin{}
	}
	if !isNilToolValue(tool) {
		if provider, ok := tool.(ToolOriginProvider); ok {
			if origin := provider.ToolOrigin(); origin.Class != "" {
				return origin
			}
		}
	}
	return ToolOrigin{Class: ToolOriginUnclassified, ID: reflect.TypeOf(tool).String()}
}

// ToolBindingIDOf returns an explicitly declared binding identity, if any.
func ToolBindingIDOf(tool Tool) string {
	if tool == nil || isNilToolValue(tool) {
		return ""
	}
	if provider, ok := tool.(ToolBindingIDProvider); ok {
		return provider.ToolBindingID()
	}
	return ""
}

func isNilToolValue(tool Tool) bool {
	value := reflect.ValueOf(tool)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
