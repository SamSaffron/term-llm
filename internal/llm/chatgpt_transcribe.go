package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
)

// chatGPTTranscribeURL is the ChatGPT backend speech-to-text endpoint used by
// Codex dictation. It is undocumented: the server picks the model and returns
// plain {"text": "..."} JSON.
const chatGPTTranscribeURL = "https://chatgpt.com/backend-api/transcribe"

// chatGPTTranscribeMaxBytes mirrors the upload limit of the OpenAI
// transcription API; larger files are rejected before upload.
const chatGPTTranscribeMaxBytes int64 = 25 << 20

// Test seams for the ChatGPT transcription backend.
var (
	chatGPTTranscribeEndpoint             = chatGPTTranscribeURL
	loadChatGPTTranscribeCreds            = credentials.GetChatGPTCredentials
	refreshChatGPTTranscribeCreds         = credentials.RefreshChatGPTCredentials
	refreshRejectedChatGPTTranscribeCreds = credentials.RefreshRejectedChatGPTCredentials
	clearChatGPTTranscribeCreds           = credentials.ClearChatGPTCredentialsIfRefreshToken
)

// The "not configured" wording lets callers such as the Telegram bot classify
// a missing or dead ChatGPT session as a setup problem.
const chatGPTTranscribeLoginHint = "ChatGPT transcription is not configured: run 'term-llm auth login chatgpt'"

