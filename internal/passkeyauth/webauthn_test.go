package passkeyauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

func TestRelyingPartyOptionsPolicy(t *testing.T) {
	endpoint, err := ParseEndpoint(EndpointOptions{PublicURL: "https://hub.example.com/hub/", BasePath: "", BasePathExplicit: false})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := NewRelyingParty(RelyingPartyOptions{Endpoint: endpoint, DisplayName: "Test passkey auth"})
	if err != nil {
		t.Fatal(err)
	}
	user := User{ID: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Name: DefaultUserName, Credentials: []Credential{{RecordID: "record", DisplayName: "Key", WebAuthn: webauthn.Credential{ID: []byte{1, 2, 3}, PublicKey: []byte{4}}}}}
	creation, session, err := rp.BeginRegistration(user)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(creation)
	text := string(data)
	for _, want := range []string{`"name":"Test passkey auth"`, `"residentKey":"preferred"`, `"userVerification":"required"`, `"attestation":"none"`, `"excludeCredentials"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("creation options missing %s: %s", want, text)
		}
	}
	if session.RelyingPartyID != "hub.example.com" {
		t.Fatalf("rp=%q", session.RelyingPartyID)
	}
	assertion, loginSession, err := rp.BeginLogin(user)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(assertion)
	text = string(data)
	for _, want := range []string{`"userVerification":"required"`, `"allowCredentials"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("assertion options missing %s: %s", want, text)
		}
	}
	if len(loginSession.AllowedCredentialIDs) != 1 {
		t.Fatalf("allowed=%d", len(loginSession.AllowedCredentialIDs))
	}
}

func localAssertionCredential(t *testing.T) (*ecdsa.PrivateKey, webauthn.Credential) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	return key, webauthn.Credential{ID: []byte("local-credential-id"), PublicKey: publicKey, Flags: webauthn.CredentialFlags{UserPresent: true, UserVerified: true}}
}

