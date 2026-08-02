package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/gateway/protocol"
)

type sealedProviderState struct {
	Version   int    `json:"version"`
	ClientID  string `json:"client_id"`
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
	State     []byte `json:"state"`
}

type StateSealer struct{ aead cipher.AEAD }

func OpenStateSealer(path string) (*StateSealer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read gateway state key: %w", err)
		}
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("generate gateway state key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, fmt.Errorf("write gateway state key: %w", err)
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("gateway state key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &StateSealer{aead: aead}, nil
}

func (s *StateSealer) Seal(clientID, provider, sessionID string, state []byte) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("cannot seal provider state without a session ID")
	}
	plain, err := json.Marshal(sealedProviderState{
		Version: protocol.Version, ClientID: clientID, Provider: provider,
		SessionID: sessionID, State: state,
	})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := s.aead.Seal(nil, nonce, plain, []byte("term-llm-gateway-state-v1"))
	return base64.RawURLEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func (s *StateSealer) Open(blob, clientID, provider, sessionID string) ([]byte, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("cannot open provider state without a session ID")
	}
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return nil, fmt.Errorf("invalid sealed provider state")
	}
	nonce, ciphertext := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, ciphertext, []byte("term-llm-gateway-state-v1"))
	if err != nil {
		return nil, fmt.Errorf("invalid sealed provider state")
	}
	var state sealedProviderState
	if err := json.Unmarshal(plain, &state); err != nil ||
		state.Version != protocol.Version || state.ClientID != clientID ||
		state.Provider != provider || state.SessionID != sessionID {
		return nil, fmt.Errorf("sealed provider state does not belong to this client/provider/session")
	}
	return append([]byte(nil), state.State...), nil
}
