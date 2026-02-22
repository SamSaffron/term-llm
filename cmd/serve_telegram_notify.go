package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
)

var (
	serveTelegramChatID    int64
	serveTelegramParseMode string
)

var serveTelegramCmd = &cobra.Command{
	Use:   "telegram",
	Short: "Telegram utilities",
}

var serveTelegramNotifyCmd = &cobra.Command{
	Use:   "notify --chat-id <id> <message>",
	Short: "Send a Telegram message and log it to the session store",
	Args:  cobra.ExactArgs(1),
	RunE:  runServeTelegramNotify,
}

func init() {
	serveTelegramNotifyCmd.Flags().Int64Var(&serveTelegramChatID, "chat-id", 0, "Telegram chat ID to send to")
	serveTelegramNotifyCmd.Flags().StringVar(&serveTelegramParseMode, "parse-mode", "Markdown", "Telegram parse mode: Markdown or HTML")
	if err := serveTelegramNotifyCmd.MarkFlagRequired("chat-id"); err != nil {
		panic(fmt.Sprintf("failed to mark chat-id required: %v", err))
	}

	serveTelegramCmd.AddCommand(serveTelegramNotifyCmd)
	serveCmd.AddCommand(serveTelegramCmd)
}

type telegramSendRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

type telegramSendResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func runServeTelegramNotify(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	message := args[0]
	parseMode, err := normalizeTelegramParseMode(serveTelegramParseMode)
	if err != nil {
		return err
	}

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	token := strings.TrimSpace(cfg.Serve.Telegram.Token)
	if token == "" {
		return fmt.Errorf("telegram token is not configured (serve.telegram.token)")
	}

	if err := sendTelegramMessage(ctx, token, serveTelegramChatID, message, parseMode); err != nil {
		return err
	}

	logTelegramNotifySession(ctx, cfg, serveTelegramChatID, message, cmd.ErrOrStderr())
	return nil
}

func normalizeTelegramParseMode(parseMode string) (string, error) {
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

func sendTelegramMessage(ctx context.Context, token string, chatID int64, message, parseMode string) error {
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

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("telegram send failed: status %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	var apiResp telegramSendResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("decode telegram response: %w", err)
	}
	if !apiResp.OK {
		detail := strings.TrimSpace(apiResp.Description)
		if detail == "" {
			detail = "unknown error"
		}
		return fmt.Errorf("telegram send failed: %s", detail)
	}

	return nil
}

func logTelegramNotifySession(ctx context.Context, cfg *config.Config, chatID int64, message string, errWriter io.Writer) {
	store, cleanup := InitSessionStore(cfg, errWriter)
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
