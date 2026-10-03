package typesafe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageTransportOptInAndBearer(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer local-key" {
			t.Error("missing provider-specific bearer")
		}
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if len(req.Images) != 1 || req.Images[0].Base64 != "aW1hZ2U=" || req.Images[0].ContentType != "image/png" {
			t.Errorf("images: %#v", req.Images)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"local","answers":{"result":{"type":"noul","noul":0.75}}}`))
	}))
	defer server.Close()
	req := Request{Model: "local", State: json.RawMessage(`"inspect"`), Questions: map[string]Question{"result": {Type: "noul", Instructions: json.RawMessage(`"Is it red?"`)}}, Images: []Image{{ContentType: "image/png", Base64: "aW1hZ2U="}}}
	for _, enabled := range []bool{false, true} {
		client, err := NewClient(Options{APIKey: "local-key", BaseURL: server.URL, SupportsImages: enabled})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Classify(context.Background(), req)
		if enabled && err != nil {
			t.Fatal(err)
		}
		if !enabled && (err == nil || !strings.Contains(err.Error(), "does not support images")) {
			t.Fatalf("expected explicit unsupported error, got %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("network calls = %d", calls)
	}
	body, _ := json.Marshal(req)
	if msg := redactMessage("payload aW1hZ2U= local-key", "local-key", body); strings.Contains(msg, "aW1hZ2U=") || strings.Contains(msg, "local-key") {
		t.Fatalf("leaked image or key: %s", msg)
	}
}

func TestTextRequestOmitsImages(t *testing.T) {
	body, err := json.Marshal(Request{State: json.RawMessage(`"text"`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "images") {
		t.Fatal(string(body))
	}
}
