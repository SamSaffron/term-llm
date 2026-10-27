//go:build !js

package peer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// openRemoteChannel opens a channel from the requesting peer after the
// connection is up and reports when it closes.
func openRemoteChannel(t *testing.T, ep *Endpoint, label string, init *webrtc.DataChannelInit) <-chan struct{} {
	t.Helper()
	dc, err := ep.PeerConnection().CreateDataChannel(label, init)
	if err != nil {
		t.Errorf("create %q: %v", label, err)
		return nil
	}
	closed := make(chan struct{})
	var once sync.Once
	dc.OnClose(func() { once.Do(func() { close(closed) }) })
	dc.OnOpen(func() {
		// Even if it opens briefly, a request on an extra channel must never
		// reach the handler.
		_ = dc.SendText(`{"version":1,"type":"request","id":"x","method":"GET","path":"/extra"}`)
	})
	return closed
}

func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was not closed", what)
	}
}

func TestEndpointServesOnlyOneBridgeChannel(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	p := newPair(t, pairConfig{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, "primary")
	})})
	get := func() (string, error) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/primary", nil)
		resp, err := p.httpClient().Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}

	// Extra channels opened concurrently: correctly labelled reliable ones,
	// an unreliable one, and a foreign label. Server callbacks race; none
	// may win against the already-claimed primary.
	unordered := false
	inits := []struct {
		label string
		init  *webrtc.DataChannelInit
	}{
		{ChannelLabel, nil}, {ChannelLabel, nil}, {ChannelLabel, nil}, {ChannelLabel, nil},
		{ChannelLabel, &webrtc.DataChannelInit{Ordered: &unordered}},
		{"other", nil},
	}
	closedChans := make([]<-chan struct{}, len(inits))
	var wg sync.WaitGroup
	for i, c := range inits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closedChans[i] = openRemoteChannel(t, p.cliEP, c.label, c.init)
		}()
	}
	wg.Wait()
	for i, ch := range closedChans {
		if ch == nil {
			t.FailNow()
		}
		waitClosed(t, "extra channel "+inits[i].label, ch)
	}

	if body, err := get(); err != nil || body != "primary" {
		t.Fatalf("primary after extras: %q, %v", body, err)
	}

	// The slot is terminal: closing the primary does not let a new bridge
	// channel in.
	_ = p.client.Close()
	if _, err := get(); !errors.Is(err, ErrClosed) {
		t.Fatalf("request on closed primary: %v", err)
	}
	waitClosed(t, "bridge channel reopened after close", openRemoteChannel(t, p.cliEP, ChannelLabel, nil))

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/primary" {
		t.Fatalf("handler saw %v; only the primary request may reach it", paths)
	}
}

func TestServerServeChannelBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	srv, err := NewServer(ServerConfig{Handler: http.NotFoundHandler(), SessionGate: gateOpen, Authorize: allowAll, MaxChannels: 1, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	cfg := EndpointConfig{SettingEngine: loopbackSettings()}
	srvEP, err := NewEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srvEP.Close() })
	cliEP, err := NewEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cliEP.Close() })

	results := make(chan error, 8)
	srvEP.PeerConnection().OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == ChannelLabel {
			err := srv.ServeChannel(ctx, dc)
			select {
			case results <- err:
			default: // never block Pion's callback
			}
		}
	})
	if _, err := cliEP.PeerConnection().CreateDataChannel("warmup", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := cliEP.CreateOffer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := srvEP.AcceptOffer(ctx, offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := cliEP.AcceptAnswer(answer); err != nil {
		t.Fatal(err)
	}

	first, err := cliEP.NewClient(ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WaitOpen(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatalf("first channel: %v", err)
	}
	waitClosed(t, "channel over MaxChannels", openRemoteChannel(t, cliEP, ChannelLabel, nil))
	if err := <-results; !errors.Is(err, ErrTooManyChannels) {
		t.Fatalf("second channel: %v", err)
	}

	// Closing the served channel releases its slot (asynchronously, once the
	// server observes the close), so retry one channel at a time.
	_ = first.Close()
	deadline := time.After(10 * time.Second)
	for {
		_ = openRemoteChannel(t, cliEP, ChannelLabel, nil)
		select {
		case err := <-results:
			if err == nil {
				return
			}
		case <-deadline:
			t.Fatal("slot never released after the served channel closed")
		}
	}
}
