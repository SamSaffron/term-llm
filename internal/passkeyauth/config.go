package passkeyauth

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Endpoint is the browser-visible WebAuthn relying-party configuration.
type Endpoint struct {
	URL        *url.URL
	Origin     string
	RPID       string
	BasePath   string
	CookiePath string
	Secure     bool
}

type EndpointOptions struct {
	PublicURL        string
	BasePath         string
	BasePathExplicit bool
}

// ParseEndpoint validates a browser-visible URL without consulting
// request-controlled Host or forwarding headers.
func ParseEndpoint(opts EndpointOptions) (Endpoint, error) {
	rawPublicURL := strings.TrimSpace(opts.PublicURL)
	rawBasePath := opts.BasePath
	basePathExplicit := opts.BasePathExplicit
	if rawPublicURL == "" {
		return Endpoint{}, fmt.Errorf("public URL is required for passkey authentication")
	}
	u, err := url.Parse(rawPublicURL)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return Endpoint{}, fmt.Errorf("public URL must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Endpoint{}, fmt.Errorf("public URL must not contain userinfo, query, or fragment")
	}
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") || port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return Endpoint{}, fmt.Errorf("public URL has an invalid port")
		}
	}
	if hostname == "" || net.ParseIP(hostname) != nil {
		return Endpoint{}, fmt.Errorf("public URL host must be a domain name, not an IP address")
	}
	if !validDomain(hostname) {
		return Endpoint{}, fmt.Errorf("public URL has an invalid domain name")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(scheme == "http" && hostname == "localhost") {
		return Endpoint{}, fmt.Errorf("public URL must use HTTPS (http is allowed only for localhost)")
	}
	if _, err := url.ParseRequestURI(u.EscapedPath()); err != nil || strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") || strings.Contains(strings.ToLower(u.EscapedPath()), "%5c") {
		return Endpoint{}, fmt.Errorf("public URL contains an invalid path")
	}
	publicBase, err := normalizeBasePath(u.Path)
	if err != nil {
		return Endpoint{}, fmt.Errorf("public URL path: %w", err)
	}
	base := publicBase
	if basePathExplicit {
		base, err = normalizeBasePath(rawBasePath)
		if err != nil {
			return Endpoint{}, fmt.Errorf("base path: %w", err)
		}
		if base != publicBase {
			return Endpoint{}, fmt.Errorf("base path %q does not match public URL path %q", displayPath(base), displayPath(publicBase))
		}
	}
	u.Scheme = scheme
	u.Host = canonicalHost(hostname, port, scheme)
	cookiePath := "/"
	if base != "" {
		cookiePath = base + "/"
	}
	u.Path = cookiePath
	u.RawPath = ""
	origin := scheme + "://" + u.Host
	return Endpoint{URL: u, Origin: origin, RPID: hostname, BasePath: base, CookiePath: cookiePath, Secure: scheme == "https"}, nil
}

func validDomain(host string) bool {
	if host == "localhost" {
		return true
	}
	if len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func canonicalHost(hostname, port, scheme string) string {
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		return hostname
	}
	return net.JoinHostPort(hostname, port)
}

func normalizeBasePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/" {
		return "", nil
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#\\") || hasDotDot(p) {
		return "", fmt.Errorf("must be a root-relative path without .. segments")
	}
	p = path.Clean(p)
	if p == "/" || p == "." {
		return "", nil
	}
	return p, nil
}

func hasDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func displayPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// SafeReturnPath accepts only a root-relative browser path beneath the configured mount.
func (e Endpoint) SafeReturnPath(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return e.CookiePath
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || hasDotDot(u.Path) {
		return e.CookiePath
	}
	if e.BasePath != "" && u.Path != e.BasePath && !strings.HasPrefix(u.Path, e.BasePath+"/") {
		return e.CookiePath
	}
	u.RawQuery = ""
	return u.EscapedPath()
}
