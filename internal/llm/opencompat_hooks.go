package llm

import (
	"net"
	"net/http"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
)

// The OpenAI-compatible provider family lives in providers/opencompat; this
// installs the root-owned hooks.

func init() {
	opencompat.ParentHooks = opencompat.Hooks{
		InputLimitForProviderModel: InputLimitForProviderModel,
		InputLimitForModel:         InputLimitForModel,
		PricingForProviderModel:    PricingForProviderModel,
		ClampOutputTokens:          ClampOutputTokens,
		HTTPClient:                 func() *http.Client { return defaultHTTPClient },
	}
}

// newStreamingHTTPClient returns a client with transport-level timeouts only,
// so active streams are never killed by an overall request deadline.
func newStreamingHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 100
	return &http.Client{Transport: transport}
}

// defaultHTTPClient is the shared streaming client. http.Client.Timeout is
// intentionally unset: it would abort legitimately long-running streams.
var defaultHTTPClient = newStreamingHTTPClient()

func boolPtr(v bool) *bool { return &v }

func setNonEmptyHeaders(header http.Header, values map[string]string) {
	for key, value := range values {
		if value != "" {
			header.Set(key, value)
		}
	}
}