// TranscribeChatGPT transcribes an audio file through the ChatGPT backend
// using the stored ChatGPT OAuth session (term-llm auth login chatgpt).
func TranscribeChatGPT(ctx context.Context, filePath, language string) (string, error) {
	contentType, err := chatGPTAudioContentType(filePath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("open audio file: %w", err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("chatgpt transcription: audio file is empty")
	}
	if info.Size() > chatGPTTranscribeMaxBytes {
		return "", fmt.Errorf("chatgpt transcription: audio file is %d bytes; the limit is %d bytes", info.Size(), chatGPTTranscribeMaxBytes)
	}

	creds, err := loadChatGPTTranscribeCreds()
	if err != nil {
		return "", fmt.Errorf("%s: %w", chatGPTTranscribeLoginHint, err)
	}
	if creds.IsExpired() {
		if err := refreshChatGPTTranscribeCreds(creds); err != nil {
			return "", chatGPTTranscribeRefreshError(creds, err)
		}
	}

	upload := chatGPTTranscriptionUpload{filePath: filePath, contentType: contentType, language: language}
	resp, err := upload.send(ctx, creds)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		rejected := creds.AccessToken
		if err := refreshRejectedChatGPTTranscribeCreds(creds, rejected); err != nil {
			return "", chatGPTTranscribeRefreshError(creds, err)
		}
		if creds.AccessToken == rejected {
			return "", fmt.Errorf("chatgpt transcription: ChatGPT rejected the session (HTTP 401); %s again", chatGPTTranscribeLoginHint)
		}
		resp, err = upload.send(ctx, creds)
		if err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()
	return decodeChatGPTTranscription(resp)
}

// chatGPTTranscribeRefreshError mirrors the Responses client: a refresh token
// the server reports as invalid is cleared so later calls fail fast with a
// login hint instead of re-presenting a dead session.
func chatGPTTranscribeRefreshError(creds *credentials.ChatGPTCredentials, err error) error {
	if !errors.Is(err, oauth.ErrChatGPTRefreshTokenInvalid) {
		return fmt.Errorf("refresh ChatGPT session: %w", err)
	}
	if clearErr := clearChatGPTTranscribeCreds(creds.RefreshToken); clearErr != nil {
		return fmt.Errorf("ChatGPT session expired and clearing stored credentials failed: %w", clearErr)
	}
	return fmt.Errorf("ChatGPT session expired; %s again: %w", chatGPTTranscribeLoginHint, err)
}

type chatGPTTranscriptionUpload struct {
	filePath, contentType, language string
}

func (u chatGPTTranscriptionUpload) send(ctx context.Context, creds *credentials.ChatGPTCredentials) (*http.Response, error) {
	f, err := os.Open(u.filePath)
	if err != nil {
		return nil, fmt.Errorf("open audio file: %w", err)
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer f.Close()
		_ = pw.CloseWithError(writeChatGPTTranscriptionBody(mw, filepath.Base(u.filePath), u.contentType, f, u.language))
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatGPTTranscribeEndpoint, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, fmt.Errorf("build chatgpt transcription request: %w", err)
	}
	for name, value := range ChatGPTRequestHeaders(creds) {
		req.Header.Set(name, value)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	resp, err := chatGPTHTTPClient.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, fmt.Errorf("chatgpt transcription request: %w", err)
	}
	// The server may answer (401, 403, 429) before reading the whole upload;
	// closing the pipe with the response guarantees the writer goroutine and
	// audio file are released.
	resp.Body = pipeClosingBody{ReadCloser: resp.Body, pipe: pr}
	return resp, nil
}

type pipeClosingBody struct {
	io.ReadCloser
	pipe *io.PipeReader
}

func (b pipeClosingBody) Close() error {
	_ = b.pipe.Close()
	return b.ReadCloser.Close()
}

func writeChatGPTTranscriptionBody(mw *multipart.Writer, filename, contentType string, file io.Reader, language string) (err error) {
	defer func() {
		if closeErr := mw.Close(); err == nil {
			err = closeErr
		}
	}()

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	header.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(fw, file); err != nil {
		return fmt.Errorf("write form file: %w", err)
	}
	if language = strings.TrimSpace(language); language != "" {
		if err := mw.WriteField("language", language); err != nil {
			return fmt.Errorf("write language field: %w", err)
		}
	}
	return nil
}

// chatGPTAudioContentType labels the upload by extension; the backend rejects
// generic application/octet-stream audio parts.
func chatGPTAudioContentType(filename string) (string, error) {
	switch ext := strings.ToLower(filepath.Ext(filename)); ext {
	case ".mp3", ".mpga", ".mpeg":
		return "audio/mpeg", nil
	case ".wav":
		return "audio/wav", nil
	case ".m4a", ".mp4":
		return "audio/mp4", nil
	case ".ogg", ".oga", ".opus":
		return "audio/ogg", nil
	case ".flac":
		return "audio/flac", nil
	case ".webm":
		return "audio/webm", nil
	default:
		return "", fmt.Errorf("chatgpt transcription: unsupported audio extension %q (supported: .flac, .m4a, .mp3, .mp4, .mpeg, .mpga, .oga, .ogg, .opus, .wav, .webm)", ext)
	}
}

func decodeChatGPTTranscription(resp *http.Response) (string, error) {
	if resp.StatusCode != http.StatusOK {
		body := readLimitedBody(resp.Body, whisperErrorBodyLimit)
		if resp.StatusCode == http.StatusTooManyRequests {
			if err := parseChatGPTRateLimitError([]byte(body), resp.Header); err != nil {
				return "", err
			}
		}
		message := fmt.Sprintf("chatgpt transcription error %d: %s", resp.StatusCode, body)
		if resp.StatusCode == http.StatusUnauthorized {
			message += "; " + chatGPTTranscribeLoginHint + " again"
		}
		if resp.StatusCode == http.StatusForbidden && isCloudflareChallenge(resp.Header, body) {
			message = fmt.Sprintf("chatgpt transcription error %d: blocked by a Cloudflare challenge on %s; this endpoint is undocumented and may be unavailable from this network — use another transcription provider", resp.StatusCode, chatGPTTranscribeURL)
		}
		return "", newHTTPStatusErrorMessageString(message, resp.StatusCode, resp.Status, resp.Header, body)
	}

	var result whisperResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode chatgpt transcription response: %w", err)
	}
	return result.Text, nil
}

func isCloudflareChallenge(headers http.Header, body string) bool {
	if strings.EqualFold(headers.Get("cf-mitigated"), "challenge") {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "<html") && (strings.Contains(lower, "cloudflare") || strings.Contains(lower, "__cf_chl"))
}
