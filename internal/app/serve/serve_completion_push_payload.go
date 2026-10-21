package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/session"
)

// webpush-go's single-record encoding uses an 86-byte header, a 16-byte
// authentication tag and a one-byte padding delimiter in addition to the JSON.
const completionPushEncryptionOverhead = 103

// sendCompletionPush retries size failures once with minimal display text. Keep
// the event identity and route intact, including for oversized records already
// persisted in the outbox.
func sendCompletionPush(ctx context.Context, sub *session.PushSubscription, payload []byte, opts *webpush.Options, send func(context.Context, *session.PushSubscription, []byte, *webpush.Options) (int, time.Duration, error)) (int, time.Duration, error) {
	status, retryAfter, err := send(ctx, sub, payload, opts)
	if err == nil || (!errors.Is(err, webpush.ErrMaxPadExceeded) && status != http.StatusRequestEntityTooLarge) {
		return status, retryAfter, err
	}
	var compact completionPushPayload
	if json.Unmarshal(payload, &compact) != nil {
		return status, retryAfter, err
	}
	compact.Title = "Response complete"
	compact.Body = "Your term-llm response is ready."
	if compact.Outcome == "failed" {
		compact.Title = "Response failed"
		compact.Body = "Your term-llm response stopped with an error."
	}
	fallback, marshalErr := json.Marshal(compact)
	if marshalErr != nil {
		return status, retryAfter, err
	}
	maxRecordSize := opts.RecordSize
	if maxRecordSize == 0 {
		maxRecordSize = webpush.MaxRecordSize
	}
	if len(fallback)+completionPushEncryptionOverhead > int(maxRecordSize) {
		compact.Body = ""
		fallback, _ = json.Marshal(compact)
	}
	if len(fallback)+completionPushEncryptionOverhead > int(maxRecordSize) {
		return status, retryAfter, err
	}
	// Merely shortening JSON does not shrink the padded ciphertext. Use the
	// smallest record that fits the fallback so a relay's HTTP 413 can recover.
	fallbackOpts := *opts
	fallbackOpts.RecordSize = uint32(len(fallback) + completionPushEncryptionOverhead)
	if fallbackOpts.RecordSize >= maxRecordSize && len(fallback) >= len(payload) {
		return status, retryAfter, err
	}
	return send(ctx, sub, fallback, &fallbackOpts)
}
