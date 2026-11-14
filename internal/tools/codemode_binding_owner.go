package tools

import (
	"reflect"

	"github.com/term-llm/term-llm/internal/llm/core"
)

// ownedCodeModeNames runs under r.mu and prevents name-only authority capture.
// Direct authority unit callers may omit Tools; engine admission always supplies
// the exact server instances. An engine override must retain its own execution
// semantics, not acquire a synthesized local file/shell/service implementation.
func (r *LocalToolRegistry) ownedCodeModeNames(ref core.CodeModeCellRef, names []string) []string {
	owned := make([]string, 0, len(names))
	for _, name := range names {
		local, exists := r.tools[name]
		if !exists || local == nil {
			continue
		}
		if ref.Tools != nil {
			admitted := ref.Tools[name]
			typ := reflect.TypeOf(local)
			if typ != reflect.TypeOf(admitted) || !reflect.ValueOf(local).Comparable() || !reflect.ValueOf(admitted).Comparable() || local != admitted {
				continue
			}
		}
		owned = append(owned, name)
	}
	return owned
}
