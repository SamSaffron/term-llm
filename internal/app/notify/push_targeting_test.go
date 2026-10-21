package notify

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

type targetingStore struct {
	session.Store
	subs []session.PushSubscription
	err  error
}

func (s *targetingStore) ListPushSubscriptions(context.Context) ([]session.PushSubscription, error) {
	return s.subs, s.err
}

func (s *targetingStore) DeletePushSubscription(_ context.Context, endpoint string) error {
	s.subs = slices.DeleteFunc(s.subs, func(sub session.PushSubscription) bool {
		return sub.Endpoint == endpoint
	})
	return nil
}

// All kinds deliberately share the same endpoint host: routing must use metadata.
func targetingFixture(t *testing.T, kinds []string, status int) (Deps, *config.Config, chan string) {
	t.Helper()
	requests := make(chan string, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	store := &targetingStore{}
	for i, kind := range kinds {
		store.subs = append(store.subs, session.PushSubscription{
			ID:         fmt.Sprint(i),
			Endpoint:   fmt.Sprintf("%s/%d", server.URL, i),
			KeyP256DH:  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			KeyAuth:    base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
			ClientKind: kind,
			Status:     "active",
		})
	}
	cfg := &config.Config{Serve: config.ServeConfig{WebPush: config.WebPushConfig{
		VAPIDPublicKey: public, VAPIDPrivateKey: private,
	}}}
	deps := Deps{
		LoadConfig: func() (*config.Config, error) { return cfg, nil },
		InitSessionStore: func(*config.Config, io.Writer) (session.Store, func()) {
			return store, func() {}
		},
	}
	return deps, cfg, requests
}

func TestPushDeliveryTargeting(t *testing.T) {
	for _, tt := range []struct {
		name string
		want []string
	}{
		{"all", []string{"/0", "/1", "/2"}},
		{"broadcast", []string{"/0", "/1", "/2"}},
		{"app", []string{"/1"}},
		{"web", []string{"/0", "/2"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deps, cfg, requests := targetingFixture(t, []string{session.PushClientBrowser, session.PushClientApp, ""}, http.StatusCreated)
			var output bytes.Buffer
			var err error
			switch tt.name {
			case "all":
				n, errs := SendWebPushAll(context.Background(), deps, cfg, "finished", &output)
				if n != len(tt.want) || len(errs) != 0 {
					t.Fatalf("SendWebPushAll = %d, %v", n, errs)
				}
			case "broadcast":
				err = RunBroadcast(context.Background(), deps, BroadcastOptions{Message: "finished", Stderr: &output})
			case "app":
				err = RunApp(context.Background(), deps, AppOptions{Message: "finished", Stderr: &output})
			case "web":
				err = RunWeb(context.Background(), deps, WebOptions{Message: "finished", Stderr: &output})
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for len(requests) > 0 {
				got = append(got, <-requests)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("delivered endpoints = %v, want %v (output: %s)", got, tt.want, &output)
			}
			if tt.name == "app" || tt.name == "web" {
				if want := fmt.Sprintf("sent to %d subscription(s)\n", len(tt.want)); output.String() != want {
					t.Fatalf("output = %q, want %q", &output, want)
				}
			}
		})
	}
}

func runTargeted(ctx context.Context, deps Deps, kind string, output io.Writer) error {
	if kind == session.PushClientApp {
		return RunApp(ctx, deps, AppOptions{Message: "finished", Stderr: output})
	}
	return RunWeb(ctx, deps, WebOptions{Message: "finished", Stderr: output})
}

func TestTargetedPushNoMatches(t *testing.T) {
	for _, tt := range []struct {
		name, target string
		kinds        []string
	}{
		{"app excludes browser and legacy", session.PushClientApp, []string{session.PushClientBrowser, ""}},
		{"web excludes app", session.PushClientBrowser, []string{session.PushClientApp}},
		{"app empty store", session.PushClientApp, nil},
		{"web empty store", session.PushClientBrowser, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deps, _, requests := targetingFixture(t, tt.kinds, http.StatusCreated)
			var output bytes.Buffer
			if err := runTargeted(context.Background(), deps, tt.target, &output); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 0 || output.String() != "no push subscriptions found\n" {
				t.Fatalf("requests = %d, output = %q", len(requests), &output)
			}
		})
	}
}

func TestTargetedPushErrors(t *testing.T) {
	for _, kind := range []string{session.PushClientApp, session.PushClientBrowser} {
		t.Run(kind, func(t *testing.T) {
			t.Run("config", func(t *testing.T) {
				want := errors.New("config unavailable")
				deps := Deps{LoadConfig: func() (*config.Config, error) { return nil, want }}
				if err := runTargeted(context.Background(), deps, kind, io.Discard); !errors.Is(err, want) {
					t.Fatalf("error = %v, want %v", err, want)
				}
			})
			t.Run("missing keys", func(t *testing.T) {
				deps := Deps{LoadConfig: func() (*config.Config, error) { return &config.Config{}, nil }}
				if err := runTargeted(context.Background(), deps, kind, io.Discard); err == nil || !strings.Contains(err.Error(), "VAPID keys not configured") {
					t.Fatalf("error = %v, want missing VAPID keys", err)
				}
			})
			for _, failure := range []string{"delivery", "store", "list"} {
				t.Run(failure, func(t *testing.T) {
					deps, _, requests := targetingFixture(t, []string{kind}, http.StatusInternalServerError)
					want := "push to subscription 0:"
					if failure != "delivery" {
						deps.InitSessionStore = func(*config.Config, io.Writer) (session.Store, func()) {
							if failure == "store" {
								return nil, func() {}
							}
							return &targetingStore{err: errors.New("read failed")}, func() {}
						}
						want = "session store not available"
						if failure == "list" {
							want = "list subscriptions: read failed"
						}
					}
					var output bytes.Buffer
					// Delivery/store failures must reach Cobra so automation exits nonzero.
					if err := runTargeted(context.Background(), deps, kind, &output); err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("error = %v, want %q", err, want)
					}
					if !strings.Contains(output.String(), "error: "+want) {
						t.Fatalf("output = %q, want %q", &output, want)
					}
					if failure != "delivery" && len(requests) != 0 {
						t.Fatalf("unexpected delivery attempts: %d", len(requests))
					}
				})
			}
		})
	}
}

func TestTargetedPushExpiredEndpoint(t *testing.T) {
	for _, kind := range []string{session.PushClientApp, session.PushClientBrowser} {
		for _, status := range []int{http.StatusNotFound, http.StatusGone} {
			t.Run(fmt.Sprintf("%s/%d", kind, status), func(t *testing.T) {
				deps, _, requests := targetingFixture(t, []string{kind}, status)
				var output bytes.Buffer
				err := runTargeted(context.Background(), deps, kind, &output)
				if err == nil || !strings.Contains(err.Error(), "endpoint expired") || !strings.Contains(err.Error(), "re-enroll") {
					t.Fatalf("error = %v, want expired enrollment error", err)
				}
				if strings.Contains(output.String(), "no push subscriptions found") || len(requests) != 1 {
					t.Fatalf("requests=%d output=%q", len(requests), &output)
				}
				output.Reset()
				if err := runTargeted(context.Background(), deps, kind, &output); err != nil {
					t.Fatal(err)
				}
				if output.String() != "no push subscriptions found\n" || len(requests) != 1 {
					t.Fatalf("expired subscription was retried: requests=%d output=%q", len(requests), &output)
				}
			})
		}
	}
}
