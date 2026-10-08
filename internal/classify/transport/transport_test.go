package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewDefaultsAndValidation(t *testing.T) {
	c, err := New(Config{Provider: "p", APIKey: " k\n", DefaultBaseURL: "https://example.com/v1/", DefaultTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// The overall timeout is applied per operation through the context; an
	// http.Client timeout would cut retries short.
	if c.BaseURL() != "https://example.com/v1" || c.Timeout() != 3*time.Second || c.http.Timeout != 0 || c.apiKey != "k" {
		t.Fatalf("client = base %s timeout %s http timeout %s", c.BaseURL(), c.Timeout(), c.http.Timeout)
	}
	for _, cfg := range []Config{
		{Provider: "p", MissingKeyHint: "set X"},
		{Provider: "p", APIKey: "k", BaseURL: "ftp://x"},
		{Provider: "p", APIKey: "k", BaseURL: "https://u:p@x"},
		{Provider: "p", APIKey: "k", BaseURL: "https://x", Timeout: -1},
	} {
		if _, err := New(cfg); err == nil || !strings.HasPrefix(err.Error(), "p: ") {
			t.Fatalf("config %+v: err = %v", cfg, err)
		}
	}
}

func TestDoUsesErrorMessageAndRedacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"m":"bad Ada with k"}`))
	}))
	defer srv.Close()
	c, err := New(Config{Provider: "p", APIKey: "k", BaseURL: srv.URL, ErrorMessage: func(body []byte) string {
		return strings.TrimSuffix(strings.TrimPrefix(string(body), `{"m":"`), `"}`)
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(context.Background(), http.MethodPost, "/x", []byte(`{}`), []byte(`"Ada"`))
	if err == nil || err.Error() != "p POST /x failed: 400 Bad Request: bad [redacted] with [redacted]" {
		t.Fatalf("err = %v", err)
	}
}
