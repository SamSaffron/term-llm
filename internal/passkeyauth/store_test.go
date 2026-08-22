package passkeyauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
)

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testCredential(id byte) webauthn.Credential {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		panic(err)
	}
	return webauthn.Credential{ID: []byte{id}, PublicKey: publicKey, Flags: webauthn.CredentialFlags{UserPresent: true, UserVerified: true}}
}
func TestStoreLifecycleAndStableHandle(t *testing.T) {
	p := filepath.Join(privateTempDir(t), "auth.json")
	now := time.Date(2026, 8, 21, 1, 2, 3, 0, time.UTC)
	const userName = "Example control-plane administrator"
	s, err := OpenStore(StoreOptions{Path: p, RPID: "hub.example.com", UserName: userName, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	id := s.User().ID
	if s.User().Name != userName {
		t.Fatalf("user name = %q", s.User().Name)
	}
	if s.CredentialCount() != 0 {
		t.Fatal("expected empty")
	}
	first, err := s.CommitFirstCredential(testCredential(1), "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AddCredential(testCredential(2), "Recovery key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RenameCredential(second.RecordID, "Phone"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteCredential(second.RecordID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteCredential(first.RecordID); !errors.Is(err, ErrFinalCredential) {
		t.Fatalf("err=%v", err)
	}
	reopened, err := OpenStore(StoreOptions{Path: p, RPID: "hub.example.com", UserName: userName})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.User().ID != id {
		t.Fatal("handle changed")
	}
	renamedConfig, err := OpenStore(StoreOptions{Path: p, RPID: "hub.example.com", UserName: "Different identity"})
	if err != nil {
		t.Fatalf("display-name configuration change rejected: %v", err)
	}
	if renamedConfig.User().Name != userName {
		t.Fatalf("persisted identity name changed unexpectedly: %q", renamedConfig.User().Name)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode=%o", info.Mode().Perm())
		}
	}
}
func TestStoreRejectsCorruptionSymlinkAndMode(t *testing.T) {
	dir := privateTempDir(t)
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreOptions{Path: bad, RPID: "x.example"}); err == nil {
		t.Fatal("expected corrupt error")
	}
	if runtime.GOOS != "windows" {
		target := filepath.Join(dir, "target")
		_ = os.WriteFile(target, []byte("{}"), 0o600)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err == nil {
			if _, err := OpenStore(StoreOptions{Path: link, RPID: "x.example"}); err == nil {
				t.Fatal("expected symlink error")
			}
		}
	}
}
func TestStoreWriteFailurePreservesState(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "auth.json")
	store, err := OpenStore(StoreOptions{Path: path, RPID: "hub.example"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.CommitFirstCredential(testCredential(4), "Original")
	if err != nil {
		t.Fatal(err)
	}
	failing, err := OpenStore(StoreOptions{Path: path, RPID: "hub.example", WriteFile: func(string, []byte, os.FileMode) error { return errors.New("injected write failure") }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.RenameCredential(saved.RecordID, "Changed"); err == nil {
		t.Fatal("expected write failure")
	}
	if got := failing.Credentials()[0].DisplayName; got != "Original" {
		t.Fatalf("in-memory state changed to %q", got)
	}
	reopened, err := OpenStore(StoreOptions{Path: path, RPID: "hub.example"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Credentials()[0].DisplayName; got != "Original" {
		t.Fatalf("on-disk state changed to %q", got)
	}
}

func TestStoreDoesNotChmodExistingParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission policy")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreOptions{Path: filepath.Join(parent, "auth.json"), RPID: "hub.example"}); err == nil {
		t.Fatal("accepted insecure existing parent")
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("parent mode changed to %04o", info.Mode().Perm())
	}
}

func TestStoreRejectsUnknownVersionAndInsecureMode(t *testing.T) {
	p := filepath.Join(privateTempDir(t), "auth.json")
	if _, err := OpenStore(StoreOptions{Path: p, RPID: "hub.example"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"version": 1`, `"version": 2`, 1))
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreOptions{Path: p, RPID: "hub.example"}); err == nil {
		t.Fatal("accepted unknown version")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStore(StoreOptions{Path: p, RPID: "hub.example"}); err == nil {
			t.Fatal("accepted insecure mode")
		}
	}
}

func TestStoreApplicationOwnedSchemaAndCorruptCredential(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "auth.json")
	store, err := OpenStore(StoreOptions{Path: path, RPID: "hub.example"})
	if err != nil {
		t.Fatal(err)
	}
	credential := testCredential(9)
	if _, err := store.CommitFirstCredential(credential, "Key"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddCredential(credential, "Duplicate"); !errors.Is(err, ErrDuplicateCredential) {
		t.Fatalf("duplicate err=%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"webauthn"`) || strings.Contains(string(raw), `"administrator"`) {
		t.Fatal("implementation-specific fields leaked into reusable schema")
	}
	for _, field := range []string{`"user"`, `"public_key"`, `"sign_count"`, `"backup_eligible"`, `"attestation"`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("schema missing %s", field)
		}
	}
	unknown := []byte(strings.Replace(string(raw), `"record_id":`, `"unexpected":true,"record_id":`, 1))
	if err := os.WriteFile(path, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreOptions{Path: path, RPID: "hub.example"}); err == nil {
		t.Fatal("accepted unknown credential field")
	}
	var persisted storeFile
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	persisted.User.Credentials[0].WebAuthn.PublicKey = []byte{1, 2, 3}
	corrupt, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreOptions{Path: path, RPID: "hub.example"}); err == nil {
		t.Fatal("accepted corrupt COSE public key")
	}
}

func TestStoreCounterPolicy(t *testing.T) {
	warnings := 0
	s, err := OpenStore(StoreOptions{Path: filepath.Join(privateTempDir(t), "auth.json"), RPID: "hub.example", Warnf: func(string, ...any) { warnings++ }})
	if err != nil {
		t.Fatal(err)
	}
	raw := testCredential(7)
	raw.Authenticator.SignCount = 5
	raw.Flags.BackupEligible = true
	saved, err := s.CommitFirstCredential(raw, "key")
	if err != nil {
		t.Fatal(err)
	}
	validated := saved.WebAuthn
	validated.Authenticator.SignCount = 4
	validated.Flags.BackupEligible = true
	validated.Flags.BackupState = true
	updated, err := s.UpdateAfterAssertion(validated)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WebAuthn.Authenticator.SignCount != 5 || !updated.WebAuthn.Authenticator.CloneWarning || warnings != 1 {
		t.Fatalf("count=%d clone=%v warnings=%d", updated.WebAuthn.Authenticator.SignCount, updated.WebAuthn.Authenticator.CloneWarning, warnings)
	}
	validated = updated.WebAuthn
	validated.Authenticator.SignCount = 0
	if zeroUpdated, err := s.UpdateAfterAssertion(validated); err != nil {
		t.Fatal(err)
	} else if warnings != 1 || zeroUpdated.WebAuthn.Authenticator.SignCount != 5 {
		t.Fatalf("zero counter lost evidence or warned: count=%d warnings=%d", zeroUpdated.WebAuthn.Authenticator.SignCount, warnings)
	} else {
		inconsistent := zeroUpdated.WebAuthn
		inconsistent.Flags.BackupEligible = false
		if _, err := s.UpdateAfterAssertion(inconsistent); err == nil {
			t.Fatal("accepted changed backup eligibility")
		}
	}
}

func TestConcurrentFirstCredentialOneWinner(t *testing.T) {
	s, err := OpenStore(StoreOptions{Path: filepath.Join(privateTempDir(t), "auth.json"), RPID: "hub.example"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wins := 0
	var mu sync.Mutex
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CommitFirstCredential(testCredential(byte(i+1)), "key")
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrFirstCredentialExists) {
				t.Errorf("err=%v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || s.CredentialCount() != 1 {
		t.Fatalf("wins=%d count=%d", wins, s.CredentialCount())
	}
}

func FuzzStoreDecoding(f *testing.F) {
	for _, seed := range [][]byte{[]byte(`{}`), []byte(`{"version":1}`), []byte(`{"version":999,"user":{}}`), {0xff, 0x00}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := privateTempDir(t)
		path := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = OpenStore(StoreOptions{Path: path, RPID: "hub.example"})
	})
}
