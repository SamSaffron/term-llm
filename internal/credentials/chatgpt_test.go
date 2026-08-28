package credentials

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/oauth"
)

func TestRefreshChatGPTCredentialsAdoptsRotatedCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	initial := &ChatGPTCredentials{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		AccountID:    "account",
	}
	if err := SaveChatGPTCredentials(initial); err != nil {
		t.Fatalf("save initial credentials: %v", err)
	}

	first, err := GetChatGPTCredentials()
	if err != nil {
		t.Fatalf("load first credentials: %v", err)
	}
	second, err := GetChatGPTCredentials()
	if err != nil {
		t.Fatalf("load second credentials: %v", err)
	}

	oldRefresh := refreshChatGPTToken
	t.Cleanup(func() { refreshChatGPTToken = oldRefresh })
	var refreshTokens []string
	refreshChatGPTToken = func(refreshToken string) (*oauth.ChatGPTTokenResponse, error) {
		refreshTokens = append(refreshTokens, refreshToken)
		return &oauth.ChatGPTTokenResponse{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
			ExpiresIn:    3600,
		}, nil
	}

	if err := RefreshChatGPTCredentials(first); err != nil {
		t.Fatalf("refresh first credentials: %v", err)
	}
	if err := RefreshChatGPTCredentials(second); err != nil {
		t.Fatalf("refresh stale second credentials: %v", err)
	}

	if len(refreshTokens) != 1 || refreshTokens[0] != "old-refresh" {
		t.Fatalf("refresh calls = %v, want one exchange of old-refresh", refreshTokens)
	}
	if second.AccessToken != "new-access" || second.RefreshToken != "new-refresh" {
		t.Fatalf("stale credentials were not updated from disk: %+v", second)
	}
	stored, err := GetChatGPTCredentials()
	if err != nil {
		t.Fatalf("load stored credentials: %v", err)
	}
	if stored.AccessToken != "new-access" || stored.RefreshToken != "new-refresh" {
		t.Fatalf("stored credentials = %+v, want rotated credentials", stored)
	}
}

func TestClearChatGPTCredentialsIfRefreshTokenPreservesNewerCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newer := &ChatGPTCredentials{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	if err := SaveChatGPTCredentials(newer); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	if err := ClearChatGPTCredentialsIfRefreshToken("stale-refresh"); err != nil {
		t.Fatalf("conditionally clear stale token: %v", err)
	}
	stored, err := GetChatGPTCredentials()
	if err != nil {
		t.Fatalf("newer credentials were removed: %v", err)
	}
	if stored.RefreshToken != "new-refresh" {
		t.Fatalf("stored refresh token = %q, want new-refresh", stored.RefreshToken)
	}

	if err := ClearChatGPTCredentialsIfRefreshToken("new-refresh"); err != nil {
		t.Fatalf("conditionally clear current token: %v", err)
	}
	if ChatGPTCredentialsExist() {
		t.Fatal("matching credentials were not removed")
	}
}

func TestChatGPTCredentialsLockSerializesProcesses(t *testing.T) {
	if os.Getenv("TERM_LLM_CHATGPT_LOCK_HELPER") != "" {
		runChatGPTCredentialsLockHelper(t)
		return
	}

	configDir := t.TempDir()
	stateDir := t.TempDir()
	ready := filepath.Join(stateDir, "ready")
	release := filepath.Join(stateDir, "release")
	attempting := filepath.Join(stateDir, "attempting")
	acquired := filepath.Join(stateDir, "acquired")

	holder := chatGPTLockHelperCommand(configDir, "hold", ready, release, "")
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		if holder.Process != nil {
			_ = holder.Process.Kill()
		}
	})
	waitForTestFile(t, ready)

	contender := chatGPTLockHelperCommand(configDir, "acquire", attempting, "", acquired)
	if err := contender.Start(); err != nil {
		t.Fatalf("start lock contender: %v", err)
	}
	t.Cleanup(func() {
		if contender.Process != nil {
			_ = contender.Process.Kill()
		}
	})
	waitForTestFile(t, attempting)

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(acquired); err == nil {
		t.Fatal("second process acquired ChatGPT credential lock before release")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat contender state: %v", err)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release lock holder: %v", err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("lock holder failed: %v", err)
	}
	if err := contender.Wait(); err != nil {
		t.Fatalf("lock contender failed: %v", err)
	}
	waitForTestFile(t, acquired)
}

func chatGPTLockHelperCommand(configDir, action, ready, release, acquired string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestChatGPTCredentialsLockSerializesProcesses$")
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+configDir,
		"TERM_LLM_CHATGPT_LOCK_HELPER="+action,
		"TERM_LLM_CHATGPT_LOCK_READY="+ready,
		"TERM_LLM_CHATGPT_LOCK_RELEASE="+release,
		"TERM_LLM_CHATGPT_LOCK_ACQUIRED="+acquired,
	)
	return cmd
}

