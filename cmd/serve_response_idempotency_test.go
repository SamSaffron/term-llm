package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestResponsesConcurrentIdempotencyAdmission(t *testing.T) {
	for _, firstParty := range []bool{false, true} {
		t.Run(fmt.Sprintf("firstParty=%t", firstParty), func(t *testing.T) {
			srv, _ := newServeProjectTestServer(t)
			srv.startupDir = t.TempDir()
			provider := newStagedProvider("hello ", "world")
			release := sync.OnceFunc(func() { close(provider.releaseSecond) })
			srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
				rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), defaultModel: "mock-model"}
				rt.Touch()
				return rt, nil
			})
			defer srv.sessionMgr.Close()
			defer srv.ensureResponseRuns().Close()
			server := httptest.NewServer(http.HandlerFunc(srv.handleResponses))
			defer server.Close()
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			send := func(input string) (*http.Response, error) {
				body := fmt.Sprintf(`{"input":%q,"stream":true,"client_message_id":"same-message","no_project":%t}`, input, firstParty)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
				if err != nil {
					return nil, err
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("session_id", "concurrent-idempotency")
				req.Header.Set("Idempotency-Key", "same-operation")
				if firstParty {
					req.Header.Set("X-Term-LLM-UI-Version", "test")
				}
				return server.Client().Do(req)
			}
			type result struct {
				response *http.Response
				err      error
			}
			const callers = 8
			results := make(chan result, callers)
			start := make(chan struct{})
			for i := 0; i < callers; i++ {
				go func() { <-start; response, err := send("hello"); results <- result{response, err} }()
			}
			close(start)
			var responses []*http.Response
			// All callers must receive the same live run before the provider completes.
			// Holding the admission lock for the entire stream would deadlock here.
			var responseID string
			for i := 0; i < callers; i++ {
				result := <-results
				if result.err != nil {
					t.Fatal(result.err)
				}
				response := result.response
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK || response.Header.Get("x-response-id") == "" {
					release()
					body, _ := io.ReadAll(response.Body)
					t.Fatalf("status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
				}
				if responseID == "" {
					responseID = response.Header.Get("x-response-id")
				}
				if response.Header.Get("x-response-id") != responseID {
					t.Fatal("duplicate created a different run")
				}
				responses = append(responses, response)
			}
			conflict, err := send("different input")
			if err != nil {
				t.Fatal(err)
			}
			conflict.Body.Close()
			if conflict.StatusCode != http.StatusConflict {
				t.Fatalf("different fingerprint status=%d", conflict.StatusCode)
			}
			release()
			for _, response := range responses {
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), "event: response.completed") || strings.Contains(string(body), "event: response.failed") {
					t.Fatalf("expected successful completion: %s", body)
				}
			}
			provider.mu.Lock()
			count := len(provider.requests)
			provider.mu.Unlock()
			if count != 1 {
				t.Fatalf("provider calls=%d, want 1", count)
			}
		})
	}
}

func TestResponseIdempotencyAdmissionCancellation(t *testing.T) {
	mgr := newServeResponseRunManager()
	defer mgr.Close()
	release, err := mgr.admitIdempotency(context.Background(), "session", "key")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock, err := mgr.admitIdempotency(ctx, "session", "key")
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting admission: %v", err)
	}
	// Unrelated keys are independent, and releasing is safe on both the streaming
	// path and the handler's deferred failure cleanup.
	other, err := mgr.admitIdempotency(context.Background(), "session", "other")
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	release()
	again, err := mgr.admitIdempotency(context.Background(), "session", "key")
	if err != nil {
		t.Fatal(err)
	}
	again()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.idempotencyAdmissions) != 0 {
		t.Fatal("admission entries leaked")
	}
}
