package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
)

type chatGPTTranscribeRequest struct {
	auth, accountID, originator, userAgent string
	filename, fileType, file               string
	language                               string
	hasModel                               bool
}

type chatGPTTranscribeFixture struct {
	requests []chatGPTTranscribeRequest
	creds    credentials.ChatGPTCredentials
	// refreshes and rejectedRefreshes count calls to the two refresh seams;
	// cleared records refresh tokens passed to the clear seam.
	refreshes, rejectedRefreshes int
	cleared                      []string
	refreshErr                   error
	// rotateOnRejected controls whether the forced refresh yields a new token.
	rotateOnRejected bool
}

func setupChatGPTTranscribeTest(t *testing.T, handler func(w http.ResponseWriter, got chatGPTTranscribeRequest)) *chatGPTTranscribeFixture {
	t.Helper()
	fx := &chatGPTTranscribeFixture{
		creds:            credentials.ChatGPTCredentials{AccessToken: "old-token", RefreshToken: "refresh", AccountID: "acct-1", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		rotateOnRejected: true,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got := chatGPTTranscribeRequest{
			auth:       r.Header.Get("Authorization"),
			accountID:  r.Header.Get("ChatGPT-Account-ID"),
			originator: r.Header.Get("originator"),
			userAgent:  r.Header.Get("User-Agent"),
			language:   r.FormValue("language"),
		}
		_, got.hasModel = r.MultipartForm.Value["model"]
		if files := r.MultipartForm.File["file"]; len(files) == 1 {
			got.filename = files[0].Filename
			got.fileType = files[0].Header.Get("Content-Type")
			f, err := files[0].Open()
			if err == nil {
				data, _ := io.ReadAll(f)
				f.Close()
				got.file = string(data)
			}
		}
		fx.requests = append(fx.requests, got)
		handler(w, got)
	}))
	t.Cleanup(server.Close)

	origEndpoint, origLoad, origRefresh, origRejected, origClear, origProbe := chatGPTTranscribeEndpoint, loadChatGPTTranscribeCreds, refreshChatGPTTranscribeCreds, refreshRejectedChatGPTTranscribeCreds, clearChatGPTTranscribeCreds, probeAudioDuration
	t.Cleanup(func() {
		chatGPTTranscribeEndpoint, loadChatGPTTranscribeCreds, refreshChatGPTTranscribeCreds, refreshRejectedChatGPTTranscribeCreds, clearChatGPTTranscribeCreds, probeAudioDuration = origEndpoint, origLoad, origRefresh, origRejected, origClear, origProbe
	})
	chatGPTTranscribeEndpoint = server.URL + "/backend-api/transcribe"
	loadChatGPTTranscribeCreds = func() (*credentials.ChatGPTCredentials, error) {
		creds := fx.creds
		return &creds, nil
	}
	refreshChatGPTTranscribeCreds = func(creds *credentials.ChatGPTCredentials) error {
		fx.refreshes++
		if fx.refreshErr != nil {
			return fx.refreshErr
		}
		creds.AccessToken = "new-token"
		creds.ExpiresAt = time.Now().Add(time.Hour).Unix()
		return nil
	}
	refreshRejectedChatGPTTranscribeCreds = func(creds *credentials.ChatGPTCredentials, rejected string) error {
		fx.rejectedRefreshes++
		if rejected != creds.AccessToken {
			t.Errorf("rejected token = %q, want current token %q", rejected, creds.AccessToken)
		}
		if fx.refreshErr != nil {
			return fx.refreshErr
		}
		if fx.rotateOnRejected {
			creds.AccessToken = "new-token"
		}
		return nil
	}
	clearChatGPTTranscribeCreds = func(refreshToken string) error {
		fx.cleared = append(fx.cleared, refreshToken)
		return nil
	}
	probeAudioDuration = func(context.Context, string) (time.Duration, error) { return time.Minute, nil }
	return fx
}

func writeTestAudio(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	return path
}

