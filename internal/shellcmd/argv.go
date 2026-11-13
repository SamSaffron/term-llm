package shellcmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// SplitShellWords parses a command line into argv without invoking a shell.
func SplitShellWords(input string) ([]string, error) {
	var (
		args         []string
		current      strings.Builder
		inSingle     bool
		inDouble     bool
		escaped      bool
		tokenStarted bool
	)

	flush := func() {
		if tokenStarted {
			args = append(args, current.String())
			current.Reset()
			tokenStarted = false
		}
	}

	for _, r := range input {
		switch {
		case escaped:
			current.WriteRune(r)
			tokenStarted = true
			escaped = false

		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				current.WriteRune(r)
				tokenStarted = true
			}

		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			default:
				current.WriteRune(r)
				tokenStarted = true
			}

		default:
			switch r {
			case '\'':
				inSingle = true
				tokenStarted = true
			case '"':
				inDouble = true
				tokenStarted = true
			case '\\':
				escaped = true
				tokenStarted = true
			case ' ', '\t', '\n', '\r':
				flush()
			default:
				current.WriteRune(r)
				tokenStarted = true
			}
		}
	}

	if escaped || inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quoted argument")
	}
	flush()
	return args, nil
}

// HasUnsafeShellSyntax reports shell operators or expansions requiring a shell.
func HasUnsafeShellSyntax(input string) bool {
	inSingle := false
	inDouble := false
	escaped := false

	for _, r := range input {
		switch {
		case escaped:
			escaped = false
			continue

		case inSingle:
			if r == '\'' {
				inSingle = false
			}
			continue

		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			case '$', '`':
				return true
			}
			continue

		default:
			switch r {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '\\':
				escaped = true
			case ';', '|', '&', '<', '>', '(', ')', '\n', '\r', '$', '`':
				return true
			}
		}
	}

	return false
}

// ValidateApprovalPattern validates the quoted argv and each word's glob syntax.
func ValidateApprovalPattern(pattern string) error {
	parts, err := SplitShellWords(pattern)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("shell pattern is required")
	}
	for _, part := range parts {
		if _, err := doublestar.Match(part, ""); err != nil {
			return fmt.Errorf("invalid glob in shell word %q: %w", part, err)
		}
	}
	return nil
}

// MatchPattern matches one shell word using the validated glob grammar.
func MatchPattern(pattern, value string) bool {
	matched, err := doublestar.Match(pattern, value)
	return err == nil && matched
}
