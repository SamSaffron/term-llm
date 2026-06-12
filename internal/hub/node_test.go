package hub

import (
	"strings"
	"testing"
)

func TestNodeNormalizeReverseConnection(t *testing.T) {
	n := Node{ID: "artist", Name: "Artist", Connection: "reverse", BasePath: "/chat"}
	if err := n.Normalize(); err != nil {
		t.Fatalf("Normalize reverse: %v", err)
	}
	if !n.UsesReverseConnection() {
		t.Fatalf("UsesReverseConnection = false")
	}
	if n.URL != "" || n.BasePath != "/chat" || n.Connection != "reverse" {
		t.Fatalf("normalized node = %+v", n)
	}
}

func TestNodeNormalizeReverseRequiresBasePath(t *testing.T) {
	n := Node{ID: "artist", Connection: "reverse"}
	if err := n.Normalize(); err == nil {
		t.Fatalf("reverse node without base path should fail")
	}
}

func TestNodeNormalizeInvalidConnection(t *testing.T) {
	n := Node{ID: "artist", Connection: "sideways", URL: "http://127.0.0.1:8080/chat"}
	if err := n.Normalize(); err == nil {
		t.Fatalf("invalid connection should fail")
	}
}

func TestNodeBaseURL(t *testing.T) {
	cases := []struct {
		raw      string
		origin   string
		basePath string
		wantErr  bool
	}{
		{"http://127.0.0.1:8081/chat", "http://127.0.0.1:8081", "/chat", false},
		{"http://127.0.0.1:8081/chat/", "http://127.0.0.1:8081", "/chat", false},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081", "", false},
		{"https://node.example.com/agent/x", "https://node.example.com", "/agent/x", false},
		{"", "", "", true},
		{"ftp://127.0.0.1/chat", "", "", true},
		{"http:///chat", "", "", true},
		{"http://127.0.0.1:8081/chat?x=1", "", "", true},
	}
	for _, tc := range cases {
		origin, basePath, err := ParseNodeURL(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseNodeURL(%q) = (%q,%q), want error", tc.raw, origin, basePath)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseNodeURL(%q) error: %v", tc.raw, err)
			continue
		}
		if origin != tc.origin || basePath != tc.basePath {
			t.Errorf("ParseNodeURL(%q) = (%q,%q), want (%q,%q)", tc.raw, origin, basePath, tc.origin, tc.basePath)
		}
	}
}

func TestValidateID(t *testing.T) {
	for _, ok := range []string{"jarvis", "node-1", "a.b_c", "X9"} {
		if err := ValidateID(ok); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", ok, err)
		}
	}
	bad := []string{"", "-lead", ".lead", "has space", "a/b", "a%2fb", strings.Repeat("x", 65)}
	for _, id := range bad {
		if err := ValidateID(id); err == nil {
			t.Errorf("ValidateID(%q) = nil, want error", id)
		}
	}
}

func TestNormalizeDerivesIDAndName(t *testing.T) {
	n := Node{Name: "My Node!", URL: "http://127.0.0.1:9000/chat"}
	if err := n.Normalize(); err != nil {
		t.Fatal(err)
	}
	if n.ID != "my-node" {
		t.Errorf("ID = %q, want my-node", n.ID)
	}
	if n.URL != "http://127.0.0.1:9000" || n.BasePath != "/chat" {
		t.Errorf("URL/BasePath = %q %q", n.URL, n.BasePath)
	}
	if n.BaseURL() != "http://127.0.0.1:9000/chat" {
		t.Errorf("BaseURL = %q", n.BaseURL())
	}

	n2 := Node{ID: "n2", URL: "http://127.0.0.1:9000/chat"}
	if err := n2.Normalize(); err != nil {
		t.Fatal(err)
	}
	if n2.Name != "n2" {
		t.Errorf("Name = %q, want fallback to ID", n2.Name)
	}
}

func TestNormalizeRejectsRootBasePath(t *testing.T) {
	for _, n := range []Node{
		{ID: "root-url", URL: "http://127.0.0.1:9000"},
		{ID: "slash-base", URL: "http://127.0.0.1:9000/chat", BasePath: "/"},
	} {
		if err := n.Normalize(); err == nil {
			t.Fatalf("Normalize(%+v) = nil, want root base path error", n)
		}
	}
}

func TestNormalizeExplicitBasePathWins(t *testing.T) {
	n := Node{ID: "x", URL: "http://127.0.0.1:9000/chat", BasePath: "/other/"}
	if err := n.Normalize(); err != nil {
		t.Fatal(err)
	}
	if n.BasePath != "/other" {
		t.Errorf("BasePath = %q, want /other", n.BasePath)
	}
}
