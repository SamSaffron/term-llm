// Package diff provides shared constants for diff handling across packages.
package diff

// MaxDiffSize is the maximum size of old/new content for diff emission and parsing (20KB).
// This constant is shared between tools that emit diff markers (internal/tools/edit.go)
// and the UI that parses them (internal/ui/stream_adapter.go).
const MaxDiffSize = 20 * 1024
