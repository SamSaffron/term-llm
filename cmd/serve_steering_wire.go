package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const steeringProtocolHeader = "X-Term-LLM-Steering-Protocol"

// Live legacy projection lasts one release after steering_v1. History decoding
// stays here as long as stored events can contain the old vocabulary.
type steeringWireWriter struct {
	http.ResponseWriter
	canonical bool
}

func (w *steeringWireWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *steeringWireWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func steeringWireCanonical(w io.Writer) bool {
	wire, ok := w.(*steeringWireWriter)
	return !ok || wire.canonical
}

func normalizeSteeringObject(v any, canonical bool) any {
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
				out[newKey] = normalizeSteeringObject(item, canonical)
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
			out[i] = normalizeSteeringObject(item, canonical)
		}
		return out
	default:
		return v
	}
}
func steeringWireJSON(w io.Writer, payload any) ([]byte, error) {
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
	return json.Marshal(normalizeSteeringObject(value, steeringWireCanonical(w)))
}
func steeringWireEvent(w io.Writer, event string) string {
	if event == "response.interjection" {
		event = "response.steering"
	}
	if !steeringWireCanonical(w) && event == "response.steering" {
		return "response.interjection"
	}
	return event
}

// Decode old IDs at ingress only; never maintain parallel internal fields.
func (req *sessionInterruptRequest) UnmarshalJSON(data []byte) error {
	type canonical sessionInterruptRequest
	var wire struct {
		canonical
		LegacyID string `json:"interjection_id"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*req = sessionInterruptRequest(wire.canonical)
	if wire.LegacyID != "" && req.SteeringID != "" && wire.LegacyID != req.SteeringID {
		return fmt.Errorf("contradictory steering IDs")
	}
	if req.SteeringID == "" {
		req.SteeringID = wire.LegacyID
	}
	if req.ClientMessageID != "" && req.SteeringID != "" && req.ClientMessageID != req.SteeringID {
		return fmt.Errorf("contradictory client message IDs")
	}
	return nil
}
