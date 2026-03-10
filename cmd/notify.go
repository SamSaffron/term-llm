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
	notifyTelegramChatID    int64
	notifyTelegramParseMode string
)

var notifyCmd = &cobra.Command{
	Use:   "notify <message>",
	Short: "Send a notification to all configured platforms",
	Long: `Send a notification message to all configured platforms (Telegram, Web Push).

Examples:
  term-llm notify "build finished"
  term-llm notify --chat-id 12345 "deploy complete"
  term-llm notify telegram --chat-id 12345 "test"
  term-llm notify web "test"`,
	Args: cobra.ExactArgs(1),
	RunE: runNotifyBroadcast,
}

func init() {
	notifyCmd.Flags().Int64Var(&notifyTelegramChatID, "chat-id", 0, "Telegram chat ID to send to")
	notifyCmd.Flags().StringVar(&notifyTelegramParseMode, "parse-mode", "Markdown", "Telegram parse mode: Markdown or HTML")

	rootCmd.AddCommand(notifyCmd)
}

func runNotifyBroadcast(cmd *cobra.Command, args []string) error {
	message := args[0]

	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	var errs []string
	sent := 0

	// Telegram: send if token is configured and chat-id provided
	token := strings.TrimSpace(cfg.Serve.Telegram.Token)
	if token != "" && notifyTelegramChatID != 0 {
		parseMode, err := normalizeTelegramParseMode(notifyTelegramParseMode)
		if err != nil {
			return err
		}
		if err := sendTelegramMessage(cmd.Context(), token, notifyTelegramChatID, message, parseMode); err != nil {
			errs = append(errs, fmt.Sprintf("telegram: %v", err))
		} else {
			logTelegramNotifySession(cmd.Context(), cfg, notifyTelegramChatID, message, cmd.ErrOrStderr())
			fmt.Fprintln(cmd.ErrOrStderr(), "sent via telegram")
			sent++
		}
	}

	// Web Push: send if VAPID keys are configured
	if cfg.Serve.WebPush.VAPIDPublicKey != "" && cfg.Serve.WebPush.VAPIDPrivateKey != "" {
		n, webErrs := sendWebPushAll(cmd.Context(), cfg, message, cmd.ErrOrStderr())
		sent += n
		errs = append(errs, webErrs...)
		if n > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "sent via web push (%d subscriptions)\n", n)
		}
	}

	if sent == 0 && len(errs) == 0 {
		return fmt.Errorf("no notification platforms configured\n\nConfigure telegram (serve.telegram.token + --chat-id) or web push (serve.web_push VAPID keys)")
	}
	if len(errs) > 0 {
		return fmt.Errorf("notification errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Telegram helpers — shared by notify and notify telegram subcommands.

type telegramSendRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

type telegramSendResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
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
