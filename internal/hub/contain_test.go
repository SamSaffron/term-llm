package hub

import (
	"fmt"
	"testing"

	"github.com/samsaffron/term-llm/internal/contain"
)

func TestContainResolverDiscoversWorkspaces(t *testing.T) {
	r := &ContainResolver{
		Host: "127.0.0.1",
		List: func() ([]contain.ListEntry, error) {
			return []contain.ListEntry{
				{Name: "jarvis", Status: "running"},
				{Name: "untokened", Status: "running"},
				{Name: "broken", Status: "missing"},
			}, nil
		},
		Read: func(name string) (contain.WebConfig, error) {
			switch name {
			case "jarvis":
				return contain.WebConfig{Port: "8222", Token: "tkn-1", BasePath: "/chat"}, nil
			case "untokened":
				return contain.WebConfig{Port: "8223", Token: "", BasePath: "/chat"}, nil
			default:
				return contain.WebConfig{}, fmt.Errorf("no .env")
			}
		},
	}

	nodes, err := r.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	// Workspaces without a provisioned token or readable .env are skipped.
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1: %+v", len(nodes), nodes)
	}
	n := nodes[0]
	if n.ID != "jarvis" || n.Source != SourceContain || n.Token != "tkn-1" {
		t.Errorf("node = %+v", n)
	}
	if n.URL != "http://127.0.0.1:8222" || n.BasePath != "/chat" {
		t.Errorf("url/base = %q %q", n.URL, n.BasePath)
	}
	if n.BaseURL() != "http://127.0.0.1:8222/chat" {
		t.Errorf("BaseURL = %q", n.BaseURL())
	}
}

func TestContainResolverDelegationWorkdir(t *testing.T) {
	r := &ContainResolver{
		Host: "127.0.0.1",
		List: func() ([]contain.ListEntry, error) {
			return []contain.ListEntry{{Name: "jarvis"}, {Name: "nohint"}}, nil
		},
		Read: func(name string) (contain.WebConfig, error) {
			return contain.WebConfig{Port: "8222", Token: "tkn-1", BasePath: "/chat"}, nil
		},
		Workspace: func(name string) string {
			if name == "jarvis" {
				return "/workspace"
			}
			return ""
		},
	}
	nodes, err := r.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes) = %d, want 2", len(nodes))
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	withHint := byID["jarvis"]
	if withHint.Delegation == nil || withHint.Delegation.Workdir != "/workspace" {
		t.Errorf("workspace hint should set the delegation workdir: %+v", withHint.Delegation)
	}
	if err := withHint.AcceptsDelegationFrom("anyone"); err != nil {
		t.Errorf("contain node with workdir should accept delegations: %v", err)
	}
	noHint := byID["nohint"]
	if noHint.Delegation != nil {
		t.Errorf("workspace without a hint must stay non-accepting: %+v", noHint.Delegation)
	}
}

func TestContainResolverListError(t *testing.T) {
	r := &ContainResolver{
		Host: "127.0.0.1",
		List: func() ([]contain.ListEntry, error) { return nil, fmt.Errorf("no config dir") },
		Read: func(string) (contain.WebConfig, error) { return contain.WebConfig{}, nil },
	}
	if _, err := r.Nodes(); err == nil {
		t.Fatal("list error swallowed, want error")
	}
}