func assertionHTTPRequest(t *testing.T, key *ecdsa.PrivateKey, credential webauthn.Credential, session webauthn.SessionData, ceremonyType, origin, rpID string, signCount uint32, uv, validSignature bool) *http.Request {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{"type": ceremonyType, "challenge": session.Challenge, "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(rpID))
	authenticatorData := make([]byte, 37)
	copy(authenticatorData, rpHash[:])
	authenticatorData[32] = byte(protocol.FlagUserPresent)
	if uv {
		authenticatorData[32] |= byte(protocol.FlagUserVerified)
	}
	binary.BigEndian.PutUint32(authenticatorData[33:], signCount)
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authenticatorData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if !validSignature {
		signature[len(signature)-1] ^= 1
	}
	encoded := func(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }
	body, err := json.Marshal(map[string]any{"id": encoded(credential.ID), "rawId": encoded(credential.ID), "type": "public-key", "response": map[string]any{"clientDataJSON": encoded(clientData), "authenticatorData": encoded(authenticatorData), "signature": encoded(signature), "userHandle": nil}, "clientExtensionResults": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "https://backend.invalid/api/auth/login/finish", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func registrationHTTPRequest(t *testing.T, publicKey, credentialID []byte, session webauthn.SessionData, origin, rpID string, uv bool) *http.Request {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": session.Challenge, "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(rpID))
	authenticatorData := make([]byte, 0, 37+16+2+len(credentialID)+len(publicKey))
	authenticatorData = append(authenticatorData, rpHash[:]...)
	flags := byte(protocol.FlagUserPresent | protocol.FlagAttestedCredentialData)
	if uv {
		flags |= byte(protocol.FlagUserVerified)
	}
	authenticatorData = append(authenticatorData, flags, 0, 0, 0, 0)
	authenticatorData = append(authenticatorData, make([]byte, 16)...)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(credentialID)))
	authenticatorData = append(authenticatorData, length...)
	authenticatorData = append(authenticatorData, credentialID...)
	authenticatorData = append(authenticatorData, publicKey...)
	attestationObject, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": authenticatorData, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{"id": encode(credentialID), "rawId": encode(credentialID), "type": "public-key", "response": map[string]any{"clientDataJSON": encode(clientData), "attestationObject": encode(attestationObject), "transports": []string{"internal"}}, "clientExtensionResults": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://backend.invalid/api/auth/bootstrap/register/finish", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestRelyingPartyValidatesLocalRegistration(t *testing.T) {
	endpoint, err := ParseEndpoint(EndpointOptions{PublicURL: "https://hub.example.com/", BasePath: "", BasePathExplicit: false})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := NewRelyingParty(RelyingPartyOptions{Endpoint: endpoint, DisplayName: "Test passkey auth"})
	if err != nil {
		t.Fatal(err)
	}
	key, credential := localAssertionCredential(t)
	_ = key
	user := User{ID: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Name: DefaultUserName}
	_, session, err := rp.BeginRegistration(user)
	if err != nil {
		t.Fatal(err)
	}
	request := registrationHTTPRequest(t, credential.PublicKey, credential.ID, session, endpoint.Origin, endpoint.RPID, true)
	registered, err := rp.FinishRegistration(user, session, request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(registered.ID, credential.ID) || !registered.Flags.UserVerified {
		t.Fatalf("registered=%+v", registered)
	}
}

func TestZeroCounterPolicySurvivesLibraryCounterUpdate(t *testing.T) {
	endpoint, err := ParseEndpoint(EndpointOptions{PublicURL: "https://hub.example.com/", BasePath: "", BasePathExplicit: false})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := NewRelyingParty(RelyingPartyOptions{Endpoint: endpoint, DisplayName: "Test passkey auth"})
	if err != nil {
		t.Fatal(err)
	}
	key, credential := localAssertionCredential(t)
	credential.Authenticator.SignCount = 5
	user := User{ID: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Name: DefaultUserName, Credentials: []Credential{{RecordID: "record", DisplayName: "Key", WebAuthn: credential}}}
	_, session, err := rp.BeginLogin(user)
	if err != nil {
		t.Fatal(err)
	}
	request := assertionHTTPRequest(t, key, credential, session, "webauthn.get", endpoint.Origin, endpoint.RPID, 0, true, true)
	validated, err := rp.FinishLogin(user, session, request)
	if err != nil {
		t.Fatal(err)
	}
	if !validated.Authenticator.CloneWarning || validated.Authenticator.SignCount != 5 {
		t.Fatalf("library behavior changed: %+v", validated.Authenticator)
	}
	warnings := 0
	store, err := OpenStore(StoreOptions{Path: filepath.Join(privateTempDir(t), "auth.json"), RPID: endpoint.RPID, Warnf: func(string, ...any) { warnings++ }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitFirstCredential(credential, "Key"); err != nil {
		t.Fatal(err)
	}
	updated, err := store.UpdateAfterAssertion(validated, 0)
	if err != nil {
		t.Fatal(err)
	}
	if warnings != 0 || updated.WebAuthn.Authenticator.SignCount != 5 || updated.WebAuthn.Authenticator.CloneWarning {
		t.Fatalf("zero counter mishandled: warnings=%d authenticator=%+v", warnings, updated.WebAuthn.Authenticator)
	}
}

func TestRelyingPartyValidatesLocallySignedAssertions(t *testing.T) {
	endpoint, err := ParseEndpoint(EndpointOptions{PublicURL: "https://hub.example.com/hub/", BasePath: "", BasePathExplicit: false})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := NewRelyingParty(RelyingPartyOptions{Endpoint: endpoint, DisplayName: "Test passkey auth"})
	if err != nil {
		t.Fatal(err)
	}
	key, protocolCredential := localAssertionCredential(t)
	user := User{ID: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Name: DefaultUserName, Credentials: []Credential{{RecordID: "record", DisplayName: "Key", WebAuthn: protocolCredential}}}
	tests := []struct {
		name, ceremonyType, origin, rpID           string
		wrongChallenge, uv, validSignature, wantOK bool
	}{
		{"valid", "webauthn.get", endpoint.Origin, endpoint.RPID, false, true, true, true},
		{"wrong challenge", "webauthn.get", endpoint.Origin, endpoint.RPID, true, true, true, false},
		{"wrong type", "webauthn.create", endpoint.Origin, endpoint.RPID, false, true, true, false},
		{"wrong origin", "webauthn.get", "https://evil.example.com", endpoint.RPID, false, true, true, false},
		{"wrong RP ID", "webauthn.get", endpoint.Origin, "evil.example.com", false, true, true, false},
		{"missing UV", "webauthn.get", endpoint.Origin, endpoint.RPID, false, false, true, false},
		{"bad signature", "webauthn.get", endpoint.Origin, endpoint.RPID, false, true, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, session, err := rp.BeginLogin(user)
			if err != nil {
				t.Fatal(err)
			}
			requestSession := session
			if test.wrongChallenge {
				requestSession.Challenge = base64.RawURLEncoding.EncodeToString([]byte("different-challenge-value-32bytes"))
			}
			request := assertionHTTPRequest(t, key, protocolCredential, requestSession, test.ceremonyType, test.origin, test.rpID, 1, test.uv, test.validSignature)
			validated, err := rp.FinishLogin(user, session, request)
			if test.wantOK {
				if err != nil {
					t.Fatalf("valid assertion: %v", err)
				}
				if validated.Authenticator.SignCount != 1 {
					t.Fatalf("sign count=%d", validated.Authenticator.SignCount)
				}
			} else if err == nil {
				t.Fatal("invalid assertion accepted")
			}
		})
	}
}
