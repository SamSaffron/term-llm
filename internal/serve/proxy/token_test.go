package proxy

import (
	"strings"
	"testing"
)

func TestGenerateTokenUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if !strings.HasPrefix(tok, tokenPlaintextPrefix) {
			t.Fatalf("token missing prefix: %q", tok)
		}
		if seen[tok] {
			t.Fatalf("duplicate token generated: %q", tok)
		}
		seen[tok] = true
	}
}

func TestHashTokenDeterministicAndDistinct(t *testing.T) {
	a, _ := GenerateToken()
	b, _ := GenerateToken()
	if HashToken(a) != HashToken(a) {
		t.Fatal("hash not deterministic")
	}
	if HashToken(a) == HashToken(b) {
		t.Fatal("distinct tokens hashed to same value")
	}
	// Whitespace is trimmed before hashing.
	if HashToken(a) != HashToken("  "+a+"  ") {
		t.Fatal("expected surrounding whitespace to be trimmed")
	}
}

func TestTokenDisplayPrefix(t *testing.T) {
	tok, _ := GenerateToken()
	p := TokenDisplayPrefix(tok)
	if len(p) != tokenDisplayPrefixLen {
		t.Fatalf("prefix len = %d, want %d", len(p), tokenDisplayPrefixLen)
	}
	if !strings.HasPrefix(tok, p) {
		t.Fatalf("prefix %q is not a prefix of %q", p, tok)
	}
	// Short inputs are returned as-is.
	if got := TokenDisplayPrefix("abc"); got != "abc" {
		t.Fatalf("short prefix = %q", got)
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("secret", "secret") {
		t.Fatal("expected equal")
	}
	if ConstantTimeEqual("secret", "secreu") {
		t.Fatal("expected not equal")
	}
	if ConstantTimeEqual("secret", "secre") {
		t.Fatal("expected not equal for different lengths")
	}
}
