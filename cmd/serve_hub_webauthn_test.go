package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
)

func hubTestAssertionCredential(t *testing.T) (*ecdsa.PrivateKey, webauthn.Credential) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	return key, webauthn.Credential{ID: []byte("cmd-local-credential"), PublicKey: publicKey, Flags: webauthn.CredentialFlags{UserPresent: true, UserVerified: true}}
}

func hubTestAssertionBody(t *testing.T, key *ecdsa.PrivateKey, credential webauthn.Credential, challenge, origin, rpID string) string {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(rpID))
	authData := make([]byte, 37)
	copy(authData, rpHash[:])
	authData[32] = byte(protocol.FlagUserPresent | protocol.FlagUserVerified)
	binary.BigEndian.PutUint32(authData[33:], 1)
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{"id": encode(credential.ID), "rawId": encode(credential.ID), "type": "public-key", "response": map[string]any{"clientDataJSON": encode(clientData), "authenticatorData": encode(authData), "signature": encode(signature), "userHandle": nil}, "clientExtensionResults": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func beginHubTestLogin(t *testing.T, s *hubServer, remote string) (string, *http.Cookie) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/begin", strings.NewReader(`{"return_path":"/"}`))
	request.RemoteAddr = remote
	request.Header.Set("Origin", s.passkey.endpoint.Origin)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("begin=%d %s", response.Code, response.Body.String())
	}
	var options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	var ceremony *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == hubLoginCeremonyCookieName {
			ceremony = cookie
		}
	}
	if ceremony == nil || options.PublicKey.Challenge == "" {
		t.Fatalf("begin response missing state: cookies=%v body=%s", response.Result().Cookies(), response.Body.String())
	}
	return options.PublicKey.Challenge, ceremony
}

func hubTestRegistrationBody(t *testing.T, credential webauthn.Credential, challenge, origin, rpID string) string {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": challenge, "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(rpID))
	authData := make([]byte, 0, 37+16+2+len(credential.ID)+len(credential.PublicKey))
	authData = append(authData, rpHash[:]...)
	authData = append(authData, byte(protocol.FlagUserPresent|protocol.FlagUserVerified|protocol.FlagAttestedCredentialData), 0, 0, 0, 0)
	authData = append(authData, make([]byte, 16)...)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(credential.ID)))
	authData = append(authData, length...)
	authData = append(authData, credential.ID...)
	authData = append(authData, credential.PublicKey...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": authData, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{"id": encode(credential.ID), "rawId": encode(credential.ID), "type": "public-key", "response": map[string]any{"clientDataJSON": encode(clientData), "attestationObject": encode(attestation), "transports": []string{"internal"}}, "clientExtensionResults": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func beginHubTestBootstrapRegistration(t *testing.T, s *hubServer, grant *http.Cookie, name, remote string) (string, *http.Cookie) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/register/begin", strings.NewReader(`{"display_name":"`+name+`"}`))
	request.RemoteAddr = remote
	request.Header.Set("Origin", s.passkey.endpoint.Origin)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(grant)
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("registration begin=%d %s", response.Code, response.Body.String())
	}
	var options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	var ceremony *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == hubLoginCeremonyCookieName {
			ceremony = cookie
		}
	}
	if ceremony == nil {
		return "", nil
	}
	return options.PublicKey.Challenge, ceremony
}

func TestHubPasskeyConcurrentBootstrapFinishHasOneWinner(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	verify := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/verify", strings.NewReader(`{"code":"abcdefghijklmnopqrst"}`))
	verify.RemoteAddr = "192.0.2.9:1"
	verify.Header.Set("Origin", s.passkey.endpoint.Origin)
	verify.Header.Set("Content-Type", "application/json")
	verified := httptest.NewRecorder()
	s.handler().ServeHTTP(verified, verify)
	if verified.Code != http.StatusOK {
		t.Fatalf("verify=%d %s", verified.Code, verified.Body.String())
	}
	grant := verified.Result().Cookies()[0]
	challengeA, ceremonyA := beginHubTestBootstrapRegistration(t, s, grant, "A", "192.0.2.9:1")
	challengeB, ceremonyB := beginHubTestBootstrapRegistration(t, s, grant, "B", "192.0.2.9:1")
	_, credentialA := hubTestAssertionCredential(t)
	credentialA.ID = []byte("bootstrap-credential-a")
	_, credentialB := hubTestAssertionCredential(t)
	credentialB.ID = []byte("bootstrap-credential-b")
	bodies := []string{hubTestRegistrationBody(t, credentialA, challengeA, s.passkey.endpoint.Origin, s.passkey.endpoint.RPID), hubTestRegistrationBody(t, credentialB, challengeB, s.passkey.endpoint.Origin, s.passkey.endpoint.RPID)}
	cookies := []*http.Cookie{ceremonyA, ceremonyB}
	statuses := make(chan int, 2)
	for i := range bodies {
		go func(i int) {
			request := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/bootstrap/register/finish", strings.NewReader(bodies[i]))
			request.RemoteAddr = "192.0.2.9:1"
			request.Header.Set("Origin", s.passkey.endpoint.Origin)
			request.Header.Set("Content-Type", "application/json")
			request.AddCookie(grant)
			request.AddCookie(cookies[i])
			response := httptest.NewRecorder()
			s.handler().ServeHTTP(response, request)
			statuses <- response.Code
		}(i)
	}
	first, second := <-statuses, <-statuses
	if !((first == http.StatusCreated && second == http.StatusConflict) || (second == http.StatusCreated && first == http.StatusConflict)) {
		t.Fatalf("statuses=%d,%d", first, second)
	}
	if count := s.passkey.store.CredentialCount(); count != 1 {
		t.Fatalf("credential count=%d", count)
	}
}

