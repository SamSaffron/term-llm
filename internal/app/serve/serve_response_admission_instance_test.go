package serve

import (
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/app/uploads"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestResponseAdmissionInstanceCapabilityAndFreshServer(t *testing.T) {
	first, restarted := &serveServer{}, &serveServer{}
	instance := first.responseAdmissionInstance()
	if instance == "" || instance == restarted.responseAdmissionInstance() {
		t.Fatal("server incarnations must be random and independent")
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if got := first.responseAdmissionInstance(); got != instance {
				t.Errorf("concurrent instance=%q", got)
			}
		})
	}
	wg.Wait()
	w := httptest.NewRecorder()
	first.handleCapabilities(w, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	var capabilities struct {
		Admission struct {
			Version    int    `json:"version"`
			InstanceID string `json:"instance_id"`
		} `json:"response_admission"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Admission.Version != 1 || capabilities.Admission.InstanceID != instance || !strings.Contains(w.Header().Get("ETag"), instance) {
		t.Fatalf("capability = %#v, ETag=%s", capabilities, w.Header().Get("ETag"))
	}
}

type unreadAdmissionBody struct{}

func (unreadAdmissionBody) Read([]byte) (int, error) { panic("instance mismatch read request body") }

func TestResponseAdmissionInstanceMismatchBeforeParsingOrRuntime(t *testing.T) {
	srv := &serveServer{}
	for _, continuation := range []string{"", "previous-response"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", unreadAdmissionBody{})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "same-accepted-key")
		r.Header.Set(responseServerInstanceHeader, "srv_previous-incarnation")
		if continuation != "" {
			r.Header.Set(requestSessionIDHeader, continuation)
		}
		w := httptest.NewRecorder()
		srv.handleResponses(w, r)
		if w.Code != 409 || !strings.Contains(w.Body.String(), `"type":"server_instance_changed"`) || srv.responseRuns != nil {
			t.Fatalf("mismatch status=%d: %s", w.Code, w.Body.String())
		}
	}
	for _, header := range []string{"", srv.responseAdmissionInstance()} {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("not JSON"))
		r.Header.Set("Content-Type", "application/json")
		if header != "" {
			r.Header.Set(responseServerInstanceHeader, header)
		}
		w := httptest.NewRecorder()
		srv.handleResponses(w, r)
		if w.Code != 400 || strings.Contains(w.Body.String(), "server_instance_changed") {
			t.Fatalf("compatible/matching request=%d %s", w.Code, w.Body.String())
		}
	}
}

func TestUploadAndServerInstanceCORSHeaders(t *testing.T) {
	srv := uploadTestServer()
	srv.cfg.corsOrigins = []string{"http://example.com"}
	r := newUploadRequest(nil)
	r.Method = http.MethodOptions
	w := httptest.NewRecorder()
	srv.httpHandler().ServeHTTP(w, r)
	allowed := w.Header().Get("Access-Control-Allow-Headers")
	if w.Code != 204 || !strings.Contains(allowed, uploads.FilenameHeader) || !strings.Contains(allowed, responseServerInstanceHeader) {
		t.Fatalf("preflight=%d allowed=%s", w.Code, allowed)
	}
}
