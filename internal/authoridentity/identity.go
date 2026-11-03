// Package authoridentity validates display-only sender identities and records CLI authors without replacing directory metadata.
package authoridentity

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

// ValidValue applies the shared proxy/CLI identity limits after normalization.
func ValidValue(value string, byteLimit, runeLimit int) bool {
	if value == "" || !utf8.ValidString(value) || (byteLimit > 0 && len(value) > byteLimit) || (runeLimit > 0 && utf8.RuneCountInString(value) > runeLimit) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ParseName uses one value as both author ID and display name. CLI input is not
// RFC 2047 decoded: it is a literal name, unlike optional proxy display headers.
func ParseName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !ValidValue(value, 256, 128) {
		return "", fmt.Errorf("user must be non-empty valid UTF-8, contain no control characters, and fit within 256 bytes and 128 characters")
	}
	return value, nil
}

// ParseFlag distinguishes an omitted flag from an explicitly empty --user.
func ParseFlag(value string, explicit bool) (string, error) {
	if value == "" && !explicit {
		return "", nil
	}
	name, err := ParseName(value)
	if err != nil {
		return "", fmt.Errorf("invalid --user: %w", err)
	}
	return name, nil
}

// Stamp records a new human turn. CLI names are insert-if-missing snapshots,
// never replacements for richer proxy-header names. Empty names are anonymous.
func Stamp(ctx context.Context, store session.Store, message *core.Message, name string) error {
	if message.Role != core.RoleUser {
		return nil
	}
	message.AuthorID = name
	if name != "" {
		if authors, ok := session.AsAuthorStore(store); ok {
			if err := authors.UpsertAuthor(ctx, session.Author{ID: name, Name: name}, true); err != nil {
				return fmt.Errorf("record user: %w", err)
			}
		}
	}
	return nil
}
