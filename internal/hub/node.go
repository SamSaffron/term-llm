// Package hub models a fleet of term-llm web nodes for `term-llm serve hub`.
//
// The core object is a Node: a reachable term-llm serve (web/API endpoint)
// with an identity, a backend URL + base path, an optional bearer token, and
// a source describing which resolver produced it. Resolvers (static config,
// contain workspaces, the local UI-added store) feed a Registry, which is the
// single lookup surface the hub server routes and proxies from.
//
// TODO(hub): node self-registration, scheduling, and mTLS between hub and
// nodes are deliberately out of scope for v1; see docs-site guide "Hub".
package hub

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Source labels for the built-in resolvers.
const (
	SourceConfig  = "config"
	SourceContain = "contain"
	SourceLocal   = "local"
)

// Node is one reachable term-llm web/API endpoint known to the hub.
type Node struct {
	// ID uniquely identifies the node within the hub and is used as the
	// proxy path segment (/node/<id>/...). Restricted to a path-safe charset.
	ID string `json:"id" yaml:"id"`
	// Name is the human-facing label shown on the dashboard.
	Name string `json:"name" yaml:"name"`
	// Source is the resolver that produced this node (config/contain/local).
	Source string `json:"source" yaml:"-"`
	// Connection is how the Hub reaches this node. Empty/"direct" means the Hub
	// dials URL. "reverse" means the node must dial the Hub and keep a websocket
	// open; useful for private nodes behind NAT/firewalls.
	Connection string `json:"connection,omitempty" yaml:"connection"`
	// URL is the backend origin, e.g. "http://127.0.0.1:8081". Never sent to
	// hub clients together with Token.
	URL string `json:"url" yaml:"url"`
	// BasePath is the URL prefix the node's serve is mounted under, e.g.
	// "/chat". Always rooted, no trailing slash; root-mounted serves are not
	// supported by Hub v1 because path-based proxying needs a stable prefix.
	BasePath string `json:"base_path,omitempty" yaml:"base_path"`
	// Token is the node's web bearer token. It is injected server-side by the
	// hub proxy and MUST never be marshalled into any client-facing response;
	// API handlers convert Node to a public view first.
	Token string `json:"token,omitempty" yaml:"token"`
	// Delegation is the node's cross-node delegation policy. Nil or disabled
	// means the node cannot originate or accept delegations.
	Delegation *DelegationPolicy `json:"delegation,omitempty" yaml:"delegation"`
}

// BaseURL returns the node's backend origin joined with its base path,
// without a trailing slash (e.g. "http://127.0.0.1:8081/chat"). Reverse nodes
// may not have a URL; callers that dial directly must check UsesReverseConnection first.
func (n Node) BaseURL() string {
	return strings.TrimRight(n.URL, "/") + n.BasePath
}

func (n Node) UsesReverseConnection() bool {
	return strings.EqualFold(strings.TrimSpace(n.Connection), "reverse")
}

var nodeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateID rejects node IDs that are empty or unsafe as a single proxy path
// segment. The charset deliberately excludes '/', '%', and whitespace so an ID
// can never smuggle extra path segments into /node/<id>/.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("node id must not be empty")
	}
	if len(id) > 64 {
		return fmt.Errorf("node id %q too long (max 64 chars)", id)
	}
	if !nodeIDPattern.MatchString(id) {
		return fmt.Errorf("invalid node id %q: use letters, digits, '.', '_' or '-' (must start with a letter or digit)", id)
	}
	return nil
}

// SlugID derives a valid node ID from a free-form name, for nodes added
// without an explicit ID. Returns an error when nothing usable remains.
func SlugID(name string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-._")
	if len(slug) > 64 {
		slug = slug[:64]
	}
	if err := ValidateID(slug); err != nil {
		return "", fmt.Errorf("cannot derive node id from %q: %w", name, err)
	}
	return slug, nil
}

// ParseNodeURL splits a node URL like "http://127.0.0.1:8081/chat" into its
// origin ("http://127.0.0.1:8081") and normalized base path ("/chat"). A URL
// without a path yields an empty base path; Normalize rejects that unless an
// explicit BasePath is supplied, because Hub v1's path proxy needs a prefix.
func ParseNodeURL(raw string) (origin, basePath string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("node url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid node url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("invalid node url %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("invalid node url %q: missing host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("invalid node url %q: must not carry a query or fragment", raw)
	}
	basePath = strings.TrimRight(u.Path, "/")
	if basePath != "" && !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	return u.Scheme + "://" + u.Host, basePath, nil
}

// Normalize validates and canonicalizes a node in place: the URL is split
// into origin + base path (an explicit BasePath wins over a path embedded in
// URL), the ID falls back to a slug of the name, and the name falls back to
// the ID.
func (n *Node) Normalize() error {
	n.Connection = strings.ToLower(strings.TrimSpace(n.Connection))
	if n.Connection == "" {
		n.Connection = "direct"
	}
	if n.Connection != "direct" && n.Connection != "reverse" {
		return fmt.Errorf("invalid node connection %q: use direct or reverse", n.Connection)
	}
	var urlBase string
	if n.URL != "" {
		origin, parsedBase, err := ParseNodeURL(n.URL)
		if err != nil {
			return err
		}
		n.URL = origin
		urlBase = parsedBase
	} else if !n.UsesReverseConnection() {
		return fmt.Errorf("node url must not be empty")
	}
	if n.BasePath == "" {
		n.BasePath = urlBase
	}
	n.BasePath = strings.TrimRight(n.BasePath, "/")
	if n.BasePath != "" && !strings.HasPrefix(n.BasePath, "/") {
		n.BasePath = "/" + n.BasePath
	}
	if n.BasePath == "" {
		return fmt.Errorf("node base path must not be root; use the serve web base path such as /chat")
	}
	if n.ID == "" {
		name := n.Name
		if name == "" {
			name = strings.TrimPrefix(strings.TrimPrefix(n.URL, "https://"), "http://")
		}
		id, err := SlugID(name)
		if err != nil {
			return err
		}
		n.ID = id
	}
	if err := ValidateID(n.ID); err != nil {
		return err
	}
	if n.Name == "" {
		n.Name = n.ID
	}
	return nil
}
