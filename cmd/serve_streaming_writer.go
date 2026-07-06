package cmd

import (
	"net/http"
	"time"

	servehttp "github.com/samsaffron/term-llm/internal/serve/http"
)

const (
	serveReadHeaderTimeout     = servehttp.ReadHeaderTimeout
	serveIdleTimeout           = servehttp.IdleTimeout
	serveStreamWriteTimeout    = servehttp.StreamWriteTimeout
	durableResponseLookupLimit = servehttp.DurableResponseLookupLimit
)

func newStreamingResponseWriter(w http.ResponseWriter, timeout time.Duration) http.ResponseWriter {
	return servehttp.NewStreamingResponseWriter(w, timeout)
}
