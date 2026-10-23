package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/samsaffron/term-llm/internal/notification"
)

const manualPushTTL = time.Minute

// webPushEncryptionOverhead is the aes128gcm header, authentication tag and
// padding delimiter used by webpush-go's single-record encoding.
const webPushEncryptionOverhead = 103

// newManualPush creates one event before subscription fanout. Oversized manual
// messages fail locally rather than silently dropping user-authored text or
// increasing the record size beyond the relay's supported limit.
func newManualPush(message, basePath string, now time.Time) (notification.Envelope, []byte, error) {
	base := strings.TrimSuffix(strings.TrimSpace(basePath), "/")
	if base == "" {
		base = "/ui"
	}
	created := now.UTC()
	envelope := notification.Envelope{
		Version:   notification.Version,
		Kind:      notification.KindManual,
		EventID:   uuid.NewString(),
		Title:     "term-llm",
		Body:      message,
		CreatedAt: created.Format(time.RFC3339Nano),
		ExpiresAt: created.Add(manualPushTTL).Format(time.RFC3339Nano),
		URL:       base + "/",
		Source:    base,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return envelope, nil, fmt.Errorf("encode manual notification: %w", err)
	}
	if len(payload)+webPushEncryptionOverhead > WebPushRecordSize {
		return envelope, nil, fmt.Errorf("manual notification too large for a %d-byte Web Push record; shorten the message (encoded payload is %d bytes, maximum %d)", WebPushRecordSize, len(payload), WebPushRecordSize-webPushEncryptionOverhead)
	}
	return envelope, payload, nil
}
