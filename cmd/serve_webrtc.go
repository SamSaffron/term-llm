package cmd

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/samsaffron/term-llm/internal/webrtc"
)

var (
	serveWebRTC             bool
	serveWebRTCSignalingURL string
	serveWebRTCToken        string
	serveWebRTCSTUN         []string
	serveWebRTCMaxConns     int
	serveWebRTCDiagnostics  bool
)

func init() {
	serveCmd.Flags().BoolVar(&serveWebRTC, "webrtc", false,
		"Enable WebRTC direct routing")
	serveCmd.Flags().StringVar(&serveWebRTCSignalingURL, "webrtc-signaling-url", "",
		"Signaling server base URL (must be HTTPS, e.g. https://relay.example.com/webrtc)")
	serveCmd.Flags().StringVar(&serveWebRTCToken, "webrtc-token", "",
		"Bearer token for authenticating with the signaling server")
	serveCmd.Flags().StringArrayVar(&serveWebRTCSTUN, "webrtc-stun",
		[]string{"stun:stun.l.google.com:19302"}, "STUN server URL(s)")
	serveCmd.Flags().IntVar(&serveWebRTCMaxConns, "webrtc-max-conns", 10,
		"Maximum concurrent WebRTC connections")
	serveCmd.Flags().BoolVar(&serveWebRTCDiagnostics, "webrtc-diagnostics", false,
		"Enable WebRTC diagnostics: log connection timeline and per-request latency to browser console")
}

// webrtcHTMLSnippet returns the <script> snippet to inject into index.html
// when WebRTC is enabled, so the browser knows to try a direct connection.
func webrtcHTMLSnippet() string {
	if !serveWebRTC {
		return ""
	}
	urlJSON, _ := json.Marshal(serveWebRTCSignalingURL)
	snippet := `<script>window.__WEBRTC_ENABLED__=true;window.__WEBRTC_SIGNALING_URL__=` +
		string(urlJSON) + `;`
	if serveWebRTCDiagnostics {
		snippet += `window.__WEBRTC_DIAGNOSTICS__=true;`
	}
	snippet += `</script>`
	return snippet
}

// runWebRTCPeer starts the WebRTC home peer in the background.
// It returns immediately; the peer runs until ctx is cancelled.
func runWebRTCPeer(ctx context.Context, s *serveServer) {
	if !serveWebRTC {
		return
	}
	cfg := webrtc.Config{
		SignalingURL: serveWebRTCSignalingURL,
		Token:        serveWebRTCToken,
		BasePath:     s.cfg.basePath,
		STUNURLs:     append([]string(nil), serveWebRTCSTUN...),
		PollInterval: 2 * time.Second,
		IdleTimeout:  30 * time.Minute,
		MaxConns:     serveWebRTCMaxConns,
	}
	peer, err := webrtc.New(ctx, cfg, s.httpHandler())
	if err != nil {
		log.Printf("webrtc: failed to start peer: %v", err)
		return
	}
	// peer.Close() cancels the internal context; the outer ctx cancellation
	// propagates automatically, so explicit cleanup is not required.
	_ = peer
}
