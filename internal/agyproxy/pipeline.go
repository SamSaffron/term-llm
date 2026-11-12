package agyproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// decodeGenerationRequest retains numeric tokens verbatim and accepts exactly
// one JSON value, just like Unmarshal (Decoder.Decode alone allows trailing JSON).
func decodeGenerationRequest(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode generation request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode generation request: %w", err)
	}
	return root, nil
}

// processGenerationRequest is the production decode-expand-filter-clean-encode
// path. A zero size limit is used by the standalone filtering wrapper, whose
// callers historically supplied their own transport limits.
func processGenerationRequest(body []byte, artifactRoot string, requireMCP bool, sizeLimit int) ([]byte, bool, error) {
	return processGenerationRequestWithPrompts(body, artifactRoot, "", requireMCP, sizeLimit)
}

func processGenerationRequestWithPrompts(body []byte, artifactRoot, promptRoot string, requireMCP bool, sizeLimit int) ([]byte, bool, error) {
	if sizeLimit > 0 && len(body) > sizeLimit {
		return nil, false, errors.New("oversized generation request")
	}
	root, err := decodeGenerationRequest(body)
	if err != nil {
		return nil, false, err
	}
	request, ok := root["request"].(map[string]any)
	if !ok {
		return nil, false, errors.New("generation request missing request object")
	}
	promptsExpanded, err := expandStagedPrompts(request, strings.TrimSpace(promptRoot))
	if err != nil {
		return nil, false, err
	}
	expanded := expandAgyArtifacts(request, strings.TrimSpace(artifactRoot)) || promptsExpanded
	// Check the expanded size BEFORE filtering; removed declarations must not
	// hide an oversized expansion. Count the exact Marshal size without a
	// second serialization or an intermediate expanded request buffer.
	if expanded && sizeLimit > 0 && generationJSONSize(root) > sizeLimit {
		return nil, false, errors.New("oversized expanded generation request")
	}
	filtered, verified, err := filterGenerationObject(request, requireMCP)
	if err != nil {
		return nil, false, err
	}
	if !expanded && !filtered {
		return body, verified, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode generation request: %w", err)
	}
	if sizeLimit > 0 && len(out) > sizeLimit {
		return nil, false, errors.New("oversized forwarded generation request")
	}
	return out, verified, nil
}

// generationJSONSize handles only the JSON types produced by our UseNumber
// decoder and transformations. Its string count matches Marshal's default HTML
// escaping, including invalid UTF-8 that may occur in artifact contents.
func generationJSONSize(value any) int {
	switch value := value.(type) {
	case map[string]any:
		size := 2
		for key, child := range value {
			size += generationJSONStringSize(key) + 1 + generationJSONSize(child) + 1
		}
		if len(value) > 0 {
			size-- // no trailing comma
		}
		return size
	case []any:
		size := 2
		for _, child := range value {
			size += generationJSONSize(child) + 1
		}
		if len(value) > 0 {
			size--
		}
		return size
	case string:
		return generationJSONStringSize(value)
	case json.Number:
		return len(value)
	case bool:
		if value {
			return 4
		}
		return 5
	case nil:
		return 4
	default:
		panic(fmt.Sprintf("unexpected generation JSON type %T", value))
	}
}

func generationJSONStringSize(value string) int {
	size := 2 // quotes
	for _, r := range value {
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			size += 2
		case '<', '>', '&', '\u2028', '\u2029':
			size += 6
		default:
			if r < 0x20 {
				size += 6
			} else {
				// Range and Marshal both replace invalid UTF-8 bytes with U+FFFD.
				size += utf8.RuneLen(r)
			}
		}
	}
	return size
}
