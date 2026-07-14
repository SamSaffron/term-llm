package cmd

import "context"

// proxyForcedRoute pins a request to a specific upstream provider/model. It is
// set by the proxy authorization gate (serve_proxy.go) after a client request
// has been authorized against its grants, and honored at the two serve runtime
// chokepoints so that all three provider protocols (OpenAI Responses, OpenAI
// Chat, Anthropic Messages) dispatch to the granted provider — including local
// binary providers like claude-bin exported over HTTP.
//
// Model may be empty to mean "use the provider's default model" (e.g. when a
// wildcard grant is exercised).
type proxyForcedRoute struct {
	Provider string
	Model    string
}

type proxyForcedRouteKeyType struct{}

var proxyForcedRouteKey proxyForcedRouteKeyType

// withProxyForcedRoute returns a context carrying the forced route.
func withProxyForcedRoute(ctx context.Context, provider, model string) context.Context {
	return context.WithValue(ctx, proxyForcedRouteKey, proxyForcedRoute{Provider: provider, Model: model})
}

// proxyForcedRouteFromContext returns the forced route, if any. The boolean is
// false for ordinary (non-proxy) serve requests, making the runtime hooks a
// no-op outside proxy mode.
func proxyForcedRouteFromContext(ctx context.Context) (proxyForcedRoute, bool) {
	if ctx == nil {
		return proxyForcedRoute{}, false
	}
	route, ok := ctx.Value(proxyForcedRouteKey).(proxyForcedRoute)
	if !ok || route.Provider == "" {
		return proxyForcedRoute{}, false
	}
	return route, true
}