func TestHubPasskeySignedLoginReplayOriginAndBinding(t *testing.T) {
	var logs strings.Builder
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })
	s := newTestPasskeyHub(t, "")
	key, credential := hubTestAssertionCredential(t)
	saved, err := s.passkey.store.CommitFirstCredential(credential, "Local key")
	if err != nil {
		t.Fatal(err)
	}
	challenge, ceremony := beginHubTestLogin(t, s, "192.0.2.1:1")
	body := hubTestAssertionBody(t, key, credential, challenge, s.passkey.endpoint.Origin, s.passkey.endpoint.RPID)
	wrongOrigin := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/finish", strings.NewReader(body))
	wrongOrigin.RemoteAddr = "192.0.2.1:1"
	wrongOrigin.Header.Set("Origin", "https://evil.example.com")
	wrongOrigin.Header.Set("Content-Type", "application/json")
	wrongOrigin.AddCookie(ceremony)
	wrongResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(wrongResponse, wrongOrigin)
	if wrongResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong origin=%d", wrongResponse.Code)
	}
	finish := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/finish", strings.NewReader(body))
	finish.RemoteAddr = "192.0.2.1:1"
	finish.Header.Set("Origin", s.passkey.endpoint.Origin)
	finish.Header.Set("Content-Type", "application/json")
	finish.AddCookie(ceremony)
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, finish)
	if response.Code != http.StatusOK {
		t.Fatalf("finish=%d %s", response.Code, response.Body.String())
	}
	var session *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == hubSessionCookieName {
			session = cookie
		}
	}
	if session == nil || session.Value == "" || session.HttpOnly != true || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie=%+v", session)
	}
	if strings.Contains(session.Value, saved.RecordID) {
		t.Fatal("session token is not opaque")
	}
	for _, secret := range []string{challenge, body, session.Value, base64.RawURLEncoding.EncodeToString(credential.ID)} {
		if strings.Contains(logs.String(), secret) || strings.Contains(response.Body.String(), secret) || strings.Contains(finish.URL.String(), secret) {
			t.Fatalf("authentication secret leaked outside protocol response/cookie: %q", secret)
		}
	}
	replay := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/finish", strings.NewReader(body))
	replay.RemoteAddr = "192.0.2.1:1"
	replay.Header.Set("Origin", s.passkey.endpoint.Origin)
	replay.Header.Set("Content-Type", "application/json")
	replay.AddCookie(ceremony)
	replayResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusUnauthorized {
		t.Fatalf("replay=%d %s", replayResponse.Code, replayResponse.Body.String())
	}
	s.passkey.limiter = newHubAuthLimiter(nil)
	challenge, ceremony = beginHubTestLogin(t, s, "198.51.100.1:1")
	body = hubTestAssertionBody(t, key, credential, challenge, s.passkey.endpoint.Origin, s.passkey.endpoint.RPID)
	wrongCookie := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/finish", strings.NewReader(body))
	wrongCookie.RemoteAddr = "198.51.100.1:1"
	wrongCookie.Header.Set("Origin", s.passkey.endpoint.Origin)
	wrongCookie.Header.Set("Content-Type", "application/json")
	wrongCookie.AddCookie(&http.Cookie{Name: hubLoginCeremonyCookieName, Value: "different-cookie"})
	wrongCookieResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(wrongCookieResponse, wrongCookie)
	if wrongCookieResponse.Code != http.StatusUnauthorized {
		t.Fatalf("wrong binding=%d", wrongCookieResponse.Code)
	}
}

func TestHubPasskeyExpiredCeremony(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	_, credential := hubTestAssertionCredential(t)
	if _, err := s.passkey.store.CommitFirstCredential(credential, "Local key"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.passkey.ceremonies = passkeyauth.NewCeremonies(func() time.Time { return now }, nil)
	_, ceremony := beginHubTestLogin(t, s, "203.0.113.1:1")
	now = now.Add(passkeyauth.CeremonyLifetime)
	request := httptest.NewRequest(http.MethodPost, "http://backend/api/auth/login/finish", strings.NewReader(`{}`))
	request.RemoteAddr = "203.0.113.1:1"
	request.Header.Set("Origin", s.passkey.endpoint.Origin)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(ceremony)
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expired ceremony=%d %s", response.Code, response.Body.String())
	}
}
