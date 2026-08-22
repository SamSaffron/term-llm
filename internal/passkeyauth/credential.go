package passkeyauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	StoreVersion          = 1
	DefaultUserName       = "Passkey user"
	DefaultCredentialName = "Primary passkey"
)

// User is the single logical WebAuthn identity managed by a Store.
type User struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Credentials []Credential `json:"credentials"`
}

// Credential wraps protocol state in an application-owned, versioned on-disk
// representation. WebAuthn is deliberately excluded from default JSON encoding;
// MarshalJSON and UnmarshalJSON below enumerate every persisted protocol field.
type Credential struct {
	RecordID    string              `json:"-"`
	DisplayName string              `json:"-"`
	WebAuthn    webauthn.Credential `json:"-"`
	CreatedAt   time.Time           `json:"-"`
	LastUsedAt  time.Time           `json:"-"`
}

type credentialFlagsJSON struct {
	UserPresent    bool `json:"user_present"`
	UserVerified   bool `json:"user_verified"`
	BackupEligible bool `json:"backup_eligible"`
	BackupState    bool `json:"backup_state"`
}

type credentialAuthenticatorJSON struct {
	AAGUID       string                           `json:"aaguid,omitempty"`
	SignCount    uint32                           `json:"sign_count"`
	CloneWarning bool                             `json:"clone_warning,omitempty"`
	Attachment   protocol.AuthenticatorAttachment `json:"attachment,omitempty"`
}

type credentialAttestationJSON struct {
	ClientDataJSON     string `json:"client_data_json,omitempty"`
	ClientDataHash     string `json:"client_data_hash,omitempty"`
	AuthenticatorData  string `json:"authenticator_data,omitempty"`
	PublicKeyAlgorithm int64  `json:"public_key_algorithm,omitempty"`
	Object             string `json:"object,omitempty"`
}

type credentialJSON struct {
	RecordID          string                            `json:"record_id"`
	ID                string                            `json:"id"`
	DisplayName       string                            `json:"display_name"`
	PublicKey         string                            `json:"public_key"`
	AttestationType   string                            `json:"attestation_type,omitempty"`
	AttestationFormat string                            `json:"attestation_format,omitempty"`
	Transports        []protocol.AuthenticatorTransport `json:"transports,omitempty"`
	Flags             credentialFlagsJSON               `json:"flags"`
	Authenticator     credentialAuthenticatorJSON       `json:"authenticator"`
	Attestation       credentialAttestationJSON         `json:"attestation"`
	CreatedAt         time.Time                         `json:"created_at"`
	LastUsedAt        time.Time                         `json:"last_used_at"`
}

func encodeCredentialBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeCredentialBytes(name, value string, required bool) ([]byte, error) {
	if value == "" {
		if required {
			return nil, fmt.Errorf("credential %s is required", name)
		}
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("credential %s is not valid base64url: %w", name, err)
	}
	return decoded, nil
}

func (c Credential) MarshalJSON() ([]byte, error) {
	value := credentialJSON{
		RecordID: c.RecordID, ID: encodeCredentialBytes(c.WebAuthn.ID), DisplayName: c.DisplayName,
		PublicKey: encodeCredentialBytes(c.WebAuthn.PublicKey), AttestationType: c.WebAuthn.AttestationType,
		AttestationFormat: c.WebAuthn.AttestationFormat, Transports: append([]protocol.AuthenticatorTransport(nil), c.WebAuthn.Transport...),
		Flags:         credentialFlagsJSON{c.WebAuthn.Flags.UserPresent, c.WebAuthn.Flags.UserVerified, c.WebAuthn.Flags.BackupEligible, c.WebAuthn.Flags.BackupState},
		Authenticator: credentialAuthenticatorJSON{encodeCredentialBytes(c.WebAuthn.Authenticator.AAGUID), c.WebAuthn.Authenticator.SignCount, c.WebAuthn.Authenticator.CloneWarning, c.WebAuthn.Authenticator.Attachment},
		Attestation:   credentialAttestationJSON{encodeCredentialBytes(c.WebAuthn.Attestation.ClientDataJSON), encodeCredentialBytes(c.WebAuthn.Attestation.ClientDataHash), encodeCredentialBytes(c.WebAuthn.Attestation.AuthenticatorData), c.WebAuthn.Attestation.PublicKeyAlgorithm, encodeCredentialBytes(c.WebAuthn.Attestation.Object)},
		CreatedAt:     c.CreatedAt, LastUsedAt: c.LastUsedAt,
	}
	return json.Marshal(value)
}

