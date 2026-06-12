package hub

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// configFile is the on-disk shape of a hub nodes config (--config). YAML is
// the primary format; JSON parses too since YAML is a superset.
//
//	nodes:
//	  - id: jarvis            # optional, derived from name when omitted
//	    name: Jarvis          # optional, falls back to id
//	    url: http://127.0.0.1:8081/chat
//	    base_path: /chat      # optional, may also be embedded in url
//	    token: secret         # optional web bearer token, injected server-side
//	  - id: private
//	    connection: reverse  # node dials /api/connect; no Hub-reachable url required
//	    base_path: /chat
//	    token: secret
type configFile struct {
	Nodes []Node `yaml:"nodes"`
}

// ParseConfig parses a static hub config document into normalized nodes.
func ParseConfig(data []byte) ([]Node, error) {
	var cfg configFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse hub config: %w", err)
	}
	seen := map[string]bool{}
	nodes := make([]Node, 0, len(cfg.Nodes))
	for i := range cfg.Nodes {
		n := cfg.Nodes[i]
		n.Source = SourceConfig
		if err := n.Normalize(); err != nil {
			return nil, fmt.Errorf("hub config node %d: %w", i+1, err)
		}
		if seen[n.ID] {
			return nil, fmt.Errorf("hub config node %d: duplicate node id %q", i+1, n.ID)
		}
		seen[n.ID] = true
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// StaticResolver resolves nodes from a config file on every call, so edits
// are picked up without restarting the hub.
type StaticResolver struct {
	Path string
}

// NewStaticResolver returns a resolver over the given config file path.
func NewStaticResolver(path string) *StaticResolver {
	return &StaticResolver{Path: path}
}

// Source implements Resolver.
func (s *StaticResolver) Source() string { return SourceConfig }

// Nodes implements Resolver by re-reading and re-parsing the config file.
func (s *StaticResolver) Nodes() ([]Node, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read hub config: %w", err)
	}
	return ParseConfig(data)
}
