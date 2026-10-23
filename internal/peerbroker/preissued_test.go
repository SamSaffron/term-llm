package peerbroker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPreissuedSelfHostedTURN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn.json")
	now := time.Now()
	creds := ICECredentials{ExpiresAt: now.Add(20 * time.Minute), ICEServers: []ICEServer{{URLs: []string{"turn:127.0.0.1:3478?transport=udp"}, Username: "expiring-test-user", Credential: "test-only-throwaway"}}}
	write := func() {
		t.Helper()
		b, _ := json.Marshal(creds)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	minter, err := NewPreissuedTURNFile(path, now)
	if err != nil {
		t.Fatal(err)
	}
	out, err := minter.MintICEServers(context.Background(), MinTURNTTL)
	if err != nil || out.ICEServers[0].Username != creds.ICEServers[0].Username {
		t.Fatalf("mint: %+v %v", out, err)
	}
	out.ICEServers[0].URLs[0] = "changed"
	again, _ := minter.MintICEServers(context.Background(), MinTURNTTL)
	if again.ICEServers[0].URLs[0] == "changed" {
		t.Fatal("mutable mint state leaked")
	}
	creds.ExpiresAt = now.Add(time.Hour)
	write()
	if _, err := NewPreissuedTURNFile(path, now); err == nil {
		t.Fatal("accepted long-lived credentials")
	}
	creds.ExpiresAt = now.Add(-time.Second)
	write()
	if _, err := NewPreissuedTURNFile(path, now); err == nil {
		t.Fatal("accepted expired credentials")
	}
	creds.ExpiresAt = now.Add(20 * time.Minute)
	creds.ICEServers[0].URLs = []string{"https://example.invalid"}
	write()
	if _, err := NewPreissuedTURNFile(path, now); err == nil {
		t.Fatal("accepted arbitrary HTTP target")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPreissuedTURNFile(path, now); err == nil {
		t.Fatal("accepted public credential file")
	}
}
