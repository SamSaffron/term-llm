package shellcmd

import "strings"

// SplitPipeCommands splits on single pipe (|) but NOT on ||, &&, or ;.
func SplitPipeCommands(input string) []string {
	var commands []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	runes := []rune(input)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case inSingle:
			current.WriteRune(r)
			if r == '\'' {
				inSingle = false
			}
		case inDouble:
			current.WriteRune(r)
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			}
		default:
			switch {
			case r == '\'':
				inSingle = true
				current.WriteRune(r)
			case r == '"':
				inDouble = true
				current.WriteRune(r)
			case r == '\\':
				escaped = true
				current.WriteRune(r)
			case r == '|' && i+1 < len(runes) && runes[i+1] == '|':
				// || is a sequential operator, preserve it
				current.WriteRune(r)
				current.WriteRune(runes[i+1])
				i++ // skip second |
			case r == '|':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
			default:
				current.WriteRune(r)
			}
		}
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		commands = append(commands, s)
	}
	return commands
}

func ExtractCommandPrefix(cmd string) string {
	tokens := TokenizeCommand(cmd)
	if len(tokens) == 0 {
		return ""
	}

	// Known multi-word commands
	multiWord := map[string]bool{
		"git":     true,
		"npm":     true,
		"go":      true,
		"docker":  true,
		"cargo":   true,
		"yarn":    true,
		"pnpm":    true,
		"make":    true,
		"kubectl": true,
	}

	if len(tokens) >= 2 && multiWord[tokens[0]] {
		return tokens[0] + " " + tokens[1] + " *"
	}
	return tokens[0] + " *"
}

// TokenizeCommand splits a command into tokens, handling quotes.
func TokenizeCommand(cmd string) []string {
	var tokens []string
	var current strings.Builder
	inQuote := false
	quoteChar := rune(0)

	for _, r := range cmd {
		switch {
		case r == '"' || r == '\'':
			if inQuote && r == quoteChar {
				inQuote = false
				quoteChar = 0
			} else if !inQuote {
				inQuote = true
				quoteChar = r
			} else {
				current.WriteRune(r)
			}
		case r == ' ' && !inQuote:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}
