package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
)

func TestHubReverseRequestAllowsEncodedSlashInQuery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/v1/sessions/s1/file-changes/diff" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("path"); got != "var/www/discourse/Gemfile.lock" {
			t.Fatalf("query path = %q", got)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	var frames []ReverseResponse
	HandleHubReverseRequest(
		context.Background(),
		ReverseRequest{
			ID:     "req_1",
			Method: http.MethodGet,
			Path:   "/chat/v1/sessions/s1/file-changes/diff?path=var%2Fwww%2Fdiscourse%2FGemfile.lock",
		},
		"node-token",
		backend.URL,
		"/chat",
		backend.Client(),
		func(resp ReverseResponse) error {
			frames = append(frames, resp)
			return nil
		},
	)

	if len(frames) == 0 {
		t.Fatal("no reverse response frames written")
	}
	if frames[0].Error != "" {
		t.Fatalf("unexpected reverse error: %s", frames[0].Error)
	}
	if frames[0].Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", frames[0].Status)
	}
}

// A reverse-connected node must not relay traversal, encoded separators or
// requests outside the configured node base path to its local HTTP listener.
func TestHubReverseRequestRejectsInvalidPaths(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backendCalls++ }))
	defer backend.Close()
	cases := []struct {
		path   string
		status int
	}{
		{"/chat/../admin", http.StatusBadRequest},
		{"/chat/%2fadmin", http.StatusBadRequest},
		{"/chat/%5cadmin", http.StatusBadRequest},
		{"/elsewhere", http.StatusForbidden},
		{"/chatty", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var frames []ReverseResponse
			HandleHubReverseRequest(context.Background(), ReverseRequest{ID: "req", Method: http.MethodGet, Path: tc.path}, "secret", backend.URL, "/chat", backend.Client(), func(frame ReverseResponse) error {
				frames = append(frames, frame)
				return nil
			})
			if len(frames) != 1 || frames[0].Status != tc.status || backendCalls != 0 {
				t.Fatalf("frames=%+v backend calls=%d; want status %d and no backend call", frames, backendCalls, tc.status)
			}
		})
	}
}

func TestHubReverseRequestReloadTerminalFrame(t *testing.T) {
	for _, afterHeaders := range []bool{false, true} {
		name := "before_headers"
		if afterHeaders {
			name = "after_headers"
		}
		t.Run(name, func(t *testing.T) {
			c := &restart.Coordinator{InterruptAfter: time.Millisecond}
			ctx, release, err := (&restart.HTTPTransport{Coordinator: c}).AdmitRequest(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			replaced := make(chan struct{}, 1)
			stop, err := c.Bind(t.Context(), func(context.Context) error {
				replaced <- struct{}{}
				return errors.New("fixture exec")
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			interrupt := func() error {
				c.Request()
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
					t.Fatal("reload did not interrupt local HTTP request")
				}
				if !errors.Is(context.Cause(ctx), restart.ErrInterrupt) {
					t.Fatalf("cancellation cause = %v", context.Cause(ctx))
				}
				return ctx.Err()
			}
			client := &http.Client{Transport: reverseRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if !afterHeaders {
					return nil, interrupt()
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(reverseReadFunc(func([]byte) (int, error) { return 0, interrupt() })),
					Request:    req,
				}, nil
			})}
			var frames []ReverseResponse
			func() {
				defer release() // Match the connector's ownership through the final write.
				HandleHubReverseRequest(ctx, ReverseRequest{ID: "req", Method: http.MethodPost, Path: "/chat"}, "", "http://node", "/chat", client, func(frame ReverseResponse) error {
					frames = append(frames, frame)
					if frame.Error != "" {
						// A child cannot be admitted from a prematurely released owner.
						_, finish, err := restart.Child(ctx)
						if err != nil {
							t.Fatalf("ownership released during terminal write: %v", err)
						}
						finish()
						select {
						case <-replaced:
							t.Fatal("replacement started during terminal write")
						default:
						}
					}
					return nil
				})
			}()
			wantCount, wantType, wantStatus := 1, ReverseFrameResponseStart, http.StatusServiceUnavailable
			if afterHeaders {
				wantCount, wantType, wantStatus = 2, ReverseFrameResponseEnd, 0
				if len(frames) == 0 || frames[0].Type != ReverseFrameResponseStart || frames[0].Status != http.StatusOK || frames[0].Error != "" {
					t.Fatalf("missing successful headers: %+v", frames)
				}
			}
			if len(frames) != wantCount {
				t.Fatalf("frames = %+v; want %d frames ending in %s error", frames, wantCount, wantType)
			}
			if last := frames[len(frames)-1]; last.Type != wantType || last.ID != "req" || last.Status != wantStatus || last.Error == "" {
				t.Fatalf("terminal frame = %+v; want %s status %d with error", last, wantType, wantStatus)
			}
			select {
			case <-replaced:
			case <-time.After(time.Second):
				t.Fatal("completed terminal write prevented replacement")
			}
		})
	}
}

type reverseRoundTripFunc func(*http.Request) (*http.Response, error)

func (f reverseRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type reverseReadFunc func([]byte) (int, error)

func (f reverseReadFunc) Read(p []byte) (int, error) { return f(p) }
