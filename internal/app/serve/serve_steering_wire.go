package serve

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
)

const steeringProtocolHeader = wire.SteeringProtocolHeader

type steeringWireWriter = wire.SteeringWriter

func steeringWireCanonical(w io.Writer) bool { return wire.SteeringCanonical(w) }
func normalizeSteeringObject(v any, canonical bool) any {
	return wire.NormalizeSteeringObject(v, canonical)
}
func steeringWireJSON(w io.Writer, payload any) ([]byte, error) { return wire.SteeringJSON(w, payload) }
func steeringWireEvent(w io.Writer, event string) string        { return wire.SteeringEvent(w, event) }

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
