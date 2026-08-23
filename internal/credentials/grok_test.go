package credentials

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/oauth"
)

type grokCredentialRoundTripFunc func(*http.Request) (*http.Response, error)

func (f grokCredentialRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func grokCredentialResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func isolateGrokCredentialTestEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
}

func TestGrokCredentialsSecureAtomicStorage(t *testing.T) {
	isolateGrokCredentialTestEnv(t)
	root := os.Getenv("XDG_CONFIG_HOME")
	creds := &GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	if err := SaveGrokCredentials(creds); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "term-llm", grokCredentialsFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := GetGrokCredentials()
	if err != nil || *loaded != *creds {
		t.Fatalf("loaded = %+v, err=%v", loaded, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GetGrokCredentials(); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
		t.Fatalf("insecure file error = %v", err)
	}
}

func TestGrokCredentialRefreshRotationContinuityAndConcurrency(t *testing.T) {
	isolateGrokCredentialTestEnv(t)
	initial := &GrokCredentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "acct_1"}
	if err := SaveGrokCredentials(initial); err != nil {
		t.Fatal(err)
	}
	var tokenCalls atomic.Int32
	client := oauth.NewGrokOAuthClient(&http.Client{Transport: grokCredentialRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case oauth.GrokTokenEndpoint:
			tokenCalls.Add(1)
			return grokCredentialResponse(200, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
		case oauth.GrokUserInfoEndpoint:
			return grokCredentialResponse(200, `{"sub":"acct_1"}`), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})})
	oldClient := grokOAuthClient
	grokOAuthClient = client
	t.Cleanup(func() { grokOAuthClient = oldClient })

	a := *initial
	b := *initial
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, creds := range []*GrokCredentials{&a, &b} {
		wg.Add(1)
		go func(creds *GrokCredentials) {
			defer wg.Done()
			errCh <- RefreshGrokCredentials(context.Background(), creds, false)
		}(creds)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", tokenCalls.Load())
	}
	for _, creds := range []*GrokCredentials{&a, &b} {
		if creds.AccessToken != "new-access" || creds.RefreshToken != "new-refresh" || creds.AccountID != "acct_1" {
			t.Fatalf("refreshed creds = %+v", creds)
		}
	}
	if err := ClearGrokCredentialsIfRefreshToken("old-refresh"); err != nil {
		t.Fatal(err)
	}
	if !GrokCredentialsExist() {
		t.Fatal("stale conditional clear removed rotated credentials")
	}
}

func TestGrokConcurrentForced401RefreshRunsOnceAndSiblingsAdopt(t *testing.T) {
	isolateGrokCredentialTestEnv(t)
	initial := &GrokCredentials{AccessToken: "rejected-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	if err := SaveGrokCredentials(initial); err != nil {
		t.Fatal(err)
	}
	var tokenCalls atomic.Int32
	client := oauth.NewGrokOAuthClient(&http.Client{Transport: grokCredentialRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case oauth.GrokTokenEndpoint:
			tokenCalls.Add(1)
			return grokCredentialResponse(200, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
		case oauth.GrokUserInfoEndpoint:
			return grokCredentialResponse(200, `{"sub":"acct_1"}`), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})})

	a := *initial
	b := *initial
	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for _, creds := range []*GrokCredentials{&a, &b} {
		wg.Add(1)
		go func(creds *GrokCredentials) {
			defer wg.Done()
			<-start
			errCh <- RefreshGrokCredentialsWithClient(context.Background(), creds, true, client)
		}(creds)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("forced OAuth refreshes = %d, want 1", tokenCalls.Load())
	}
	for _, creds := range []*GrokCredentials{&a, &b} {
		if creds.AccessToken != "fresh-access" || creds.RefreshToken != "fresh-refresh" || creds.AccountID != "acct_1" {
			t.Fatalf("sibling did not adopt refreshed credentials: %+v", creds)
		}
	}
}

