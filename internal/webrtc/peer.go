package webrtc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/datachannel"
	dtls "github.com/pion/dtls/v3"
	dtlsfp "github.com/pion/dtls/v3/pkg/crypto/fingerprint"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	dtlsnet "github.com/pion/dtls/v3/pkg/net"
	"github.com/pion/ice/v4"
	pionlog "github.com/pion/logging"
	"github.com/pion/sctp"
	"github.com/pion/sdp/v3"
	"github.com/pion/stun/v3"
)

const maxFrameBytes = 10 * 1024 * 1024 // 10 MB

// signalingMsg is a message exchanged via the signaling server.
type signalingMsg struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"` // "offer" or "answer"
	SDP       string `json:"sdp"`
}

// requestFrame is a data-channel request from the browser.
type requestFrame struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"` // base64-encoded UTF-8 bytes
}

// responseFrame is a data-channel response chunk or completion.
type responseFrame struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`              // "headers", "chunk", or "done"
	Headers map[string]string `json:"headers,omitempty"` // only for "headers"
	Data    string            `json:"data,omitempty"`    // SSE line; only for "chunk"
	Status  int               `json:"status,omitempty"`  // HTTP status; for "headers" and "done"
}

// peer is the home-side WebRTC peer.
type peer struct {
	cfg     Config
	handler http.Handler
	cancel  context.CancelFunc
	active  atomic.Int32
	client  *http.Client
}

// New creates and starts a WebRTC home peer.
func New(ctx context.Context, cfg Config, handler http.Handler) (Peer, error) {
	if !strings.HasPrefix(cfg.SignalingURL, "https://") {
		return nil, fmt.Errorf("webrtc: --webrtc-signaling-url must use HTTPS (got %q)", cfg.SignalingURL)
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 10
	}
	if len(cfg.STUNURLs) == 0 {
		cfg.STUNURLs = []string{"stun:stun.l.google.com:19302"}
	}

	peerCtx, cancel := context.WithCancel(ctx)
	p := &peer{
		cfg:     cfg,
		handler: handler,
		cancel:  cancel,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
	go p.run(peerCtx)
	return p, nil
}

func (p *peer) Close() error {
	p.cancel()
	return nil
}

func (p *peer) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := p.pollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("webrtc: signaling poll error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.cfg.PollInterval):
		}
	}
}

func (p *peer) pollOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", p.cfg.SignalingURL+"/signal", nil)
	if err != nil {
		return fmt.Errorf("build poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusRequestTimeout:
		return nil // nothing waiting
	case http.StatusOK:
		// proceed
	default:
		return fmt.Errorf("signaling poll returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("read poll response: %w", err)
	}
	var msg signalingMsg
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("decode poll response: %w", err)
	}
	if msg.Type != "offer" {
		return nil
	}
	if int(p.active.Load()) >= p.cfg.MaxConns {
		log.Printf("webrtc: max connections (%d) reached, rejecting offer for session %s", p.cfg.MaxConns, msg.SessionID)
		return nil
	}
	go p.handleOffer(ctx, msg)
	return nil
}

// offerInfo holds the ICE/DTLS parameters extracted from a browser offer SDP.
type offerInfo struct {
	iceUfrag   string
	icePwd     string
	fpAlgo     string
	fpValue    string
	mid        string
	candidates []string
}

// parseOffer extracts ICE credentials, DTLS fingerprint, mid, and candidates
// from the first application media section of a WebRTC offer SDP.
func parseOffer(sdpStr string) (offerInfo, error) {
	var sess sdp.SessionDescription
	if err := sess.Unmarshal([]byte(sdpStr)); err != nil {
		return offerInfo{}, fmt.Errorf("parse SDP: %w", err)
	}
	for _, m := range sess.MediaDescriptions {
		if m.MediaName.Media != "application" {
			continue
		}
		var info offerInfo
		info.mid = "0" // default
		for _, a := range m.Attributes {
			switch a.Key {
			case "ice-ufrag":
				info.iceUfrag = a.Value
			case "ice-pwd":
				info.icePwd = a.Value
			case "fingerprint":
				parts := strings.SplitN(a.Value, " ", 2)
				if len(parts) == 2 {
					info.fpAlgo = parts[0]
					info.fpValue = parts[1]
				}
			case "mid":
				info.mid = a.Value
			case "candidate":
				info.candidates = append(info.candidates, a.Value)
			}
		}
		// Some browsers place the fingerprint at the session level.
		if info.fpAlgo == "" {
			for _, a := range sess.Attributes {
				if a.Key == "fingerprint" {
					parts := strings.SplitN(a.Value, " ", 2)
					if len(parts) == 2 {
						info.fpAlgo = parts[0]
						info.fpValue = parts[1]
					}
					break
				}
			}
		}
		if info.iceUfrag == "" || info.icePwd == "" {
			return offerInfo{}, fmt.Errorf("offer SDP missing ICE credentials")
		}
		if info.fpAlgo == "" {
			return offerInfo{}, fmt.Errorf("offer SDP missing DTLS fingerprint")
		}
		return info, nil
	}
	return offerInfo{}, fmt.Errorf("offer SDP has no application media section")
}

// buildAnswer constructs a JSEP-compliant SDP answer for a WebRTC data channel.
func buildAnswer(localUfrag, localPwd, fpValue, mid string, candidates []ice.Candidate) (string, error) {
	sess, err := sdp.NewJSEPSessionDescription(false)
	if err != nil {
		return "", err
	}
	sess.WithValueAttribute("group", "BUNDLE "+mid)

	media := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "application",
			Port:    sdp.RangedPort{Value: 9},
			Protos:  []string{"UDP", "DTLS", "SCTP"},
			Formats: []string{"webrtc-datachannel"},
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: "0.0.0.0"},
		},
	}
	media.WithValueAttribute("ice-ufrag", localUfrag)
	media.WithValueAttribute("ice-pwd", localPwd)
	media.WithValueAttribute("ice-options", "trickle")
	media.WithFingerprint("sha-256", fpValue)
	media.WithValueAttribute("setup", "passive")
	media.WithValueAttribute("mid", mid)
	media.WithValueAttribute("sctp-port", "5000")
	media.WithValueAttribute("max-message-size", "262144")
	for _, c := range candidates {
		media.WithValueAttribute("candidate", c.Marshal())
	}
	media.WithPropertyAttribute("end-of-candidates")
	sess.WithMedia(media)

	data, err := sess.Marshal()
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// certFingerprint computes the SHA-256 fingerprint of the first certificate
// in a tls.Certificate, formatted as hex pairs separated by colons.
func certFingerprint(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", errors.New("tls.Certificate has no certificates")
	}
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", err
	}
	return dtlsfp.Fingerprint(x509Cert, crypto.SHA256)
}

// makeFingerprintVerifier returns a VerifyPeerCertificate func that validates
// the remote DTLS certificate fingerprint against the value from the SDP offer.
func makeFingerprintVerifier(algo, expected string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("webrtc: no peer certificate presented")
		}
		x509Cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("webrtc: parse peer certificate: %w", err)
		}
		hashAlgo, err := dtlsfp.HashFromString(algo)
		if err != nil {
			return fmt.Errorf("webrtc: unknown fingerprint algorithm %q: %w", algo, err)
		}
		fp, err := dtlsfp.Fingerprint(x509Cert, hashAlgo)
		if err != nil {
			return fmt.Errorf("webrtc: compute peer fingerprint: %w", err)
		}
		if !strings.EqualFold(fp, expected) {
			return errors.New("webrtc: DTLS fingerprint mismatch")
		}
		return nil
	}
}

func (p *peer) handleOffer(ctx context.Context, offer signalingMsg) {
	p.active.Add(1)
	defer p.active.Add(-1)

	info, err := parseOffer(offer.SDP)
	if err != nil {
		log.Printf("webrtc: parse offer for session %s: %v", offer.SessionID, err)
		return
	}

	// Generate a self-signed DTLS certificate for this connection.
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		log.Printf("webrtc: generate cert: %v", err)
		return
	}
	fp, err := certFingerprint(cert)
	if err != nil {
		log.Printf("webrtc: cert fingerprint: %v", err)
		return
	}

	// Parse STUN server URLs.
	var stunURLs []*stun.URI
	for _, raw := range p.cfg.STUNURLs {
		u, parseErr := stun.ParseURI(raw)
		if parseErr != nil {
			log.Printf("webrtc: parse STUN URL %q: %v", raw, parseErr)
			continue
		}
		stunURLs = append(stunURLs, u)
	}

	// Build a shared pion logger factory. Default is Error; ICE at Info for
	// connection state visibility; DTLS/SCTP at Warn to surface fatal alerts
	// without flooding the log with per-packet trace output.
	logFactory := pionlog.NewDefaultLoggerFactory()
	logFactory.DefaultLogLevel = pionlog.LogLevelError
	logFactory.ScopeLevels = map[string]pionlog.LogLevel{
		"ice":  pionlog.LogLevelInfo,
		"dtls": pionlog.LogLevelWarn,
		"sctp": pionlog.LogLevelWarn,
	}

	// Create the ICE agent and gather candidates.
	gatherDone := make(chan struct{})
	var gatherOnce sync.Once
	agent, err := ice.NewAgentWithOptions(
		ice.WithUrls(stunURLs),
		ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4, ice.NetworkTypeUDP6}),
		ice.WithLoggerFactory(logFactory),
		ice.WithDisconnectedTimeout(30*time.Second),
	)
	if err != nil {
		log.Printf("webrtc: create ICE agent: %v", err)
		return
	}
	defer agent.Close()

	if err := agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			gatherOnce.Do(func() { close(gatherDone) })
		}
	}); err != nil {
		log.Printf("webrtc: OnCandidate: %v", err)
		return
	}

	if err := agent.OnConnectionStateChange(func(state ice.ConnectionState) {
		log.Printf("webrtc: ICE state=%s for session %s", state, offer.SessionID)
	}); err != nil {
		log.Printf("webrtc: OnConnectionStateChange: %v", err)
		return
	}

	if err := agent.GatherCandidates(); err != nil {
		log.Printf("webrtc: gather candidates: %v", err)
		return
	}

	select {
	case <-ctx.Done():
		return
	case <-gatherDone:
	case <-time.After(20 * time.Second):
		log.Printf("webrtc: ICE gathering timed out for session %s", offer.SessionID)
		return
	}

	localUfrag, localPwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		log.Printf("webrtc: get local credentials: %v", err)
		return
	}
	localCandidates, err := agent.GetLocalCandidates()
	if err != nil {
		log.Printf("webrtc: get local candidates: %v", err)
		return
	}

	answerSDP, err := buildAnswer(localUfrag, localPwd, fp, info.mid, localCandidates)
	if err != nil {
		log.Printf("webrtc: build answer SDP: %v", err)
		return
	}

	if err := p.postSignal(ctx, signalingMsg{
		SessionID: offer.SessionID,
		Type:      "answer",
		SDP:       answerSDP,
	}); err != nil {
		log.Printf("webrtc: post answer for session %s: %v", offer.SessionID, err)
		return
	}

	// Add remote ICE candidates from the offer before waiting for connectivity.
	for _, rawCand := range info.candidates {
		c, parseErr := ice.UnmarshalCandidate(rawCand)
		if parseErr != nil {
			log.Printf("webrtc: parse remote candidate: %v", parseErr)
			continue
		}
		if addErr := agent.AddRemoteCandidate(c); addErr != nil {
			log.Printf("webrtc: add remote candidate: %v", addErr)
		}
	}

	// Accept the ICE connection (controlled/answerer side).
	iceCtx, iceCancel := context.WithTimeout(ctx, 30*time.Second)
	defer iceCancel()
	log.Printf("webrtc: waiting for ICE accept for session %s", offer.SessionID)
	iceConn, err := agent.Accept(iceCtx, info.iceUfrag, info.icePwd)
	if err != nil {
		log.Printf("webrtc: ICE accept for session %s: %v", offer.SessionID, err)
		return
	}
	defer iceConn.Close()
	log.Printf("webrtc: ICE accepted for session %s remote=%s", offer.SessionID, iceConn.RemoteAddr())

	// Run DTLS server handshake over the ICE connection.
	log.Printf("webrtc: starting DTLS handshake for session %s", offer.SessionID)
	pconn := dtlsnet.PacketConnFromConn(iceConn)
	dtlsConn, err := dtls.Server(pconn, iceConn.RemoteAddr(), &dtls.Config{
		Certificates:          []tls.Certificate{cert},
		ClientAuth:            dtls.RequireAnyClientCert,
		InsecureSkipVerify:    true, // CA chain not applicable; fingerprint checked below
		VerifyPeerCertificate: makeFingerprintVerifier(info.fpAlgo, info.fpValue),
		LoggerFactory:         logFactory,
		// Browsers always include the use_srtp extension in their DTLS ClientHello
		// (even for data-channel-only connections). Without matching profiles the
		// pion/dtls server sends a fatal alert, killing the connection.
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{
			dtls.SRTP_AEAD_AES_256_GCM,
			dtls.SRTP_AEAD_AES_128_GCM,
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
		},
	})
	if err != nil {
		log.Printf("webrtc: DTLS handshake for session %s: %v", offer.SessionID, err)
		return
	}
	defer dtlsConn.Close()
	log.Printf("webrtc: DTLS handshake complete for session %s", offer.SessionID)

	// Establish SCTP association over DTLS.
	// Per RFC 8832: the DTLS client (browser) is the SCTP client (sends INIT).
	// We are the DTLS server, so we act as the SCTP server (wait for INIT).
	log.Printf("webrtc: starting SCTP server for session %s", offer.SessionID)
	sctpAssoc, err := sctp.Server(sctp.Config{
		NetConn:              &loggedConn{Conn: dtlsConn, sessionID: offer.SessionID},
		MaxReceiveBufferSize: uint32(maxFrameBytes + 64*1024),
		LoggerFactory:        logFactory,
	})
	if err != nil {
		log.Printf("webrtc: SCTP client for session %s: %v", offer.SessionID, err)
		return
	}
	defer sctpAssoc.Close()
	log.Printf("webrtc: SCTP association established for session %s", offer.SessionID)

	// Accept the WebRTC data channel over SCTP.
	log.Printf("webrtc: waiting for data channel accept for session %s", offer.SessionID)
	dc, err := datachannel.Accept(sctpAssoc, &datachannel.Config{
		LoggerFactory: logFactory,
	})
	if err != nil {
		log.Printf("webrtc: accept data channel for session %s: %v", offer.SessionID, err)
		return
	}
	if dc == nil {
		log.Printf("webrtc: accept data channel returned nil for session %s", offer.SessionID)
		return
	}
	defer dc.Close()

	p.runDataChannel(ctx, dc)
}

func (p *peer) runDataChannel(ctx context.Context, dc *datachannel.DataChannel) {
	idle := time.NewTimer(p.cfg.IdleTimeout)
	defer idle.Stop()

	// Serialize writes — concurrent WriteDataChannel calls can interleave frames.
	var sendMu sync.Mutex
	send := func(text string) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		_, err := dc.WriteDataChannel([]byte(text), true)
		return err
	}

	// Signal idle resets via channel so we avoid concurrent Timer.Reset / Timer.C access.
	activity := make(chan struct{}, 1)

	var wg sync.WaitGroup
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, maxFrameBytes+64*1024)
		for {
			n, isString, err := dc.ReadDataChannel(buf)
			if err != nil {
				return
			}
			if !isString {
				continue // only process string (JSON) frames
			}
			if n > maxFrameBytes {
				log.Printf("webrtc: oversized data channel frame (%d bytes), dropping", n)
				continue
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case activity <- struct{}{}:
			default:
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.dispatchRequest(ctx, send, data)
			}()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			_ = dc.Close()
			wg.Wait()
			return
		case <-readDone:
			wg.Wait()
			return
		case <-activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(p.cfg.IdleTimeout)
		case <-idle.C:
			log.Printf("webrtc: connection idle timeout, closing data channel")
			_ = dc.Close()
			wg.Wait()
			return
		}
	}
}

// dispatchRequest decodes a requestFrame, validates it, dispatches it to the
// HTTP handler, and sends response frames via send.
func (p *peer) dispatchRequest(ctx context.Context, send func(string) error, raw []byte) {
	var frame requestFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return
	}

	// Security: reject invalid or out-of-scope paths.
	if !p.validPath(frame.Path) {
		sendDoneFrame(send, frame.ID, http.StatusBadRequest)
		return
	}

	// Security: enforce body size limit before decoding.
	if len(frame.Body) > maxFrameBytes {
		sendDoneFrame(send, frame.ID, http.StatusRequestEntityTooLarge)
		return
	}

	var bodyReader io.Reader = http.NoBody
	if frame.Body != "" {
		bodyBytes, err := base64.StdEncoding.DecodeString(frame.Body)
		if err != nil {
			sendDoneFrame(send, frame.ID, http.StatusBadRequest)
			return
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, frame.Method, "http://localhost"+frame.Path, bodyReader)
	if err != nil {
		sendDoneFrame(send, frame.ID, http.StatusBadRequest)
		return
	}
	for k, v := range frame.Headers {
		req.Header.Set(k, v)
	}

	w := newDCResponseWriter(frame.ID, send)
	p.handler.ServeHTTP(w, req)
	w.finish()
}

// validPath returns true if path is safe to dispatch to the HTTP handler.
// It only permits paths under {basePath}/v1/ to prevent routing to
// admin endpoints, static assets, or parent paths.
func (p *peer) validPath(path string) bool {
	if strings.Contains(path, "..") {
		return false
	}
	prefix := p.cfg.BasePath + "/v1/"
	return strings.HasPrefix(path, prefix)
}

// postSignal POSTs a message to the signaling server.
func (p *peer) postSignal(ctx context.Context, msg signalingMsg) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", p.cfg.SignalingURL+"/signal",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("signaling POST returned %d", resp.StatusCode)
	}
	return nil
}

// loggedConn wraps a net.Conn and logs every Read and Write call.
// It is used to observe what errors SCTP sees on the DTLS connection.
type loggedConn struct {
	net.Conn
	sessionID string
}

func (l *loggedConn) Read(b []byte) (int, error) {
	n, err := l.Conn.Read(b)
	if err != nil {
		log.Printf("webrtc: dtlsConn.Read session=%s err=%v", l.sessionID, err)
	}
	return n, err
}

func (l *loggedConn) Write(b []byte) (int, error) {
	n, err := l.Conn.Write(b)
	if err != nil {
		log.Printf("webrtc: dtlsConn.Write session=%s err=%v", l.sessionID, err)
	}
	return n, err
}

func sendDoneFrame(send func(string) error, id string, status int) {
	f := responseFrame{ID: id, Type: "done", Status: status}
	data, _ := json.Marshal(f)
	_ = send(string(data))
}

// dcResponseWriter implements http.ResponseWriter and http.Flusher.
// It streams the response body as data-channel frames.
type dcResponseWriter struct {
	id          string
	send        func(string) error
	header      http.Header
	status      int
	headersSent bool
	buf         bytes.Buffer
}

func newDCResponseWriter(id string, send func(string) error) *dcResponseWriter {
	return &dcResponseWriter{id: id, send: send, header: make(http.Header)}
}

func (w *dcResponseWriter) Header() http.Header { return w.header }

func (w *dcResponseWriter) WriteHeader(code int) {
	if w.headersSent {
		return
	}
	w.status = code
	w.headersSent = true

	// Send a headers frame so the browser can read response headers
	// (e.g. x-response-id) before the body stream begins.
	hdrs := make(map[string]string, len(w.header))
	for k, v := range w.header {
		if len(v) > 0 {
			hdrs[strings.ToLower(k)] = v[0]
		}
	}
	f := responseFrame{ID: w.id, Type: "headers", Status: code, Headers: hdrs}
	data, _ := json.Marshal(f)
	_ = w.send(string(data))
}

func (w *dcResponseWriter) Write(b []byte) (int, error) {
	if !w.headersSent {
		w.WriteHeader(http.StatusOK)
	}
	w.buf.Write(b)
	// Flush complete lines immediately so streaming handlers (SSE) work in real time.
	for {
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(data[:idx])
		w.buf.Next(idx + 1)
		f := responseFrame{ID: w.id, Type: "chunk", Data: line}
		enc, _ := json.Marshal(f)
		_ = w.send(string(enc))
	}
	return len(b), nil
}

// Flush implements http.Flusher so SSE handlers can push events incrementally.
func (w *dcResponseWriter) Flush() {
	// Write drains complete lines; nothing to flush here unless the
	// handler omits a trailing newline. finish() handles that case.
}

// finish flushes any remaining buffered data and is called after ServeHTTP returns.
func (w *dcResponseWriter) finish() {
	if w.buf.Len() > 0 {
		f := responseFrame{ID: w.id, Type: "chunk", Data: w.buf.String()}
		w.buf.Reset()
		enc, _ := json.Marshal(f)
		_ = w.send(string(enc))
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	sendDoneFrame(w.send, w.id, status)
}