func respondTranscript(text string) func(http.ResponseWriter, chatGPTTranscribeRequest) {
	return func(w http.ResponseWriter, _ chatGPTTranscribeRequest) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"`+text+`"}`)
	}
}

func TestTranscribeWithConfig_ChatGPTSendsOAuthMultipart(t *testing.T) {
	fx := setupChatGPTTranscribeTest(t, respondTranscript("hello world"))
	audio := writeTestAudio(t, "memo.ogg", "audio-bytes")

	cfg := &config.Config{Transcription: config.TranscriptionConfig{Model: "ignored-model"}}
	got, err := TranscribeWithConfig(context.Background(), cfg, audio, "en", "chatgpt")
	if err != nil {
		t.Fatalf("TranscribeWithConfig: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("transcript = %q, want hello world", got)
	}
	if fx.refreshes != 0 || fx.rejectedRefreshes != 0 || len(fx.requests) != 1 {
		t.Fatalf("refreshes=%d rejected=%d requests=%d, want 0, 0 and 1", fx.refreshes, fx.rejectedRefreshes, len(fx.requests))
	}
	want := chatGPTTranscribeRequest{
		auth: "Bearer old-token", accountID: "acct-1", originator: chatGPTOriginator, userAgent: chatGPTUserAgent(),
		filename: "memo.ogg", fileType: "audio/ogg", file: "audio-bytes", language: "en",
	}
	if req := fx.requests[0]; req != want {
		t.Fatalf("request = %+v, want %+v", req, want)
	}
}

func TestTranscribeChatGPT_RefreshesExpiredSessionBeforeUpload(t *testing.T) {
	fx := setupChatGPTTranscribeTest(t, respondTranscript("fresh"))
	fx.creds.ExpiresAt = time.Now().Add(-time.Hour).Unix()

	got, err := TranscribeChatGPT(context.Background(), writeTestAudio(t, "memo.webm", "audio-bytes"), "")
	if err != nil {
		t.Fatalf("TranscribeChatGPT: %v", err)
	}
	if got != "fresh" || fx.refreshes != 1 || len(fx.requests) != 1 {
		t.Fatalf("transcript=%q refreshes=%d requests=%d", got, fx.refreshes, len(fx.requests))
	}
	if req := fx.requests[0]; req.auth != "Bearer new-token" || req.fileType != "audio/webm" {
		t.Fatalf("request = %+v, want refreshed token and audio/webm", req)
	}
}

func TestTranscribeChatGPT_UnauthorizedForcesRefresh(t *testing.T) {
	tests := []struct {
		name             string
		rotate           bool
		acceptNewToken   bool
		wantRequests     int
		wantText         string
		wantErrSubstring string
	}{
		{name: "rotated token retried", rotate: true, acceptNewToken: true, wantRequests: 2, wantText: "after refresh"},
		{name: "unchanged token not retried", rotate: false, wantRequests: 1, wantErrSubstring: "auth login chatgpt"},
		{name: "retry still unauthorized", rotate: true, wantRequests: 2, wantErrSubstring: "auth login chatgpt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := setupChatGPTTranscribeTest(t, func(w http.ResponseWriter, got chatGPTTranscribeRequest) {
				if !tt.acceptNewToken || got.auth != "Bearer new-token" {
					http.Error(w, `{"detail":"expired"}`, http.StatusUnauthorized)
					return
				}
				_, _ = io.WriteString(w, `{"text":"after refresh"}`)
			})
			fx.rotateOnRejected = tt.rotate

			got, err := TranscribeChatGPT(context.Background(), writeTestAudio(t, "memo.wav", "audio-bytes"), "")
			if len(fx.requests) != tt.wantRequests || fx.rejectedRefreshes != 1 {
				t.Fatalf("requests=%d rejectedRefreshes=%d, want %d and 1", len(fx.requests), fx.rejectedRefreshes, tt.wantRequests)
			}
			if tt.wantErrSubstring != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstring) {
					t.Fatalf("error = %v, want %q", err, tt.wantErrSubstring)
				}
				return
			}
			if err != nil || got != tt.wantText {
				t.Fatalf("transcript=%q err=%v, want %q", got, err, tt.wantText)
			}
			if retry := fx.requests[1]; retry.file != "audio-bytes" {
				t.Fatalf("retry request = %+v, want full body", retry)
			}
		})
	}
}

func TestTranscribeChatGPT_InvalidRefreshTokenClearsSession(t *testing.T) {
	fx := setupChatGPTTranscribeTest(t, respondTranscript("unexpected"))
	fx.creds.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	fx.refreshErr = oauth.ErrChatGPTRefreshTokenInvalid

	_, err := TranscribeChatGPT(context.Background(), writeTestAudio(t, "memo.wav", "audio-bytes"), "")
	if err == nil || !strings.Contains(err.Error(), "not configured") || !strings.Contains(err.Error(), "auth login chatgpt") {
		t.Fatalf("error = %v, want not-configured login hint", err)
	}
	if len(fx.cleared) != 1 || fx.cleared[0] != "refresh" {
		t.Fatalf("cleared = %v, want the rejected refresh token", fx.cleared)
	}
	if len(fx.requests) != 0 {
		t.Fatalf("requests = %d, want none", len(fx.requests))
	}
}

func TestTranscribeChatGPT_ReportsCloudflareChallenge(t *testing.T) {
	setupChatGPTTranscribeTest(t, func(w http.ResponseWriter, _ chatGPTTranscribeRequest) {
		w.Header().Set("cf-mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<!DOCTYPE html><html>Just a moment...</html>`)
	})
	audio := writeTestAudio(t, "memo.mp3", "audio-bytes")

	_, err := TranscribeChatGPT(context.Background(), audio, "")
	if err == nil || !strings.Contains(err.Error(), "Cloudflare challenge") {
		t.Fatalf("error = %v, want Cloudflare challenge message", err)
	}
}

func TestTranscribeChatGPT_RejectsInvalidInputBeforeRequest(t *testing.T) {
	fx := setupChatGPTTranscribeTest(t, respondTranscript("unexpected"))
	large := filepath.Join(t.TempDir(), "large.wav")
	f, err := os.Create(large)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(chatGPTTranscribeMaxBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	tests := []struct {
		name, path, want string
	}{
		{name: "too large", path: large, want: "limit"},
		{name: "empty", path: writeTestAudio(t, "empty.wav", ""), want: "empty"},
		{name: "unsupported extension", path: writeTestAudio(t, "memo.aiff", "audio"), want: "unsupported audio extension"},
		{name: "no extension", path: writeTestAudio(t, "voice-note", "audio"), want: "unsupported audio extension"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TranscribeChatGPT(context.Background(), tt.path, "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	if len(fx.requests) != 0 {
		t.Fatalf("requests = %d, want none", len(fx.requests))
	}
}
