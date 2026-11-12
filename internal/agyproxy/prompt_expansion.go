package agyproxy

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	promptMarker     = "<term_llm_prompt"
	promptClose      = "\n</term_llm_prompt>"
	userRequestOpen  = "<USER_REQUEST>\n"
	userRequestClose = "\n</USER_REQUEST>"
)

var (
	promptHeader     = regexp.MustCompile(`\A<term_llm_prompt sha256="([0-9a-f]{64})">\n`)
	promptTruncation = regexp.MustCompile(`\n<truncated ([0-9]+) bytes>\z`)
)

func promptEnvelope(hash, prompt string) string {
	return `<term_llm_prompt sha256="` + hash + `">` + "\n" + prompt + promptClose
}

// expandStagedPrompts scans every historical user turn on every generation.
// Markers inside quoted/nested text, model output, and tool slots are not sources.
// Nothing restored here is rescanned, even if the original itself quotes markers.
func expandStagedPrompts(request map[string]any, home string) (bool, error) {
	contents, _ := request["contents"].([]any)
	changed, restoredBytes := false, 0
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		if content["role"] != "user" {
			continue
		}
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			restored, expanded, err := expandStagedPromptText(text, home)
			if err != nil {
				return false, fmt.Errorf("restore staged agy prompt: %w", err)
			}
			if expanded {
				restoredBytes += len(restored)
				if restoredBytes > maxRequestBody {
					return false, errors.New("oversized restored prompt history")
				}
				part["text"] = restored
				changed = true
			}
		}
	}
	return changed, nil
}

func expandStagedPromptText(text, home string) (string, bool, error) {
	prefix, payload := "", text
	if strings.HasPrefix(payload, userRequestOpen) {
		prefix, payload = userRequestOpen, payload[len(userRequestOpen):]
	}
	if !strings.HasPrefix(payload, promptMarker) {
		return text, false, nil
	}
	match := promptHeader.FindStringSubmatch(payload)
	if match == nil {
		return "", false, errors.New("invalid staged prompt marker")
	}
	dir, err := openPromptDirectory(home, false)
	if err != nil {
		return "", false, err
	}
	defer dir.Close()
	original, err := readStagedPrompt(dir, match[1])
	if err != nil {
		return "", false, err
	}
	wire := promptEnvelope(match[1], original)
	if strings.HasPrefix(payload, wire) {
		tail := payload[len(wire):]
		if prefix == "" && tail == "" {
			return original, true, nil
		}
		if prefix != "" && validPromptMetadataTail(tail) {
			return prefix + original + tail, true, nil
		}
		// A cut may land just after the intact envelope but before the
		// USER_REQUEST closing tag; authenticate that prefix below too.
	}
	cut := promptTruncation.FindStringSubmatchIndex(payload)
	if cut == nil {
		return "", false, errors.New("staged prompt payload does not match staged file")
	}
	omitted, err := strconv.ParseUint(payload[cut[2]:cut[3]], 10, 64)
	if err != nil || omitted == 0 {
		return "", false, errors.New("invalid staged prompt truncation count")
	}
	expected := wire
	if prefix != "" {
		expected += userRequestClose
	}
	if !promptPrefixMatches(expected, payload[:cut[0]]) {
		return "", false, errors.New("truncated staged prompt prefix does not match staged file")
	}
	if prefix != "" {
		return prefix + original + userRequestClose, true, nil
	}
	return original, true, nil
}

// validPromptMetadataTail permits only agy's known metadata blocks after the
// closing USER_REQUEST, never arbitrary appended user prose.
func validPromptMetadataTail(tail string) bool {
	if !strings.HasPrefix(tail, userRequestClose) {
		return false
	}
	tail = tail[len(userRequestClose):]
	for tail != "" {
		var tag string
		switch {
		case strings.HasPrefix(tail, "\n<ADDITIONAL_METADATA>\n"):
			tag = "ADDITIONAL_METADATA"
		case strings.HasPrefix(tail, "\n<USER_SETTINGS_CHANGE>\n"):
			tag = "USER_SETTINGS_CHANGE"
		default:
			return false
		}
		close := "\n</" + tag + ">"
		end := strings.Index(tail, close)
		if end < 0 {
			return false
		}
		tail = tail[end+len(close):]
	}
	return true
}

// JSON replaces incomplete UTF-8 at byte-prefix cuts with U+FFFD. Accept that
// lossy boundary only when all preceding bytes match and the replacement can
// represent a strict partial encoding of the next known rune. Never normalize
// interior characters or tolerate any other change to the captured prefix.
func promptPrefixMatches(expected, captured string) bool {
	if len(captured) < len(expected) && strings.HasPrefix(expected, captured) {
		return true
	}
	base := captured
	for replacements := 1; replacements <= 3; replacements++ {
		if !strings.HasSuffix(base, "\ufffd") {
			break
		}
		base = strings.TrimSuffix(base, "\ufffd")
		if !strings.HasPrefix(expected, base) || len(base) >= len(expected) {
			continue
		}
		_, size := utf8.DecodeRuneInString(expected[len(base):])
		// Encoders replace either the entire incomplete sequence once or
		// each invalid byte. Keep literal U+FFFD in the base when possible.
		if size > replacements {
			return true
		}
	}
	return false
}
