package pairing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Identity is a persistent Ed25519 host or device identity.
type Identity struct {
	priv ed25519.PrivateKey
}

type identityFile struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	Seed B64    `json:"seed"`
}

// GenerateIdentity creates a fresh in-memory identity.
func GenerateIdentity() (*Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	return &Identity{priv: priv}, nil
}

// IdentityFromSeed builds an identity from a 32-byte Ed25519 seed.
func IdentityFromSeed(seed []byte) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: identity seed must be %d bytes", ErrInvalid, ed25519.SeedSize)
	}
	return &Identity{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// LoadOrCreateIdentity loads the identity stored at path, creating it with
// mode 0600 (parent directory 0700) when absent. Files readable by group or
// others are refused.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	var id *Identity
	err := withFileLock(path, func() error {
		data, err := readPrivateFile(path)
		if err == nil {
			var f identityFile
			if err := json.Unmarshal(data, &f); err != nil {
				return fmt.Errorf("decode identity %s: %w", path, err)
			}
			if f.V != StateVersion || f.Type != "ed25519" {
				return fmt.Errorf("identity %s: unsupported format", path)
			}
			id, err = IdentityFromSeed(f.Seed)
			return err
		}
		if !isNotExist(err) {
			return fmt.Errorf("load identity: %w", err)
		}
		id, err = GenerateIdentity()
		if err != nil {
			return err
		}
		data, err = json.Marshal(identityFile{V: StateVersion, Type: "ed25519", Seed: id.priv.Seed()})
		if err != nil {
			return err
		}
		return writePrivateFile(path, data)
	})
	if err != nil {
		return nil, err
	}
	return id, nil
}

// PublicKey returns the identity's public key.
func (i *Identity) PublicKey() ed25519.PublicKey {
	return i.priv.Public().(ed25519.PublicKey)
}

// Fingerprint returns a short human-comparable fingerprint of the public key.
func (i *Identity) Fingerprint() string { return KeyFingerprint(i.PublicKey()) }

// Sign returns an Ed25519 signature over msg.
func (i *Identity) Sign(msg []byte) []byte { return ed25519.Sign(i.priv, msg) }

// VerifySignature verifies an Ed25519 wire signature.
func VerifySignature(pub, msg, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// KeyFingerprint formats the first 10 bytes of SHA-256(pub) as five groups of
// four hex digits, e.g. "1a2b-3c4d-5e6f-7a8b-9c0d".
func KeyFingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	h := hex.EncodeToString(sum[:10])
	groups := make([]string, 0, 5)
	for i := 0; i < len(h); i += 4 {
		groups = append(groups, h[i:i+4])
	}
	return strings.Join(groups, "-")
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random: %w", err)
	}
	return b, nil
}
