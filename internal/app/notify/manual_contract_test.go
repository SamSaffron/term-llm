package notify

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/notification"
	"github.com/samsaffron/term-llm/internal/session"
	"golang.org/x/crypto/hkdf"
)

type manualPushRequest struct {
	body          []byte
	topic, ttl    string
	contentCoding string
}

// Capture and decrypt the real CLI producer's HTTP payload, not a hand-written
// envelope fixture. Every endpoint and key is disposable and test-owned.
func captureManualProducer(t *testing.T) []byte {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	requests := make(chan manualPushRequest, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		requests <- manualPushRequest{body, r.Header.Get("Topic"), r.Header.Get("TTL"), r.Header.Get("Content-Encoding")}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Serve: config.ServeConfig{BasePath: "/chat", WebPush: config.WebPushConfig{
		VAPIDPublicKey: public, VAPIDPrivateKey: private,
	}}}
	store := &targetingStore{}
	for i := range 2 {
		store.subs = append(store.subs, session.PushSubscription{
			ID: fmt.Sprint(i), Endpoint: fmt.Sprintf("%s/%d", server.URL, i),
			KeyP256DH: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			KeyAuth:   base64.RawURLEncoding.EncodeToString(auth), ClientKind: session.PushClientBrowser,
		})
	}
	deps := Deps{
		LoadConfig:       func() (*config.Config, error) { return cfg, nil },
		InitSessionStore: func(*config.Config, io.Writer) (session.Store, func()) { return store, func() {} },
	}
	const message = "Build finished: all tests passed. 节点 😀 <&>\nNext step."
	var fixture []byte
	var previousID string
	for range 2 {
		before := time.Now()
		if err := RunWeb(context.Background(), deps, WebOptions{Message: message, Stderr: io.Discard}); err != nil {
			t.Fatal(err)
		}
		if len(requests) != 2 {
			t.Fatalf("manual producer sent %d requests, want 2", len(requests))
		}
		var invocationID string
		for range 2 {
			r := <-requests
			if len(r.body) != WebPushRecordSize || binary.BigEndian.Uint32(r.body[16:20]) != WebPushRecordSize {
				t.Fatalf("encrypted record size = %d", len(r.body))
			}
			if r.contentCoding != "aes128gcm" || r.ttl != "60" {
				t.Fatalf("transport encoding=%q TTL=%q", r.contentCoding, r.ttl)
			}
			payload := decryptManualPush(t, r.body, key, auth)
			var envelope notification.Envelope
			if err := json.Unmarshal(payload, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Version != 2 || envelope.Kind != "manual" || envelope.Title != "term-llm" || envelope.Body != message {
				t.Fatalf("actual producer lost manual content: %s", payload)
			}
			created, err := time.Parse(time.RFC3339Nano, envelope.CreatedAt)
			if err != nil || created.Before(before) || created.After(time.Now()) {
				t.Fatalf("actual producer created_at=%q: %v", envelope.CreatedAt, err)
			}
			expires, err := time.Parse(time.RFC3339Nano, envelope.ExpiresAt)
			if err != nil || expires.Sub(created) != time.Minute || expires.Sub(created) > notification.MaxTTL {
				t.Fatalf("actual producer expires_at=%q: %v", envelope.ExpiresAt, err)
			}
			if envelope.EventID == "" || r.topic != notification.Topic(envelope.EventID) || len(r.topic) != 32 {
				t.Fatalf("actual producer event=%q topic=%q", envelope.EventID, r.topic)
			}
			if invocationID == "" {
				invocationID = envelope.EventID
			} else if invocationID != envelope.EventID {
				t.Fatal("subscription fanout changed event identity")
			}
			fixture = payload
		}
		if invocationID == previousID {
			t.Fatal("independent manual invocations shared one event")
		}
		previousID = invocationID
	}
	// Long messages must report an actionable failure, preserve subscriptions,
	// and never issue even a partially delivered/truncated notification.
	if err := RunWeb(context.Background(), deps, WebOptions{Message: strings.Repeat("😀<&>", WebPushRecordSize), Stderr: io.Discard}); err == nil || !strings.Contains(err.Error(), "shorten the message") {
		t.Fatalf("oversized message error = %v", err)
	}
	if len(requests) != 0 || len(store.subs) != 2 {
		t.Fatal("oversized message sent a request or changed subscriptions")
	}
	return fixture
}

func TestManualPushProducerContract(t *testing.T) {
	captureManualProducer(t)
}

func TestManualPushProducerToWorkerContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the actual service-worker formatter contract")
	}
	payload := captureManualProducer(t)
	fixture := filepath.Join(t.TempDir(), "manual-push.json")
	if err := os.WriteFile(fixture, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join("..", "..", "serveui", "static", "sw.js")
	// Keep the fixture on disk so the VM consumes the exact decrypted producer
	// bytes through the real push event handler and formatter.
	const script = `
const fs = require('node:fs');
const vm = require('node:vm');
const payload = JSON.parse(fs.readFileSync(process.argv[1], 'utf8'));
const scope = 'https://example.test/chat/';
const listeners = new Map();
const stored = new Map();
let shown;
const cache = {
  async match(key) { return stored.get(String(key))?.clone(); },
  async put(key, response) { stored.set(String(key), response.clone()); },
  async delete(key) { return stored.delete(String(key)); },
  async keys() { return [...stored.keys()].map(url => ({ url })); },
};
const context = vm.createContext({ URL, Request, Response, console,
  caches: { async open() { return cache; } },
  self: {
    location: new URL(scope),
    registration: {
      scope,
      async showNotification(title, options) { shown = { title, options }; },
      async getNotifications() { return []; },
    },
    clients: { async matchAll() { return []; } },
    addEventListener(type, listener) { listeners.set(type, listener); },
  },
});
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), context);
(async () => {
  const pending = [];
  listeners.get('push')({
    data: { json: () => payload, text: () => JSON.stringify(payload) },
    waitUntil(promise) { pending.push(promise); },
  });
  await Promise.all(pending);
  if (!shown || shown.title !== payload.title || shown.options.body !== payload.body) {
    throw new Error('actual producer content was discarded: ' + JSON.stringify(shown));
  }
  if (!shown.options.tag.includes(payload.event_id) || shown.options.tag.includes('malformed')) {
    throw new Error('manual event identity was discarded: ' + JSON.stringify(shown));
  }
  if (shown.options.data.url !== scope) {
    throw new Error('manual URL left its scope: ' + JSON.stringify(shown));
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	if output, err := exec.Command(node, "-e", script, fixture, worker).CombinedOutput(); err != nil {
		t.Fatalf("actual manual producer → service worker contract: %v\n%s", err, output)
	}
}

// decryptManualPush implements the receiver side of RFC8291 independently from
// the producer to expose the exact JSON sent over the test-owned HTTP endpoint.
func decryptManualPush(t *testing.T, record []byte, receiver *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	if len(record) < webPushEncryptionOverhead || record[20] != 65 {
		t.Fatal("invalid aes128gcm record")
	}
	sender, err := ecdh.P256().NewPublicKey(record[21:86])
	if err != nil {
		t.Fatal(err)
	}
	shared, err := receiver.ECDH(sender)
	if err != nil {
		t.Fatal(err)
	}
	derive := func(secret, salt, info []byte, size int) []byte {
		result := make([]byte, size)
		if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, info), result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	info := append([]byte("WebPush: info\x00"), receiver.PublicKey().Bytes()...)
	info = append(info, sender.Bytes()...)
	ikm := derive(shared, auth, info, 32)
	cek := derive(ikm, record[:16], []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := derive(ikm, record[:16], []byte("Content-Encoding: nonce\x00"), 12)
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, record[86:], nil)
	if err != nil {
		t.Fatal(err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		t.Fatal("invalid aes128gcm padding delimiter")
	}
	return plain[:len(plain)-1]
}
