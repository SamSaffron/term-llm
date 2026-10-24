package serve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/app/uploads"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

// Real reverse WebSocket Hub -> authenticated node routes -> engine -> scripted
// mock provider. Every original is a complete, decoded noisy PNG, not padding.
func TestSixLargeUploadRefsThroughReverseHub(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("six originals received")
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: engine.NewEngine(provider, nil), defaultModel: "mock-model"}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()
	srv := uploadTestServer()
	srv.sessionMgr = manager
	srv.responseRuns = newServeResponseRunManager()
	defer srv.responseRuns.Close()
	var responseCalls, uploadCalls atomic.Int32
	handler := srv.httpHandler()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/v1/responses" {
			responseCalls.Add(1)
		}
		if r.URL.Path == "/chat/uploads" && r.Method == http.MethodPost {
			uploadCalls.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer backend.Close()
	node := hub.Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat", Token: srv.cfg.token}
	hubServer := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	hubTS := httptest.NewServer(hubServer.handler())
	defer hubTS.Close()
	// A nonempty Origin without Fetch Metadata exercises configured public Hub
	// origin validation even though the connector replaces backend Host.
	srv.cfg.hubURL = hubTS.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, hubTS.URL, node.ID, node.Token, backend.URL, node.BasePath, backend.Client())
	waitForReverseNode(t, hubServer, node.ID)
	base := hubTS.URL + "/node/artist"
	var originals [][]byte
	var inline, refs []map[string]any
	var paths []string
	for i := range 6 {
		raw := noisyUploadPNG(t, i+1)
		originals = append(originals, raw)
		filename := fmt.Sprintf("original-%d.png", i+1)
		inline = append(inline, map[string]any{"type": "input_image", "filename": filename, "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)})
		r, _ := http.NewRequest(http.MethodPost, base+"/uploads", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "image/png")
		r.Header.Set(uploads.FilenameHeader, url.PathEscape(filename))
		r.Header.Set("Origin", hubTS.URL)
		resp, err := hubTS.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 201 {
			t.Fatalf("upload %d status=%d: %s", i+1, resp.StatusCode, reply)
		}
		var ref uploads.Reference
		if err := json.Unmarshal(reply, &ref); err != nil {
			t.Fatal(err)
		}
		if ref.SizeBytes != int64(len(raw)) || ref.Filename != filename {
			t.Fatalf("ref = %#v", ref)
		}
		refs = append(refs, map[string]any{"type": "input_upload", "upload_id": ref.UploadID})
		paths = append(paths, filepath.Join(uploads.Dir(), ref.UploadID))
		t.Logf("original=%s bytes=%d upload_status=201", filename, len(raw))
	}
	makeBody := func(parts []map[string]any) []byte {
		body, err := json.Marshal(map[string]any{"stream": true, "input": []any{map[string]any{"type": "message", "role": "user", "content": parts}}})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	inlineBody := makeBody(inline)
	if len(inlineBody) <= hubReverseMaxRequestBodyBytes {
		t.Fatal("inline control is below transport limit")
	}
	send := func(body []byte, instance string) (int, http.Header, []byte) {
		r, _ := http.NewRequest(http.MethodPost, base+"/v1/responses", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", hubTS.URL)
		r.Header.Set("Idempotency-Key", "six-large-upload-refs")
		r.Header.Set(requestSessionIDHeader, "six-large-uploads")
		if instance != "" {
			r.Header.Set(responseServerInstanceHeader, instance)
		}
		resp, err := hubTS.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, reply
	}
	status, _, reply := send(inlineBody, "")
	if status != 413 || responseCalls.Load() != 0 || len(provider.RecordedRequests()) != 0 {
		t.Fatalf("legacy control=%d backend=%d provider=%d: %s", status, responseCalls.Load(), len(provider.RecordedRequests()), reply)
	}
	body := makeBody(refs)
	if len(body) > 4096 {
		t.Fatalf("reference body is too large: %d", len(body))
	}
	instance := srv.responseAdmissionInstance()
	status, headers, reply := send(body, instance)
	if status != 200 {
		t.Fatalf("reference response = %d %s", status, reply)
	}
	responseID := headers.Get("X-Response-ID")
	if responseID == "" {
		t.Fatal("missing response ID")
	}
	status, headers, reply = send(body, instance)
	if status != 200 || headers.Get("X-Response-ID") != responseID {
		t.Fatalf("replay = %d %s", status, reply)
	}
	requests := provider.RecordedRequests()
	if len(requests) != 1 || srv.responseRuns.Diagnostics().IdempotencyReplays != 1 || uploadCalls.Load() != 6 {
		t.Fatalf("provider=%d replays=%d uploads=%d", len(requests), srv.responseRuns.Diagnostics().IdempotencyReplays, uploadCalls.Load())
	}
	var images []core.Part
	for _, msg := range requests[0].Messages {
		for _, p := range msg.Parts {
			if p.Type == core.PartImage {
				images = append(images, p)
			}
		}
	}
	if len(images) != 6 {
		t.Fatalf("provider image count=%d", len(images))
	}
	for i, p := range images {
		if p.ImagePath != paths[i] {
			t.Fatalf("image %d path=%s want=%s", i, p.ImagePath, paths[i])
		}
		raw, err := os.ReadFile(paths[i])
		if err != nil || !bytes.Equal(raw, originals[i]) {
			t.Fatalf("original %d changed: %v", i, err)
		}
		sent, err := base64.StdEncoding.DecodeString(p.ImageData.Base64)
		if err != nil || len(sent) >= len(raw) || len(sent) > uploads.MaxLLMImageBytes || p.ImageData.Width != 1400 || p.ImageData.Height != 1400 {
			t.Fatalf("image %d model bytes=%d original=%d metadata=%#v error=%v", i, len(sent), len(raw), p.ImageData, err)
		}
		if _, _, err := image.Decode(bytes.NewReader(sent)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(uploads.Dir())
	if err != nil || len(entries) != 6 {
		t.Fatalf("originals duplicated: %d, %v", len(entries), err)
	}
	// Even a retry of an accepted send must not reach parsing/provider on a
	// different server incarnation. The response identity remains unchanged.
	status, _, reply = send(body, "srv_different-incarnation")
	if status != 409 || !strings.Contains(string(reply), "server_instance_changed") || len(provider.RecordedRequests()) != 1 {
		t.Fatalf("instance mismatch=%d %s", status, reply)
	}
	t.Logf("inline_control=%d bytes=>413; refs=%d bytes=>200; provider_images=6; replay=200/no new originals", len(inlineBody), len(body))
}

func noisyUploadPNG(t *testing.T, seed int) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 1400, 1400))
	rng := rand.New(rand.NewSource(int64(seed)))
	for j := 0; j < len(im.Pix); j += 4 {
		im.Pix[j] = byte(rng.Intn(256))
		im.Pix[j+1] = byte(rng.Intn(256))
		im.Pix[j+2] = byte(rng.Intn(256))
		im.Pix[j+3] = 255
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil || decoded.Bounds() != im.Bounds() || len(raw) >= uploads.MaxAttachmentBytes || len(raw) <= 5<<20 {
		t.Fatalf("fixture dimensions/size/encoding: size=%d err=%v", len(raw), err)
	}
	return raw
}
