package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/samsaffron/term-llm/internal/notification"
)

func TestNewManualPush(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 23, 0, 123, time.FixedZone("test", 11*60*60))
	const message = "Build finished: all tests passed. 节点 😀 <&>\nSecond line."
	envelope, payload, err := newManualPush(message, "/nodes/test/ui/", now)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Version != notification.Version || envelope.Kind != notification.KindManual {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	if _, err := uuid.Parse(envelope.EventID); err != nil {
		t.Fatalf("event identity is not a UUID: %v", err)
	}
	if envelope.Title != "term-llm" || envelope.Body != message || envelope.URL != "/nodes/test/ui/" || envelope.Source != "/nodes/test/ui" {
		t.Fatalf("manual display/routing changed: %+v", envelope)
	}
	created, err := time.Parse(time.RFC3339Nano, envelope.CreatedAt)
	if err != nil || !created.Equal(now) || created.Location() != time.UTC {
		t.Fatalf("created_at = %q, error = %v", envelope.CreatedAt, err)
	}
	expires, err := time.Parse(time.RFC3339Nano, envelope.ExpiresAt)
	if err != nil || expires.Sub(created) != manualPushTTL || expires.Sub(created) > notification.MaxTTL {
		t.Fatalf("expires_at = %q, error = %v", envelope.ExpiresAt, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 9 {
		t.Fatalf("manual envelope contains unexpected ownership/read fields: %s", payload)
	}
	second, _, err := newManualPush(message, "", now)
	if err != nil || second.EventID == envelope.EventID || second.URL != "/ui/" {
		t.Fatalf("independent invocation = %+v, %v", second, err)
	}
}

func TestManualPushEncodedSizeBound(t *testing.T) {
	// Include JSON escaping and multibyte text: byte count of the raw message
	// alone must not determine whether the encrypted record fits.
	for _, text := range []string{"x", "😀", "<&>\n\""} {
		t.Run(text, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
			_, empty, err := newManualPush("", "", now)
			if err != nil {
				t.Fatal(err)
			}
			_, unit, err := newManualPush(text, "", now)
			if err != nil {
				t.Fatal(err)
			}
			units := (WebPushRecordSize - webPushEncryptionOverhead - len(empty)) / (len(unit) - len(empty))
			_, payload, err := newManualPush(strings.Repeat(text, units), "", now)
			if err != nil || len(payload)+webPushEncryptionOverhead > WebPushRecordSize {
				t.Fatalf("largest fitting payload length=%d error=%v", len(payload), err)
			}
			_, payload, err = newManualPush(strings.Repeat(text, units+1), "", now)
			if err == nil || !strings.Contains(err.Error(), "shorten the message") || payload != nil {
				t.Fatalf("overflow payload length=%d error=%v", len(payload), err)
			}
		})
	}
}
