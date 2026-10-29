package server

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// GuardHost rejects unconfigured DNS names before any HTTP route (including
// authentication and streaming). Browser Origin and Fetch Metadata checks alone
// cannot detect DNS rebinding. This uses only local string/IP parsing, never DNS.
// Wildcard listeners accept literal IPs; named reverse proxies must be explicitly
// configured with publicURL. Forwarded headers are deliberately not trusted.
func GuardHost(next http.Handler, bindHost, publicURL string) http.Handler {
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	bindHost = strings.TrimSuffix(strings.Trim(strings.ToLower(strings.TrimSpace(bindHost)), "[]"), ".")
	if bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" {
		allowed[bindHost] = true
	}
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		allowed[strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")] = true
	}
	wildcard := bindHost == "0.0.0.0" || bindHost == "::" || bindHost == ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := requestHostname(r.Host)
		if !allowed[host] {
			ip, err := netip.ParseAddr(host)
			if !wildcard || err != nil || ip.IsUnspecified() {
				http.Error(w, "unrecognized Host; configure --public-url for named proxies", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func requestHostname(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return strings.TrimSuffix(strings.ToLower(host), ".")
	}
	// A bracketed IPv6 literal without a port is valid; unbracketed IPv6 is
	// not a valid HTTP authority. Reject malformed ports rather than guessing.
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		return strings.ToLower(authority[1 : len(authority)-1])
	}
	if strings.ContainsAny(authority, ":/@\\ \t\r\n") {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(authority), ".")
}