func (c *Credential) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value credentialJSON
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing credential JSON data")
	}
	id, err := decodeCredentialBytes("id", value.ID, true)
	if err != nil {
		return err
	}
	publicKey, err := decodeCredentialBytes("public_key", value.PublicKey, true)
	if err != nil {
		return err
	}
	aaguid, err := decodeCredentialBytes("aaguid", value.Authenticator.AAGUID, false)
	if err != nil {
		return err
	}
	clientDataJSON, err := decodeCredentialBytes("client_data_json", value.Attestation.ClientDataJSON, false)
	if err != nil {
		return err
	}
	clientDataHash, err := decodeCredentialBytes("client_data_hash", value.Attestation.ClientDataHash, false)
	if err != nil {
		return err
	}
	authenticatorData, err := decodeCredentialBytes("authenticator_data", value.Attestation.AuthenticatorData, false)
	if err != nil {
		return err
	}
	object, err := decodeCredentialBytes("object", value.Attestation.Object, false)
	if err != nil {
		return err
	}
	*c = Credential{
		RecordID: value.RecordID, DisplayName: value.DisplayName, CreatedAt: value.CreatedAt, LastUsedAt: value.LastUsedAt,
		WebAuthn: webauthn.Credential{
			ID: id, PublicKey: publicKey, AttestationType: value.AttestationType, AttestationFormat: value.AttestationFormat,
			Transport:     append([]protocol.AuthenticatorTransport(nil), value.Transports...),
			Flags:         webauthn.CredentialFlags{UserPresent: value.Flags.UserPresent, UserVerified: value.Flags.UserVerified, BackupEligible: value.Flags.BackupEligible, BackupState: value.Flags.BackupState},
			Authenticator: webauthn.Authenticator{AAGUID: aaguid, SignCount: value.Authenticator.SignCount, CloneWarning: value.Authenticator.CloneWarning, Attachment: value.Authenticator.Attachment},
			Attestation:   webauthn.CredentialAttestation{ClientDataJSON: clientDataJSON, ClientDataHash: clientDataHash, AuthenticatorData: authenticatorData, PublicKeyAlgorithm: value.Attestation.PublicKeyAlgorithm, Object: object},
		},
	}
	return nil
}

func (a User) WebAuthnID() []byte {
	id, _ := base64.RawURLEncoding.DecodeString(a.ID)
	return id
}
func (a User) WebAuthnName() string        { return a.Name }
func (a User) WebAuthnDisplayName() string { return a.Name }
func (a User) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, len(a.Credentials))
	for i := range a.Credentials {
		out[i] = cloneProtocolCredential(a.Credentials[i].WebAuthn)
	}
	return out
}

func normalizeUserName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || len([]rune(name)) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return "", fmt.Errorf("passkey user name must be 1-80 valid UTF-8 characters on one line")
	}
	return name, nil
}

func NormalizeCredentialName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultCredentialName
	}
	if len([]rune(name)) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return "", fmt.Errorf("credential name must be 1-80 characters on one line")
	}
	return name, nil
}

func cloneCredential(c Credential) Credential {
	c.WebAuthn = cloneProtocolCredential(c.WebAuthn)
	return c
}

func cloneProtocolCredential(c webauthn.Credential) webauthn.Credential {
	c.ID = append([]byte(nil), c.ID...)
	c.PublicKey = append([]byte(nil), c.PublicKey...)
	c.Transport = append([]protocol.AuthenticatorTransport(nil), c.Transport...)
	c.Authenticator.AAGUID = append([]byte(nil), c.Authenticator.AAGUID...)
	c.Attestation.ClientDataJSON = append([]byte(nil), c.Attestation.ClientDataJSON...)
	c.Attestation.ClientDataHash = append([]byte(nil), c.Attestation.ClientDataHash...)
	c.Attestation.AuthenticatorData = append([]byte(nil), c.Attestation.AuthenticatorData...)
	c.Attestation.Object = append([]byte(nil), c.Attestation.Object...)
	return c
}
