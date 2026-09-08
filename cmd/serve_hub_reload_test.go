package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
)

func TestHubPassiveResponseClassification(t *testing.T) {
	for _, tc := range []struct {
		name, method, contentType, id, status string
		passive                               bool
	}{
		{"GET subscription", "GET", "text/event-stream", "", "", true},
		{"POST response subscription", "POST", "text/event-stream", "resp_one", "in_progress", true},
		{"POST completed replay", "POST", "text/event-stream; charset=utf-8", "resp_one", "completed", true},
		{"POST producer", "POST", "text/event-stream", "", "", false},
		{"response id alone", "POST", "text/event-stream", "resp_one", "", false},
		{"status alone", "POST", "text/event-stream", "", "in_progress", false},
		{"ordinary response", "POST", "application/json", "resp_one", "completed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			header.Set("Content-Type", tc.contentType)
			header.Set("X-Response-ID", tc.id)
			header.Set("X-Term-LLM-Response-Status", tc.status)
			if got := hubResponseIsPassive(tc.method, header); got != tc.passive {
				t.Fatalf("passive=%v want %v", got, tc.passive)
			}
		})
	}
}

func TestHubReversePOSTObserverDoesNotDelayReload(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(map[bool]string{false: "producer", true: "response subscription"}[subscription], func(t *testing.T) {
			unblock := make(chan struct{})
			var once sync.Once
			releaseBody := func() { once.Do(func() { close(unblock) }) }
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if subscription {
					w.Header().Set("X-Response-ID", "resp_one")
					w.Header().Set("X-Term-LLM-Response-Status", "in_progress")
				}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				<-unblock
			}))
			defer backend.Close()
			defer releaseBody()
			c := &restart.Coordinator{}
			ctx, release, err := c.Activity(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			headers, finished := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(finished)
				defer release()
				handleHubReverseRequest(ctx, hubReverseRequest{ID: "one", Method: "POST", Path: "/ui/v1/responses"}, "", backend.URL, "", backend.Client(), func(frame hubReverseResponse) error {
					if frame.Type == hubReverseFrameResponseStart {
						close(headers)
					}
					return nil
				})
			}()
			<-headers
			replaced := make(chan struct{}, 1)
			stop, err := c.Bind(context.Background(), func(context.Context) error { replaced <- struct{}{}; return errors.New("fixture exec") })
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			c.Request()
			if subscription {
				select {
				case <-replaced:
				case <-time.After(time.Second):
					t.Fatal("passive POST waited for grace cancellation")
				}
				select {
				case <-finished:
					t.Fatal("body was terminated rather than detached")
				default:
				}
			} else {
				select {
				case <-replaced:
					t.Fatal("streaming producer lost ownership")
				case <-time.After(20 * time.Millisecond):
				}
			}
			releaseBody()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("forwarding did not settle")
			}
			if !subscription {
				select {
				case <-replaced:
				case <-time.After(time.Second):
					t.Fatal("settled producer still blocked reload")
				}
			}
		})
	}
}
