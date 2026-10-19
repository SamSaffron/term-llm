package wire

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const SteeringProtocolHeader = "X-Term-LLM-Steering-Protocol"

// Live legacy projection lasts one release after steering_v1. History decoding
// stays here as long as stored events can contain the old vocabulary.
type SteeringWriter struct {
	http.ResponseWriter
	canonical bool
}

// NewSteeringWriter selects canonical or legacy steering projection for a response.
func NewSteeringWriter(w http.ResponseWriter, canonical bool) *SteeringWriter {
	return &SteeringWriter{ResponseWriter: w, canonical: canonical}
}

func (w *SteeringWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *SteeringWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func SteeringCanonical(w io.Writer) bool {
	wire, ok := w.(*SteeringWriter)
	return !ok || wire.canonical
}

func NormalizeSteeringObject(v any, canonical bool) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			newKey := key
			pairs := [][2]string{{"interjection_id", "steering_id"}, {"interjection_status", "steering_status"}, {"pending_interjections", "pending_steering"}, {"pending_interjection", "pending_steering_text"}}
			for _, p := range pairs {
				if canonical && key == p[0] {
					newKey = p[1]
				}
				if !canonical && key == p[1] {
					newKey = p[0]
				}
			}
			// Message content, tool arguments and arbitrary user data are opaque.
			switch key {
			case "content", "parts", "arguments", "input", "output", "message", "text":
				out[newKey] = item
			default:
				out[newKey] = NormalizeSteeringObject(item, canonical)
			}
			if key == "type" || key == "event" || key == "interrupt_state" || key == "action" {
				if text, ok := item.(string); ok {
					if canonical {
						switch text {
						case "response.interjection":
							text = "response.steering"
						case "interject":
							text = "steer"
						}
					} else {
						switch text {
						case "response.steering":
							text = "response.interjection"
						case "steer":
							text = "interject"
						}
					}
					out[newKey] = text
				}
			}
		}
		if canonical {
			delete(out, "pending_steering_text")
		} else {
			delete(out, "active_rush")
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = NormalizeSteeringObject(item, canonical)
		}
		return out
	default:
		return v
	}
}
func SteeringJSON(w io.Writer, payload any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err = dec.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(NormalizeSteeringObject(value, SteeringCanonical(w)))
}
func SteeringEvent(w io.Writer, event string) string {
	if event == "response.interjection" {
		event = "response.steering"
	}
	if !SteeringCanonical(w) && event == "response.steering" {
		return "response.interjection"
	}
	return event
}
