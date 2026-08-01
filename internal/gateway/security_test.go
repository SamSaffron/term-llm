package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestClientStoreHashesAuthenticatesPoliciesAndRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.json")
	store, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{AllowProviders: []string{"openai"}, DenyModels: []string{"danger*"}, AllowSearch: true, AllowFetch: true, MaxConcurrentInference: 1, SearchRatePerMinute: 7, MaxConcurrentSearch: 1, FetchRatePerMinute: 9, MaxConcurrentFetch: 1}
	client, token, err := store.Add("satellite-a", policy)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) == "" || strings.Contains(string(data), token) {
		t.Fatal("plaintext client token was persisted")
	}
	got, ok := store.Authenticate(token)
	if !ok || got.ID != client.ID {
		t.Fatalf("authentication failed: %+v %t", got, ok)
	}
	if !client.Policy.Allows("openai", "gpt", false) || client.Policy.Allows("anthropic", "claude", false) || client.Policy.Allows("openai", "danger-model", false) || client.Policy.Allows("openai", "gpt", true) {
		t.Fatal("client provider/model/CLI policy was not enforced")
	}
	reopened, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reopened.Authenticate(token)
	if !ok || persisted.Policy.MaxConcurrentInference != 1 || persisted.Policy.SearchRatePerMinute != 7 || persisted.Policy.MaxConcurrentSearch != 1 || persisted.Policy.FetchRatePerMinute != 9 || persisted.Policy.MaxConcurrentFetch != 1 {
		t.Fatalf("persisted client limits = %+v, authenticated=%t", persisted.Policy, ok)
	}
	if err := store.Revoke(client.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Authenticate(token); ok {
		t.Fatal("revoked token authenticated")
	}
}

func TestClientStoreEnforcesUniqueActiveNamesAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.json")
	store, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	original, originalToken, err := store.Add("satellite-a", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add("satellite-a", Policy{}); err == nil || !strings.Contains(err.Error(), "revoke it before rotating") {
		t.Fatalf("duplicate active name error = %v", err)
	}
	if authenticated, ok := store.Authenticate(originalToken); !ok || authenticated.ID != original.ID {
		t.Fatalf("duplicate rejection disturbed original credential: %+v authenticated=%t", authenticated, ok)
	}
	if err := store.Revoke("satellite-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Authenticate(originalToken); ok {
		t.Fatal("name-based revocation did not immediately disable original token")
	}
	rotated, rotatedToken, err := store.Add("satellite-a", Policy{})
	if err != nil {
		t.Fatalf("add after revoke rotation: %v", err)
	}
	if rotated.ID == original.ID {
		t.Fatal("rotation reused client identity")
	}
	if authenticated, ok := store.Authenticate(rotatedToken); !ok || authenticated.ID != rotated.ID {
		t.Fatalf("rotated credential did not authenticate immediately: %+v authenticated=%t", authenticated, ok)
	}
}