func TestGrokCredentialRefreshRejectsAccountChange(t *testing.T) {
	isolateGrokCredentialTestEnv(t)
	creds := &GrokCredentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "acct_1"}
	if err := SaveGrokCredentials(creds); err != nil {
		t.Fatal(err)
	}
	oldClient := grokOAuthClient
	grokOAuthClient = oauth.NewGrokOAuthClient(&http.Client{Transport: grokCredentialRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == oauth.GrokTokenEndpoint {
			return grokCredentialResponse(200, `{"access_token":"new","refresh_token":"rotated","expires_in":3600,"token_type":"Bearer"}`), nil
		}
		return grokCredentialResponse(200, `{"sub":"acct_2"}`), nil
	})})
	t.Cleanup(func() { grokOAuthClient = oldClient })
	if err := RefreshGrokCredentials(context.Background(), creds, true); err == nil || !strings.Contains(err.Error(), "different account") {
		t.Fatalf("account continuity error = %v", err)
	}
	if GrokCredentialsExist() {
		t.Fatal("mismatched-account credentials were not cleared")
	}
}

func TestGrokCredentialRefreshPreservesRotationAcrossTransientUserInfoFailure(t *testing.T) {
	isolateGrokCredentialTestEnv(t)
	creds := &GrokCredentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "acct_1"}
	if err := SaveGrokCredentials(creds); err != nil {
		t.Fatal(err)
	}
	var tokenCalls int
	oldClient := grokOAuthClient
	grokOAuthClient = oauth.NewGrokOAuthClient(&http.Client{Transport: grokCredentialRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case oauth.GrokTokenEndpoint:
			tokenCalls++
			body, _ := io.ReadAll(req.Body)
			if tokenCalls == 1 {
				if !strings.Contains(string(body), "refresh_token=old-refresh") {
					t.Fatalf("first refresh body = %s", body)
				}
				return grokCredentialResponse(200, `{"access_token":"unverified-access","refresh_token":"rotated-refresh","expires_in":3600}`), nil
			}
			if !strings.Contains(string(body), "refresh_token=rotated-refresh") {
				t.Fatalf("retry refresh body = %s", body)
			}
			return grokCredentialResponse(200, `{"access_token":"verified-access","refresh_token":"final-refresh","expires_in":3600}`), nil
		case oauth.GrokUserInfoEndpoint:
			if tokenCalls == 1 {
				return grokCredentialResponse(503, `{"error":"temporarily_unavailable","error_description":"try later"}`), nil
			}
			return grokCredentialResponse(200, `{"sub":"acct_1"}`), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})})
	t.Cleanup(func() { grokOAuthClient = oldClient })

	err := RefreshGrokCredentials(context.Background(), creds, false)
	if err == nil || !strings.Contains(err.Error(), "preserved for retry") {
		t.Fatalf("first refresh error = %v", err)
	}
	stored, err := GetGrokCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "old-access" || stored.RefreshToken != "rotated-refresh" || !stored.IsExpired() {
		t.Fatalf("recovery credentials = %+v", stored)
	}
	if err := RefreshGrokCredentials(context.Background(), creds, false); err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "verified-access" || creds.RefreshToken != "final-refresh" || creds.AccountID != "acct_1" {
		t.Fatalf("verified credentials = %+v", creds)
	}
}

func TestGrokForcedRefreshDoesNotAdoptIdenticalAccessTokenFromMetadataOnlyChange(t *testing.T) {
	isolateGrokCredentialTestEnv(t)
	requested := &GrokCredentials{AccessToken: "same-access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Minute).Unix(), AccountID: "acct_1"}
	stored := *requested
	stored.ExpiresAt = time.Now().Add(time.Hour).Unix()
	if err := SaveGrokCredentials(&stored); err != nil {
		t.Fatal(err)
	}
	var refreshCalls int
	oldClient := grokOAuthClient
	grokOAuthClient = oauth.NewGrokOAuthClient(&http.Client{Transport: grokCredentialRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == oauth.GrokTokenEndpoint {
			refreshCalls++
			return grokCredentialResponse(200, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
		}
		return grokCredentialResponse(200, `{"sub":"acct_1"}`), nil
	})})
	t.Cleanup(func() { grokOAuthClient = oldClient })
	if err := RefreshGrokCredentials(context.Background(), requested, true); err != nil {
		t.Fatal(err)
	}
	if refreshCalls != 1 || requested.AccessToken != "new-access" {
		t.Fatalf("refresh calls=%d credentials=%+v", refreshCalls, requested)
	}
}
