package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/cliwire"
)

const cliWireStopTimeout = 5 * time.Second

func startCLIWireAudit(provider string, env []string) ([]string, *cliwire.Server, error) {
	server, err := startCLIWireServer(provider, env)
	if err != nil || server == nil {
		return env, server, err
	}
	return forceCLIWireEnvironment(env, server.Environment()), server, nil
}

func startCLIWireServer(provider string, env []string) (*cliwire.Server, error) {
	traceRoot := envValue(env, cliwire.TraceRootEnv)
	if strings.TrimSpace(traceRoot) == "" {
		return nil, nil
	}
	server, err := cliwire.StartWithAdditionalCA(traceRoot, provider, envValue(env, "SSL_CERT_FILE"))
	if err != nil {
		return nil, fmt.Errorf("start %s wire audit: %w", provider, err)
	}
	return server, nil
}

func forceCLIWireEnvironment(env []string, forced map[string]string) []string {
	if len(forced) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(forced))
	for _, entry := range env {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if key == cliwire.TraceRootEnv {
			continue
		}
		if _, ok := forced[key]; ok {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range forced {
		out = append(out, key+"="+value)
	}
	return out
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func stopCLIWireAudit(server *cliwire.Server) {
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliWireStopTimeout)
	_ = server.Stop(ctx)
	cancel()
}
