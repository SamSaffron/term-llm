package wire

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

func Int64Value(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
	}
	return fallback
}

func IntValue(value any, fallback int) int {
	return int(Int64Value(value, int64(fallback)))
}

func EncodeTextDeltaPayloadWithIdentity(responseID string, runEpoch int64, outputIndex, segmentOrdinal int, segmentStartSequence int64, delta string, sequenceNumber int64) ([]byte, error) {
	data := make([]byte, 0, 160+len(responseID)+len(delta))
	data = append(data, `{"response_id":`...)
	data = AppendJSONString(data, responseID)
	data = append(data, `,"run_epoch":`...)
	data = strconv.AppendInt(data, runEpoch, 10)
	data = append(data, `,"assistant_segment_ordinal":`...)
	data = strconv.AppendInt(data, int64(segmentOrdinal), 10)
	data = append(data, `,"segment_start_sequence":`...)
	data = strconv.AppendInt(data, segmentStartSequence, 10)
	data = append(data, `,"output_index":`...)
	data = strconv.AppendInt(data, int64(outputIndex), 10)
	data = append(data, `,"delta":`...)
	if utf8.ValidString(delta) {
		data = AppendJSONString(data, delta)
	} else {
		encoded, err := json.Marshal(delta)
		if err != nil {
			return nil, err
		}
		data = append(data, encoded...)
	}
	data = append(data, `,"sequence_number":`...)
	data = strconv.AppendInt(data, sequenceNumber, 10)
	data = append(data, '}')
	return data, nil
}

// AppendJSONString appends a JSON-encoded string to dst without allocating a
// separate []byte (unlike json.Marshal). Handles all characters that require
// escaping in JSON strings; non-ASCII UTF-8 bytes pass through unchanged.
func AppendJSONString(dst []byte, s string) []byte {
	const hexChars = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x20 && b != '"' && b != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		start = i + 1
		switch b {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexChars[b>>4], hexChars[b&0xf])
		}
	}
	dst = append(dst, s[start:]...)
	dst = append(dst, '"')
	return dst
}

func CloneJSONMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(src))
	for key, value := range src {
		cloned[key] = CloneJSONValue(value)
	}
	return cloned
}

func CloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return CloneJSONMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = CloneJSONValue(typed[i])
		}
		return out
	default:
		return typed
	}
}
