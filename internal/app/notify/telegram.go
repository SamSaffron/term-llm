package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/providerhttp"
	"github.com/samsaffron/term-llm/internal/session"
	"time"
)

// Telegram helpers — shared by notify, notify telegram and serve jobs.

type telegramSendRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

type TelegramSendResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func NormalizeTelegramParseMode(parseMode string) (string, error) {
	mode := strings.TrimSpace(parseMode)
	if mode == "" {
		return "Markdown", nil
	}
	switch strings.ToLower(mode) {
	case "markdown":
		return "Markdown", nil
	case "html":
		return "HTML", nil
	default:
		return "", fmt.Errorf("invalid --parse-mode %q (must be Markdown or HTML)", parseMode)
	}
}

func SendTelegramMessage(ctx context.Context, token string, chatID int64, message, parseMode string) error {
	payload := telegramSendRequest{
		ChatID:    chatID,
		Text:      message,
		ParseMode: parseMode,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telegram request: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send telegram message: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read telegram response: %w", err)
	}

	var apiResp TelegramSendResponse
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_ = json.Unmarshal(respBody, &apiResp)
		message := fmt.Sprintf("telegram send failed: status %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
		return providerhttp.NewStatusErrorMessageString(message, resp.StatusCode, resp.Status, telegramErrorHeaders(resp.Header, apiResp.Parameters.RetryAfter), string(respBody))
	}

	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("decode telegram response: %w", err)
	}
	if !apiResp.OK {
		return TelegramAPIResponseError(resp, apiResp, respBody)
	}

	return nil
}

func TelegramAPIResponseError(resp *http.Response, apiResp TelegramSendResponse, body []byte) error {
	detail := strings.TrimSpace(apiResp.Description)
	if detail == "" {
		detail = "unknown error"
	}
	message := fmt.Sprintf("telegram send failed: %s", detail)
	statusCode := apiResp.ErrorCode
	if statusCode == 0 {
		statusCode = resp.StatusCode
	}
	return providerhttp.NewStatusErrorMessageString(message, statusCode, resp.Status, telegramErrorHeaders(resp.Header, apiResp.Parameters.RetryAfter), string(body))
}

func telegramErrorHeaders(header http.Header, retryAfterSeconds int) http.Header {
	if header == nil {
		header = make(http.Header)
	} else {
		header = header.Clone()
	}
	if retryAfterSeconds > 0 && header.Get("Retry-After") == "" {
		header.Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	return header
}

func LogTelegramNotifySession(ctx context.Context, deps Deps, cfg *config.Config, chatID int64, message string, errWriter io.Writer) {
	store, cleanup := deps.InitSessionStore(cfg, errWriter)
	defer cleanup()
	if store == nil {
		return
	}

	sessionName := fmt.Sprintf("telegram:%d", chatID)
	var sess *session.Session

	summaries, err := store.List(ctx, session.ListOptions{Name: sessionName, Limit: 1})
	if err != nil {
		log.Printf("warning: list telegram sessions: %v", err)
	} else if len(summaries) > 0 && summaries[0].Status == session.StatusActive {
		sess, err = store.Get(ctx, summaries[0].ID)
		if err != nil {
			log.Printf("warning: load telegram session %s: %v", summaries[0].ID, err)
			sess = nil
		}
	}

	if sess == nil {
		sess = &session.Session{
			Name:     sessionName,
			Provider: "telegram-notify",
			Model:    "push",
			Mode:     session.ModeChat,
			Origin:   session.OriginTelegram,
			Status:   session.StatusActive,
		}
		if err := store.Create(ctx, sess); err != nil {
			log.Printf("warning: create telegram session: %v", err)
			return
		}
	}

	msg := session.NewMessage(sess.ID, llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type: llm.PartText,
			Text: message,
		}},
	}, -1)

	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		log.Printf("warning: add telegram message to session: %v", err)
		return
	}

	if err := store.Update(ctx, sess); err != nil {
		log.Printf("warning: update telegram session timestamp: %v", err)
	}
	if err := store.SetCurrent(ctx, sess.ID); err != nil {
		log.Printf("warning: set current telegram session: %v", err)
	}
}
