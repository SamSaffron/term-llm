package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
)

func TestGatewayStreamUsesSingleReaderAndIdleTimeoutTerminatesIt(t *testing.T) {
	resetGatewayCatalogProcessCacheForTest()
	t.Cleanup(resetGatewayCatalogProcessCacheForTest)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	streamCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/g1/catalog":
			_ = json.NewEncoder(w).Encode(protocol.Catalog{Version: protocol.Version, Providers: []protocol.CatalogEntry{{Key: "remote", Models: []protocol.Model{{ID: "model-a"}}}}})
		case "/g1/inference":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			event, _ := EncodeGatewayEvent(Event{Type: EventTextDelta, Text: "first"}, nil)
			record, _ := json.Marshal(protocol.StreamRecord{Version: protocol.Version, Type: "event", Event: event})
			fmt.Fprintf(w, "event: gateway\ndata: %s\n\n", record)
			flusher.Flush()
			<-r.Context().Done()
			close(streamCanceled)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Gateway: config.GatewayConfig{URL: server.URL, Token: "token", CatalogTTL: "1m", ConnectTimeout: "1s", ResponseTimeout: "1s", IdleTimeout: "25ms"}}
	provider, err := NewGatewayProvider(cfg, "remote", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), Request{Model: "model-a", Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.Text != "first" {
		t.Fatalf("first event = %+v, %v", first, err)
	}
	_, err = stream.Recv()
	if err == nil || !strings.Contains(err.Error(), "idle timeout") || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("idle error = %v", err)
	}
	select {
	case <-streamCanceled:
	case <-time.After(time.Second):
		t.Fatal("idle timeout left gateway SSE reader/request running")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}
