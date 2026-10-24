package peerbroker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ServeOptions configure the foreground broker binary. Nothing here installs a
// service. Public listeners require TLS; an SSH-only demo binds 127.0.0.1.
type ServeOptions struct {
	PreissuedTURNFile                    string // approved demo only: finite, pre-provisioned SELF-HOSTED credentials
	Host                                 string
	Port                                 int
	RegistrationFile                     string
	TLSCert, TLSKey                      string
	CloudflareKeyID, CloudflareTokenFile string
	MaxSessions                          int
}

func (o ServeOptions) Validate() error {
	if o.PreissuedTURNFile != "" && (o.CloudflareKeyID != "" || o.CloudflareTokenFile != "") {
		return errors.New("choose preissued SELF-HOSTED TURN or Cloudflare, never both")
	}
	if o.Port < 1 || o.Port > 65535 {
		return errors.New("invalid peer broker port")
	}
	if o.MaxSessions < 1 || o.MaxSessions > 64 {
		return errors.New("peer pilot max-sessions must be 1-64")
	}
	if (o.TLSCert == "") != (o.TLSKey == "") {
		return errors.New("peer broker TLS requires both certificate and key")
	}
	ip := net.ParseIP(o.Host)
	if o.TLSCert == "" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("peer broker non-loopback binds require TLS; use 127.0.0.1 behind an approved TLS proxy or signalling-only SSH forward")
	}
	if (o.CloudflareKeyID == "") != (o.CloudflareTokenFile == "") {
		return errors.New("Cloudflare TURN requires both key ID and private API token file")
	}
	return nil
}

// Run starts only the authenticated broker listener, and closes every owned
// signalling websocket on ctx cancellation. No decrypted chat relay exists.
func Run(ctx context.Context, o ServeOptions, status func(string)) error {
	if err := o.Validate(); err != nil {
		return err
	}
	token, err := ReadCapabilityFile(o.RegistrationFile)
	if err != nil {
		return err
	}
	cfg := Config{CreateToken: token, MaxSessions: o.MaxSessions, MaxConnections: 2 * o.MaxSessions, MaxWaiters: 2 * o.MaxSessions, RatePerSecond: 10, RateBurst: 20, SessionTTL: MaxSessionTTL}
	if o.CloudflareKeyID != "" {
		apiToken, err := ReadCapabilityFile(o.CloudflareTokenFile)
		if err != nil {
			return err
		}
		cfg.TURNMinter, err = NewCloudflareTURNMinter(CloudflareTURNConfig{KeyID: o.CloudflareKeyID, APIToken: apiToken})
		if err != nil {
			return err
		}
	}
	if o.PreissuedTURNFile != "" {
		cfg.TURNMinter, err = NewPreissuedTURNFile(o.PreissuedTURNFile, time.Now())
		if err != nil {
			return err
		}
		cfg.TURNTTL = MinTURNTTL
		if status != nil {
			status("TURN credentials source: preissued SELF-HOSTED demo credentials; NOT Cloudflare.")
		}
	}
	handler, err := NewHandler(cfg)
	if err != nil {
		return err
	}
	defer handler.Close()
	server := &http.Server{Addr: net.JoinHostPort(o.Host, strconv.Itoa(o.Port)), Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() {
		if o.TLSCert != "" {
			finished <- server.ServeTLS(listener, o.TLSCert, o.TLSKey)
		} else {
			finished <- server.Serve(listener)
		}
	}()
	if status != nil {
		status("Peer signalling broker started; authenticated pilot registration required. No chat payload relay.")
	}
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		handler.Close()
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(stop)
		if err != nil {
			_ = server.Close()
		}
		<-finished
		return err
	}
}
