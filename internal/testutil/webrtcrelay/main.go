// Command webrtcrelay is an isolated loopback signaling fixture for browser
// integration tests. It is not linked into the production term-llm binary.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type signal struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"`
	SDP       string `json:"sdp,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type relay struct {
	offers       chan signal
	mu           sync.Mutex
	answers      map[string]signal
	next         atomic.Uint64
	offerN       atomic.Uint64
	answerN      atomic.Uint64
	failSessions atomic.Int64
	hangSessions atomic.Int64
	hangOffers   atomic.Int64
	rejectOffers atomic.Int64
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "loopback listen address")
	certPath := flag.String("cert-out", "", "path for generated test certificate")
	keyPath := flag.String("key-out", "", "path for generated test private key")
	readyPath := flag.String("ready-out", "", "path written with the HTTPS URL once listening")
	flag.Parse()
	if *certPath == "" || *keyPath == "" || *readyPath == "" {
		fmt.Fprintln(os.Stderr, "cert-out, key-out, and ready-out are required")
		os.Exit(2)
	}
	if err := generateCertificate(*certPath, *keyPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r := &relay{offers: make(chan signal, 64), answers: make(map[string]signal)}
	server := &http.Server{Handler: r.handler(), ReadHeaderTimeout: 5 * time.Second}
	url := "https://" + listener.Addr().String()
	if err := os.WriteFile(*readyPath, []byte(url), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tlsListener, err := newTLSListener(listener, *certPath, *keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := server.Serve(tlsListener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (r *relay) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/session", r.session)
	mux.HandleFunc("/signal", r.signaling)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]uint64{"offers": r.offerN.Load(), "answers": r.answerN.Load()})
	})
	mux.HandleFunc("/control", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var control struct {
			FailSessions int64 `json:"fail_sessions"`
			HangSessions int64 `json:"hang_sessions"`
			HangOffers   int64 `json:"hang_offers"`
			RejectOffers int64 `json:"reject_offers"`
		}
		if err := json.NewDecoder(req.Body).Decode(&control); err != nil || control.FailSessions < 0 || control.HangSessions < 0 || control.HangOffers < 0 || control.RejectOffers < 0 {
			http.Error(w, "invalid control", http.StatusBadRequest)
			return
		}
		r.failSessions.Store(control.FailSessions)
		r.hangSessions.Store(control.HangSessions)
		r.hangOffers.Store(control.HangOffers)
		r.rejectOffers.Store(control.RejectOffers)
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, req)
	})
}

func consume(counter *atomic.Int64) bool {
	for {
		remaining := counter.Load()
		if remaining <= 0 {
			return false
		}
		if counter.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
}

func (r *relay) session(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if consume(&r.hangSessions) {
		<-req.Context().Done()
		return
	}
	if consume(&r.failSessions) {
		http.Error(w, "injected signaling session failure", http.StatusGatewayTimeout)
		return
	}
	id := fmt.Sprintf("browser-%d", r.next.Add(1))
	writeJSON(w, map[string]string{"session_id": id, "stun_url": "stun:127.0.0.1:9"})
}

func (r *relay) signaling(w http.ResponseWriter, req *http.Request) {
	sessionID := req.URL.Query().Get("session_id")
	if req.Method == http.MethodGet && sessionID != "" {
		r.mu.Lock()
		answer, ok := r.answers[sessionID]
		if ok {
			delete(r.answers, sessionID)
		}
		r.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, answer)
		return
	}
	if req.Method == http.MethodGet {
		select {
		case offer := <-r.offers:
			writeJSON(w, offer)
		case <-time.After(750 * time.Millisecond):
			w.WriteHeader(http.StatusNoContent)
		case <-req.Context().Done():
		}
		return
	}
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var message signal
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128<<10)).Decode(&message); err != nil {
		http.Error(w, "invalid signal", http.StatusBadRequest)
		return
	}
	switch message.Type {
	case "offer":
		r.offerN.Add(1)
		if consume(&r.hangOffers) {
			<-req.Context().Done()
			return
		}
		if consume(&r.rejectOffers) {
			r.answerN.Add(1)
			r.mu.Lock()
			r.answers[message.SessionID] = signal{SessionID: message.SessionID, Type: "rejected", Reason: "injected admission rejection"}
			r.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case r.offers <- message:
			w.WriteHeader(http.StatusAccepted)
		default:
			http.Error(w, "offer queue full", http.StatusServiceUnavailable)
		}
	case "answer", "rejected":
		r.answerN.Add(1)
		r.mu.Lock()
		r.answers[message.SessionID] = message
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "unsupported signal", http.StatusBadRequest)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func newTLSListener(listener net.Listener, certPath, keyPath string) (net.Listener, error) {
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(listener, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	}), nil
}

func generateCertificate(certPath, keyPath string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "term-llm browser test relay"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, cert, 0o600); err != nil {
		return err
	}
	return os.WriteFile(keyPath, private, 0o600)
}
