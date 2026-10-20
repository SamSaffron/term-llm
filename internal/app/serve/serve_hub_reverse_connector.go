package serve

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
	"github.com/samsaffron/term-llm/internal/restart"
)

func localHubConnectBase(host string, port int) string {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s", netJoinHostPortForURL(host, port))
}

func newHubReverseLocalClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// No environment proxy: reverse requests carry the node's local serve
			// token back to its own loopback listener.
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		CheckRedirect: hubDoNotFollowRedirects,
	}
}

func netJoinHostPortForURL(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

var hubReverseReconnectDelay = 2 * time.Second

func runHubReverseConnector(ctx context.Context, hubURL, nodeID, token, localBase, allowedBasePath string, client *http.Client) {
	if client == nil {
		client = newHubReverseLocalClient()
	}
	for ctx.Err() == nil {
		if err := hubReverseConnectOnce(ctx, hubURL, nodeID, token, localBase, allowedBasePath, client); err != nil && ctx.Err() == nil {
			log.Printf("hub reverse connect: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(hubReverseReconnectDelay):
		}
	}
}

func hubReverseConnectOnce(ctx context.Context, hubURL, nodeID, token, localBase, allowedBasePath string, client *http.Client) error {
	u, err := url.Parse(strings.TrimRight(hubURL, "/") + "/api/connect")
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return fmt.Errorf("unsupported hub url scheme %q", u.Scheme)
	}
	q := u.Query()
	q.Set("node_id", nodeID)
	u.RawQuery = q.Encode()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set(hubNodeIDHeader, nodeID)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), header)
	if err != nil {
		return err
	}
	defer conn.Close()
	var writeMu sync.Mutex
	donePing := make(chan struct{})
	defer close(donePing)
	if err := hubReverseSetHeartbeat(conn, nil); err != nil {
		return err
	}
	go hubReversePingLoop(conn, &writeMu, donePing)
	log.Printf("hub reverse connect: node %q connected to %s", nodeID, hubURL)
	activeMu := sync.Mutex{}
	active := map[string]context.CancelFunc{}
	defer func() {
		activeMu.Lock()
		defer activeMu.Unlock()
		for _, cancel := range active {
			cancel()
		}
	}()
	for {
		var req hubReverseRequest
		if err := conn.ReadJSON(&req); err != nil {
			return err
		}
		if req.Type == hubReverseFrameCancel {
			activeMu.Lock()
			cancel := active[req.ID]
			delete(active, req.ID)
			activeMu.Unlock()
			if cancel != nil {
				cancel()
			}
			continue
		}
		reqCtx, release, reloadErr := restart.Default.Activity(ctx)
		if reloadErr != nil {
			writeMu.Lock()
			_ = conn.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: req.ID, Status: 503, Error: reloadErr.Error()})
			writeMu.Unlock()
			continue
		}
		reqCtx, cancel := context.WithCancel(reqCtx)
		activeMu.Lock()
		active[req.ID] = cancel
		activeMu.Unlock()
		go func(req hubReverseRequest, reqCtx context.Context, cancel context.CancelFunc) {
			defer release()
			defer func() {
				activeMu.Lock()
				delete(active, req.ID)
				activeMu.Unlock()
				cancel()
			}()
			handleHubReverseRequest(reqCtx, req, token, localBase, allowedBasePath, client, func(resp hubReverseResponse) error {
				writeMu.Lock()
				defer writeMu.Unlock()
				_ = conn.SetWriteDeadline(time.Now().Add(hubReverseWriteWait))
				err := conn.WriteJSON(resp)
				_ = conn.SetWriteDeadline(time.Time{})
				return err
			})
		}(req, reqCtx, cancel)
	}
}

func handleHubReverseRequest(ctx context.Context, frame hubReverseRequest, token, localBase, allowedBasePath string, client *http.Client, writeFrame func(hubReverseResponse) error) {
	appserver.HandleHubReverseRequest(ctx, frame, token, localBase, allowedBasePath, client, writeFrame)
}
