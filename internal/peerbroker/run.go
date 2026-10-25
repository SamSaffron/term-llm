package peerbroker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

type ServeOptions struct {
	Host           string
	Port           int
	TLSCert        string
	TLSKey         string
	AdmissionFile  string
	STUNURLs       []string
	TURNURLs       []string
	TURNSecretFile string
	TURNUsername   string
	TURNTTL        time.Duration
	LeaseTTL       time.Duration
	MaxHosts       int
}

func (o ServeOptions) Validate() error {
	if o.Host == "" {
		return errors.New("peerbroker: bind host is required")
	}
	if o.Port < 1 || o.Port > 65535 {
		return errors.New("peerbroker: port must be 1-65535")
	}
	if (o.TLSCert == "") != (o.TLSKey == "") {
		return errors.New("peerbroker: TLS requires both certificate and key")
	}
	bindIP := net.ParseIP(o.Host)
	if o.TLSCert == "" && (bindIP == nil || !bindIP.IsLoopback()) {
		return errors.New("peerbroker: TLS is required for non-loopback binds")
	}
	if (len(o.TURNURLs) == 0) != (o.TURNSecretFile == "") {
		return errors.New("peerbroker: coturn requires both TURN URLs and a shared-secret file")
	}
	if o.MaxHosts < 1 || o.MaxHosts > 10000 {
		return errors.New("peerbroker: max-hosts must be 1-10000")
	}
	return nil
}

func Run(ctx context.Context, options ServeOptions, status func(string)) error {
	if err := options.Validate(); err != nil {
		return err
	}
	cfg := Config{LeaseTTL: options.LeaseTTL, MaxHosts: options.MaxHosts, STUNURLs: append([]string(nil), options.STUNURLs...)}
	var err error
	if options.AdmissionFile != "" {
		cfg.AdmissionCredential, err = ReadSecretFile(options.AdmissionFile)
		if err != nil {
			return err
		}
	}
	if len(options.TURNURLs) > 0 {
		secret, err := ReadSecretFile(options.TURNSecretFile)
		if err != nil {
			return err
		}
		cfg.Coturn = &CoturnConfig{URLs: append([]string(nil), options.TURNURLs...), SharedSecret: secret, TTL: options.TURNTTL, Username: options.TURNUsername}
	}
	handler, err := NewHandler(cfg)
	if err != nil {
		return err
	}
	defer handler.Close()
	server := &http.Server{
		Addr:              net.JoinHostPort(options.Host, strconv.Itoa(options.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("peerbroker: listen: %w", err)
	}
	finished := make(chan error, 1)
	go func() {
		if options.TLSCert != "" {
			finished <- server.ServeTLS(listener, options.TLSCert, options.TLSKey)
		} else {
			finished <- server.Serve(listener)
		}
	}()
	if status != nil {
		status("peer broker v2 listening on " + server.Addr)
	}
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("peerbroker: serve: %w", err)
	case <-ctx.Done():
		handler.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			_ = server.Close()
		}
		<-finished
		return err
	}
}