func TestClientStoreRevokeReloadsAuthoritativeMultiStoreWithoutOverwritingAdds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.json")
	adminA, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	victim, victimToken, err := adminA.Add("victim", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	staleAdmin, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, survivorToken, err := adminA.Add("survivor", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := staleAdmin.Revoke(victim.ID); err != nil {
		t.Fatal(err)
	}
	authoritative, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := authoritative.Authenticate(victimToken); ok {
		t.Fatal("revoked token remained active")
	}
	if survivor, ok := authoritative.Authenticate(survivorToken); !ok || survivor.Name != "survivor" {
		t.Fatalf("stale-store revoke overwrote concurrent addition: %+v authenticated=%t", survivor, ok)
	}
}

func TestClientStoreConcurrentMultiStoreAddsAreSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.json")
	stores := make([]*ClientStore, 2)
	for i := range stores {
		var err error
		stores[i], err = OpenClientStore(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	tokens := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, tokens[i], errs[i] = stores[i].Add(fmt.Sprintf("concurrent-%d", i), Policy{})
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	authoritative, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, token := range tokens {
		client, ok := authoritative.Authenticate(token)
		if !ok || client.Name != fmt.Sprintf("concurrent-%d", i) {
			t.Fatalf("concurrent client %d = %+v authenticated=%t", i, client, ok)
		}
	}
}

func TestRunningGatewayObservesAddAndRevokeFromSecondStoreImmediately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clients.json")
	runtimeStore, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	adminStore, err := OpenClientStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := OpenStateSealer(filepath.Join(dir, "state.key"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: runtimeStore, Sealer: sealer})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client, token, err := adminStore.Add("live-admin-change", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	request := func() int {
		req, reqErr := http.NewRequest(http.MethodGet, ts.URL+"/g1/catalog", nil)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(protocol.VersionHeader, "1")
		resp, reqErr := http.DefaultClient.Do(req)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := request(); status != http.StatusOK {
		t.Fatalf("new client status = %d, want 200", status)
	}
	started := time.Now()
	if err := adminStore.Revoke(client.ID); err != nil {
		t.Fatal(err)
	}
	if status := request(); status != http.StatusUnauthorized {
		t.Fatalf("revoked client status = %d, want 401", status)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("revocation observation took %s; authentication has no polling interval", elapsed)
	}
}

func TestGatewayRunTempRootScavengesOnlyOwnedPrefixDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gateway-runs")
	if err := os.MkdirAll(filepath.Join(root, "run-stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "operator-content"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run-not-a-directory"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := OpenClientStore(filepath.Join(t.TempDir(), "clients.json"))
	sealer, _ := OpenStateSealer(filepath.Join(t.TempDir(), "state.key"))
	if _, err := NewServer(ServerConfig{Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: store, Sealer: sealer, RunTempRoot: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "run-stale")); !os.IsNotExist(err) {
		t.Fatalf("stale gateway run directory remains: %v", err)
	}
	for _, name := range []string{"operator-content", "run-not-a-directory"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("safe scavenging removed %s: %v", name, err)
		}
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("run temp root mode = %v, %v", info, err)
	}
}

func TestStateSealerRoundTripTamperAndCrossClient(t *testing.T) {
	sealer, err := OpenStateSealer(filepath.Join(t.TempDir(), "state.key"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := sealer.Seal("client-a", "claude-bin", []byte("gateway-local-state"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sealer.Open(blob, "client-a", "claude-bin")
	if err != nil || string(plain) != "gateway-local-state" {
		t.Fatalf("round trip = %q, %v", plain, err)
	}
	tampered := []byte(blob)
	if tampered[len(tampered)/2] == 'A' {
		tampered[len(tampered)/2] = 'B'
	} else {
		tampered[len(tampered)/2] = 'A'
	}
	for _, tc := range []struct{ blob, client, provider string }{
		{string(tampered), "client-a", "claude-bin"},
		{blob, "client-b", "claude-bin"},
		{blob, "client-a", "grok-bin"},
	} {
		if _, err := sealer.Open(tc.blob, tc.client, tc.provider); err == nil {
			t.Fatalf("accepted tampered/foreign state: %+v", tc)
		}
	}
}

func TestEnrollmentCreatesUniqueAuthenticatedClient(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenClientStore(filepath.Join(dir, "clients.json"))
	policy := Policy{AllowProviders: []string{"openai"}, AllowSearch: true, MaxConcurrentInference: 1}
	enrollment, bootstrap, err := store.CreateEnrollment("new-satellite", policy, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if enrollment.ExpiresAt.Sub(enrollment.CreatedAt) != time.Minute {
		t.Fatalf("enrollment TTL = %s", enrollment.ExpiresAt.Sub(enrollment.CreatedAt))
	}
	sealer, _ := OpenStateSealer(filepath.Join(dir, "state.key"))
	server, err := NewServer(ServerConfig{Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: store, Sealer: sealer, Policy: Policy{}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(protocol.EnrollmentRequest{Version: protocol.Version, Name: "new-satellite"})
	enroll := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/g1/enroll", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bootstrap)
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, req)
		return rr
	}
	unsupportedPayload, _ := json.Marshal(protocol.EnrollmentRequest{Version: protocol.Version + 1, Name: "new-satellite"})
	unsupported := enroll(unsupportedPayload)
	var versionError protocol.Error
	if err := json.Unmarshal(unsupported.Body.Bytes(), &versionError); err != nil {
		t.Fatal(err)
	}
	if unsupported.Code != http.StatusUpgradeRequired || unsupported.Header().Get(protocol.VersionHeader) != "1" || versionError.Code != "unsupported_version" || len(versionError.SupportedVersions) != 1 || versionError.SupportedVersions[0] != protocol.Version {
		t.Fatalf("unsupported enrollment version = %d headers=%v body=%+v", unsupported.Code, unsupported.Header(), versionError)
	}
	rr := enroll(payload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enrollment status = %d: %s", rr.Code, rr.Body.String())
	}
	var enrolled protocol.EnrollmentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	client, ok := store.Authenticate(enrolled.Token)
	if !ok || client.ID != enrolled.ClientID || client.Name != "new-satellite" || len(client.Policy.AllowProviders) != 1 || client.Policy.MaxConcurrentInference != 1 {
		t.Fatalf("enrolled client = %+v, authenticated=%t", client, ok)
	}
	if second := enroll(payload); second.Code != http.StatusUnauthorized {
		t.Fatalf("reused enrollment status = %d, want 401", second.Code)
	}
	for _, path := range []string{filepath.Join(dir, "clients.json"), filepath.Join(dir, "clients.enrollments.json")} {
		if strings.Contains(string(mustRead(t, path)), enrolled.Token) || strings.Contains(string(mustRead(t, path)), bootstrap) {
			t.Fatalf("plaintext token persisted in %s", path)
		}
	}
}

func TestEnrollmentRejectsUnrestrictedAndExpiredTokens(t *testing.T) {
	store, err := OpenClientStore(filepath.Join(t.TempDir(), "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateEnrollment("unsafe", Policy{}, time.Minute); err == nil || !strings.Contains(err.Error(), "requires --allow-provider or --allow-model") {
		t.Fatalf("unrestricted enrollment error = %v", err)
	}
	_, token, err := store.CreateEnrollment("short", Policy{AllowProviders: []string{"openai"}}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, _, err := store.ConsumeEnrollment(token, "short"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired enrollment error = %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOversizedGatewayBodyReturnsSingleStructured413(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenClientStore(filepath.Join(dir, "clients.json"))
	sealer, _ := OpenStateSealer(filepath.Join(dir, "state.key"))
	server, err := NewServer(ServerConfig{Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: store, Sealer: sealer, MaxBodyBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"version":1,"query":"too long"}`))
	var target map[string]any
	if server.decodeJSON(rr, req, &target) {
		t.Fatal("oversized body decoded")
	}
	if rr.Code != http.StatusRequestEntityTooLarge || strings.Count(rr.Body.String(), `"code"`) != 1 {
		t.Fatalf("oversized response = %d %q", rr.Code, rr.Body.String())
	}
}

func TestRunCancellationIsClientScopedAndVersioned(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenClientStore(filepath.Join(dir, "clients.json"))
	clientA, tokenA, _ := store.Add("a", Policy{})
	_, tokenB, _ := store.Add("b", Policy{})
	sealer, _ := OpenStateSealer(filepath.Join(dir, "state.key"))
	server, err := NewServer(ServerConfig{Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: store, Sealer: sealer, Policy: Policy{}})
	if err != nil {
		t.Fatal(err)
	}
	canceled := false
	server.runs["run-a"] = &runState{clientID: clientA.ID, cancel: func() { canceled = true }, callbacks: make(map[string]chan llm.ToolExecutionResponse)}

	request := func(token, version string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/g1/runs/run-a", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if version != "" {
			req.Header.Set("Term-LLM-Gateway-Version", version)
		}
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := request(tokenB, "1"); rr.Code != http.StatusNotFound || canceled {
		t.Fatalf("foreign cancellation = %d canceled=%t", rr.Code, canceled)
	}
	if rr := request(tokenA, ""); rr.Code != http.StatusUpgradeRequired || canceled {
		t.Fatalf("version negotiation = %d canceled=%t", rr.Code, canceled)
	}
	if rr := request(tokenA, "1"); rr.Code != http.StatusNoContent || !canceled {
		t.Fatalf("owner cancellation = %d canceled=%t", rr.Code, canceled)
	}
}