func runChatGPTCredentialsLockHelper(t *testing.T) {
	action := os.Getenv("TERM_LLM_CHATGPT_LOCK_HELPER")
	ready := os.Getenv("TERM_LLM_CHATGPT_LOCK_READY")
	release := os.Getenv("TERM_LLM_CHATGPT_LOCK_RELEASE")
	acquired := os.Getenv("TERM_LLM_CHATGPT_LOCK_ACQUIRED")

	if action == "acquire" {
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			t.Fatalf("signal lock attempt: %v", err)
		}
	}
	err := withChatGPTCredentialsLock(func(string) error {
		if action == "acquire" {
			return os.WriteFile(acquired, nil, 0o600)
		}
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(release); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return os.ErrDeadlineExceeded
	})
	if err != nil {
		t.Fatalf("credential lock helper: %v", err)
	}
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestRefreshChatGPTCredentialsDoesNotHoldLockDuringNetwork(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	initial := &ChatGPTCredentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "account"}
	if err := SaveChatGPTCredentials(initial); err != nil {
		t.Fatal(err)
	}

	oldRefresh := refreshChatGPTToken
	t.Cleanup(func() { refreshChatGPTToken = oldRefresh })
	started := make(chan struct{})
	release := make(chan struct{})
	refreshChatGPTToken = func(string) (*oauth.ChatGPTTokenResponse, error) {
		close(started)
		<-release
		return &oauth.ChatGPTTokenResponse{AccessToken: "stale-result", RefreshToken: "rotated", ExpiresIn: 3600}, nil
	}

	refreshDone := make(chan error, 1)
	go func() {
		copy := *initial
		refreshDone <- RefreshChatGPTCredentials(&copy)
	}()
	<-started

	saveDone := make(chan error, 1)
	go func() {
		saveDone <- SaveChatGPTCredentials(&ChatGPTCredentials{AccessToken: "new-login", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "account"})
	}()
	select {
	case err := <-saveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("credential save waited for another session's OAuth request")
	}

	close(release)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	stored, err := GetChatGPTCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "new-login" {
		t.Fatalf("stale refresh overwrote concurrent login: %+v", stored)
	}
}

func TestRefreshChatGPTCredentialsUsesDiskGenerationAsCASBaseline(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	disk := &ChatGPTCredentials{AccessToken: "disk-old", RefreshToken: "disk-refresh", ExpiresAt: time.Now().Add(-2 * time.Hour).Unix(), AccountID: "account"}
	if err := SaveChatGPTCredentials(disk); err != nil {
		t.Fatal(err)
	}
	caller := &ChatGPTCredentials{AccessToken: "caller-newer", RefreshToken: "disk-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "account"}

	oldRefresh := refreshChatGPTToken
	t.Cleanup(func() { refreshChatGPTToken = oldRefresh })
	refreshChatGPTToken = func(token string) (*oauth.ChatGPTTokenResponse, error) {
		if token != "disk-refresh" {
			t.Fatalf("refresh token = %q, want disk generation", token)
		}
		return &oauth.ChatGPTTokenResponse{AccessToken: "fresh", RefreshToken: "rotated", ExpiresIn: 3600}, nil
	}
	if err := RefreshChatGPTCredentials(caller); err != nil {
		t.Fatal(err)
	}
	if caller.AccessToken != "fresh" || caller.RefreshToken != "rotated" {
		t.Fatalf("caller credentials = %+v", caller)
	}
}

func TestConcurrentChatGPTInvalidGrantAdoptsSiblingRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	initial := &ChatGPTCredentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "account"}
	if err := SaveChatGPTCredentials(initial); err != nil {
		t.Fatal(err)
	}

	oldRefresh := refreshChatGPTToken
	t.Cleanup(func() { refreshChatGPTToken = oldRefresh })
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	refreshChatGPTToken = func(string) (*oauth.ChatGPTTokenResponse, error) {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return &oauth.ChatGPTTokenResponse{AccessToken: "fresh", RefreshToken: "rotated", ExpiresIn: 3600}, nil
		case 2:
			close(secondStarted)
			<-releaseSecond
			return nil, errors.New("invalid_grant")
		default:
			return nil, errors.New("unexpected refresh")
		}
	}

	a, b := *initial, *initial
	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { errA <- RefreshChatGPTCredentials(&a) }()
	<-firstStarted
	go func() { errB <- RefreshChatGPTCredentials(&b) }()
	<-secondStarted
	close(releaseFirst)

	deadline := time.Now().Add(time.Second)
	for {
		stored, err := GetChatGPTCredentials()
		if err == nil && stored.AccessToken == "fresh" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sibling refresh was not committed")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseSecond)
	if err := <-errA; err != nil {
		t.Fatal(err)
	}
	if err := <-errB; err != nil {
		t.Fatalf("invalid-grant sibling did not adopt committed refresh: %v", err)
	}
	if b.AccessToken != "fresh" || b.RefreshToken != "rotated" {
		t.Fatalf("sibling credentials = %+v", b)
	}
}
