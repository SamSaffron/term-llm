package serve

import (
	"encoding/base64"
	"testing"
)

func TestGenerateTokenKeepsURLSafe32ByteFormat(t *testing.T) {
	token, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 || len(token) != 43 {
		t.Fatalf("token format changed: raw bytes=%d encoded bytes=%d err=%v", len(raw), len(token), err)
	}
}
