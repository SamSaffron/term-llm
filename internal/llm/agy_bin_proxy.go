package llm

import (
	"context"
	"path/filepath"
	"time"

	"github.com/samsaffron/term-llm/internal/agyproxy"
)

// agyToolIsolation is the single compatibility seam for agy issue #674.
// Once agy can expose MCP while its native tool list is empty, replace
// newAgyToolIsolation with a direct CLI implementation and delete
// internal/agyproxy plus this proxy-backed implementation.
type agyToolIsolation interface {
	EnsureStarted(string) error
	BeginTurn(bool)
	FilteredGenerations() int64
	Environment() map[string]string
	Stop(context.Context) error
}

type agyProxyToolIsolation struct {
	server   *agyproxy.Server
	proxyURL string
	caPath   string
}

func newAgyToolIsolation() agyToolIsolation { return &agyProxyToolIsolation{} }

func (i *agyProxyToolIsolation) EnsureStarted(agyHome string) error {
	artifactRoot := filepath.Join(agyHome, ".gemini", "antigravity-cli", "brain")
	if i.server != nil {
		i.server.SetArtifactRoot(artifactRoot)
		return nil
	}
	server := &agyproxy.Server{}
	server.SetArtifactRoot(artifactRoot)
	proxyURL, caPath, err := server.Start()
	if err != nil {
		return err
	}
	i.server, i.proxyURL, i.caPath = server, proxyURL, caPath
	return nil
}

func (i *agyProxyToolIsolation) BeginTurn(requireMCP bool) {
	if i.server != nil {
		i.server.BeginTurn(requireMCP)
	}
}

func (i *agyProxyToolIsolation) FilteredGenerations() int64 {
	if i.server == nil {
		return 0
	}
	return i.server.FilteredGenerations()
}

func (i *agyProxyToolIsolation) Environment() map[string]string {
	if i.server == nil {
		return nil
	}
	return map[string]string{
		"HTTP_PROXY": i.proxyURL, "http_proxy": i.proxyURL,
		"HTTPS_PROXY": i.proxyURL, "https_proxy": i.proxyURL,
		"ALL_PROXY": "", "all_proxy": "",
		"NO_PROXY": "127.0.0.1,localhost", "no_proxy": "127.0.0.1,localhost",
		"SSL_CERT_FILE":       i.caPath,
		"NODE_EXTRA_CA_CERTS": i.caPath,
	}
}

func (i *agyProxyToolIsolation) Stop(ctx context.Context) error {
	if i.server == nil {
		return nil
	}
	err := i.server.Stop(ctx)
	i.server, i.proxyURL, i.caPath = nil, "", ""
	return err
}

const agyIsolationStopTimeout = 5 * time.Second
