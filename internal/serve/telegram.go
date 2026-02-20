package serve

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const telegramMaxMessageLen = 4000 // Telegram limit is 4096; leave margin

// TelegramPlatform implements Platform for the Telegram messaging platform.
type TelegramPlatform struct {
	cfg config.TelegramServeConfig
}

// NewTelegramPlatform creates a new TelegramPlatform with the given config.
func NewTelegramPlatform(cfg config.TelegramServeConfig) *TelegramPlatform {
	return &TelegramPlatform{cfg: cfg}
}

func (p *TelegramPlatform) Name() string { return "telegram" }

// NeedsSetup returns true when the bot token is missing.
func (p *TelegramPlatform) NeedsSetup() bool {
	return strings.TrimSpace(p.cfg.Token) == ""
}

// RunSetup runs an interactive wizard that collects and persists bot credentials.
func (p *TelegramPlatform) RunSetup() error {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println()
	fmt.Println("Telegram Bot Setup")
	fmt.Println("==================")
	fmt.Println()
	fmt.Println("1. Open @BotFather on Telegram → /newbot → copy the token")
	fmt.Print("   Token: ")

	if !scanner.Scan() {
		return fmt.Errorf("no input received")
	}
	token := strings.TrimSpace(scanner.Text())
	if token == "" {
		return fmt.Errorf("token is required")
	}

	fmt.Println()
	fmt.Println("2. Whitelist Telegram user ID(s) and/or @username(s):")
	fmt.Println("   - Send any message to your bot")
	fmt.Printf("   - Visit https://api.telegram.org/bot%s/getUpdates\n", token)
	fmt.Println("   - Find the numeric 'id' or 'username' under 'from'")
	fmt.Println("   - Mix numeric IDs and @usernames freely (e.g. 123456, @alice)")
	fmt.Print("   Allowed users (comma-separated, required): ")

	if !scanner.Scan() {
		return fmt.Errorf("no input received")
	}
	rawEntries := strings.TrimSpace(scanner.Text())
	if rawEntries == "" {
		return fmt.Errorf("at least one user ID or username is required")
	}

	var userIDs []int64
	var usernames []string
	for _, part := range strings.Split(rawEntries, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "@") {
			name := strings.TrimPrefix(part, "@")
			if name != "" {
				usernames = append(usernames, strings.ToLower(name))
			}
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid entry %q: must be a numeric ID or @username", part)
		}
		userIDs = append(userIDs, id)
	}
	if len(userIDs) == 0 && len(usernames) == 0 {
		return fmt.Errorf("at least one valid user ID or @username is required")
	}

	newCfg := config.TelegramServeConfig{
		Token:            token,
		AllowedUserIDs:   userIDs,
		AllowedUsernames: usernames,
		IdleTimeout:      p.cfg.IdleTimeout,
	}

	if err := config.SetServeTelegramConfig(newCfg); err != nil {
		return fmt.Errorf("save telegram config: %w", err)
	}

	// Update in-memory config so Run() can proceed immediately after setup.
	p.cfg = newCfg
	fmt.Println()
	fmt.Println("Telegram configuration saved.")
	return nil
}

// Run starts the Telegram bot loop, blocking until ctx is cancelled.
func (p *TelegramPlatform) Run(ctx context.Context, cfg *config.Config, settings Settings) error {
	token := strings.TrimSpace(p.cfg.Token)
	if token == "" {
		return fmt.Errorf("telegram bot token is not configured; run with --setup to configure")
	}

	if len(p.cfg.AllowedUserIDs) == 0 && len(p.cfg.AllowedUsernames) == 0 {
		log.Println("[telegram] warning: no allowed_user_ids or allowed_usernames configured; all messages will be rejected")
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return fmt.Errorf("telegram connect: %w", err)
	}
	log.Printf("[telegram] authorised as @%s", bot.Self.UserName)

	// Resolve idle timeout: CLI flag takes priority, then per-platform config.
	idleTimeout := settings.IdleTimeout
	if idleTimeout <= 0 {
		if p.cfg.IdleTimeout > 0 {
			idleTimeout = time.Duration(p.cfg.IdleTimeout) * time.Minute
		} else {
			idleTimeout = 30 * time.Minute
		}
	}

	mgr := &telegramSessionMgr{
		sessions:         make(map[int64]*telegramSession),
		cfg:              cfg,
		settings:         settings,
		store:            settings.Store,
		idleTimeout:      idleTimeout,
		allowedUserIDs:   buildAllowedSet(p.cfg.AllowedUserIDs),
		allowedUsernames: buildAllowedUsernameSet(p.cfg.AllowedUsernames),
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			mgr.closeAllSessions()
			bot.StopReceivingUpdates()
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}
			go mgr.handleMessage(ctx, bot, update.Message)
		}
	}
}

func buildAllowedSet(ids []int64) map[int64]struct{} {
	m := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

func buildAllowedUsernameSet(names []string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, name := range names {
		m[strings.ToLower(name)] = struct{}{}
	}
	return m
}

// telegramSession holds per-chat conversation state.
type telegramSession struct {
	mu           sync.Mutex
	runtime      *SessionRuntime
	history      []llm.Message
	meta         *session.Session
	lastActivity time.Time
}

// telegramSessionMgr manages per-chat sessions.
type telegramSessionMgr struct {
	mu               sync.Mutex
	sessions         map[int64]*telegramSession
	cfg              *config.Config
	settings         Settings
	store            session.Store
	idleTimeout      time.Duration
	allowedUserIDs   map[int64]struct{}
	allowedUsernames map[string]struct{}
}

func (m *telegramSessionMgr) isAllowed(userID int64, username string) bool {
	if len(m.allowedUserIDs) == 0 && len(m.allowedUsernames) == 0 {
		return false
	}
	if _, ok := m.allowedUserIDs[userID]; ok {
		return true
	}
	if username != "" {
		_, ok := m.allowedUsernames[strings.ToLower(username)]
		return ok
	}
	return false
}

func (m *telegramSessionMgr) getOrCreate(ctx context.Context, chatID int64) (*telegramSession, error) {
	m.mu.Lock()
	if sess, ok := m.sessions[chatID]; ok {
		m.mu.Unlock()
		return sess, nil
	}
	m.mu.Unlock()

	created, err := m.newSession(ctx, chatID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, ok := m.sessions[chatID]; ok {
		m.mu.Unlock()
		created.mu.Lock()
		closeTelegramSession(created)
		created.mu.Unlock()
		return existing, nil
	}
	m.sessions[chatID] = created
	m.mu.Unlock()
	return created, nil
}

func (m *telegramSessionMgr) resetSession(ctx context.Context, chatID int64) (*telegramSession, error) {
	sess, _, err := m.resetSessionIfCurrent(ctx, chatID, nil)
	return sess, err
}

func (m *telegramSessionMgr) resetSessionIfCurrent(ctx context.Context, chatID int64, expected *telegramSession) (*telegramSession, bool, error) {
	created, err := m.newSession(ctx, chatID)
	if err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	current := m.sessions[chatID]
	if expected != nil && current != nil && current != expected {
		m.mu.Unlock()
		created.mu.Lock()
		closeTelegramSession(created)
		created.mu.Unlock()
		return current, false, nil
	}
	m.sessions[chatID] = created
	m.mu.Unlock()

	if current != nil {
		current.mu.Lock()
		closeTelegramSession(current)
		current.mu.Unlock()
	}
	return created, true, nil
}

func (m *telegramSessionMgr) newSession(ctx context.Context, chatID int64) (*telegramSession, error) {
	if m.settings.NewSession == nil {
		return nil, fmt.Errorf("telegram runtime factory is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime, err := m.settings.NewSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("create runtime: %w", err)
	}

	providerName := strings.TrimSpace(runtime.ProviderName)
	if providerName == "" {
		providerName = "unknown"
	}
	modelName := strings.TrimSpace(runtime.ModelName)
	if modelName == "" {
		modelName = "unknown"
	}

	meta := &session.Session{
		ID:        session.NewID(),
		Name:      fmt.Sprintf("telegram:%d", chatID),
		Provider:  providerName,
		Model:     modelName,
		Mode:      session.ModeChat,
		Agent:     m.settings.Agent,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Search:    m.settings.Search,
		Tools:     m.settings.Tools,
		MCP:       m.settings.MCP,
		Status:    session.StatusActive,
	}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		meta.CWD = cwd
	}

	sess := &telegramSession{
		runtime:      runtime,
		meta:         meta,
		lastActivity: time.Now(),
	}

	if m.store != nil {
		m.runStoreOp(ctx, meta.ID, "Create", func(storeCtx context.Context) error {
			return m.store.Create(storeCtx, meta)
		})
		m.runStoreOp(ctx, meta.ID, "SetCurrent", func(storeCtx context.Context) error {
			return m.store.SetCurrent(storeCtx, meta.ID)
		})
	}

	return sess, nil
}

func (m *telegramSessionMgr) closeAllSessions() {
	m.mu.Lock()
	sessions := make([]*telegramSession, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.sessions = make(map[int64]*telegramSession)
	m.mu.Unlock()

	for _, sess := range sessions {
		sess.mu.Lock()
		closeTelegramSession(sess)
		sess.mu.Unlock()
	}
}

func closeTelegramSession(sess *telegramSession) {
	if sess == nil || sess.runtime == nil || sess.runtime.Cleanup == nil {
		return
	}
	sess.runtime.Cleanup()
}

func (m *telegramSessionMgr) runStoreOp(ctx context.Context, sessionID, op string, fn func(context.Context) error) {
	if m.store == nil || fn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := fn(ctx); err != nil {
		log.Printf("[telegram] %s failed for %s: %v", op, sessionID, err)
	}
}

func (m *telegramSessionMgr) runStoreOpWithTimeout(sessionID, op string, fn func(context.Context) error) {
	if m.store == nil || fn == nil {
		return
	}
	storeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fn(storeCtx); err != nil {
		log.Printf("[telegram] %s failed for %s: %v", op, sessionID, err)
	}
}

func (m *telegramSessionMgr) handleMessage(ctx context.Context, bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	if !m.isAllowed(msg.From.ID, msg.From.UserName) {
		log.Printf("[telegram] ignoring message from unauthorised user %d (@%s)", msg.From.ID, msg.From.UserName)
		return
	}

	chatID := msg.Chat.ID

	if msg.IsCommand() {
		switch msg.Command() {
		case "start", "help":
			helpText := "I'm your AI assistant. Send me a message to get started!\n\n" +
				"Commands:\n" +
				"/reset  - Clear conversation history\n" +
				"/status - Show session info"
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, helpText))
			return

		case "reset":
			if _, err := m.resetSession(ctx, chatID); err != nil {
				_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Error resetting session: "+err.Error()))
				return
			}
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Conversation history cleared."))
			return

		case "status":
			sess, err := m.getOrCreate(ctx, chatID)
			if err != nil {
				_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Error: "+err.Error()))
				return
			}
			sess.mu.Lock()
			msgCount := len(sess.history)
			lastAct := sess.lastActivity
			sess.mu.Unlock()
			status := fmt.Sprintf("Session active\nMessages in history: %d\nLast activity: %s",
				msgCount, lastAct.Format(time.RFC3339))
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, status))
			return
		}
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	sess, err := m.getOrCreate(ctx, chatID)
	if err != nil {
		_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Error creating session: "+err.Error()))
		return
	}

	// Check idle timeout and replace the whole session if expired.
	sess.mu.Lock()
	expired := time.Since(sess.lastActivity) > m.idleTimeout
	if !expired {
		sess.lastActivity = time.Now()
	}
	sess.mu.Unlock()
	if expired {
		var replaced bool
		sess, replaced, err = m.resetSessionIfCurrent(ctx, chatID, sess)
		if err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Error resetting session: "+err.Error()))
			return
		}
		if replaced {
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, "(Session reset due to inactivity)"))
		}
	}

	// Send "typing…" indicator.
	_, _ = bot.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

	if err := m.streamReply(ctx, bot, sess, chatID, text); err != nil {
		log.Printf("[telegram] error streaming reply for chat %d: %v", chatID, err)
		_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Sorry, an error occurred: "+err.Error()))
	}
}

// streamReply streams an LLM response back to the chat via live message editing.
func (m *telegramSessionMgr) streamReply(ctx context.Context, bot *tgbotapi.BotAPI, sess *telegramSession, chatID int64, userText string) error {
	// We acquire the session lock for the entire streaming call so that
	// concurrent messages from the same chat are serialised.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.runtime == nil || sess.runtime.Engine == nil {
		return fmt.Errorf("telegram runtime is not initialized")
	}

	// Build full message list: system + history + new user turn.
	messages := make([]llm.Message, 0, len(sess.history)+2)
	historyHasSystem := containsSystemMsg(sess.history)
	if m.settings.SystemPrompt != "" && !historyHasSystem {
		messages = append(messages, llm.SystemText(m.settings.SystemPrompt))
	}
	messages = append(messages, sess.history...)
	messages = append(messages, llm.UserText(userText))

	sessionID := ""
	if sess.meta != nil {
		sessionID = sess.meta.ID
	}

	// Persist incoming messages before streaming.
	if m.store != nil && sess.meta != nil {
		if m.settings.SystemPrompt != "" && !historyHasSystem {
			sysMsg := &session.Message{
				SessionID:   sess.meta.ID,
				Role:        llm.RoleSystem,
				Parts:       []llm.Part{{Type: llm.PartText, Text: m.settings.SystemPrompt}},
				TextContent: m.settings.SystemPrompt,
				CreatedAt:   time.Now(),
				Sequence:    -1,
			}
			m.runStoreOp(ctx, sess.meta.ID, "AddMessage(system)", func(storeCtx context.Context) error {
				return m.store.AddMessage(storeCtx, sess.meta.ID, sysMsg)
			})
		}
		userMsg := &session.Message{
			SessionID:   sess.meta.ID,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: userText}},
			TextContent: userText,
			CreatedAt:   time.Now(),
			Sequence:    -1,
		}
		m.runStoreOp(ctx, sess.meta.ID, "AddMessage(user)", func(storeCtx context.Context) error {
			return m.store.AddMessage(storeCtx, sess.meta.ID, userMsg)
		})
		m.runStoreOp(ctx, sess.meta.ID, "IncrementUserTurns", func(storeCtx context.Context) error {
			return m.store.IncrementUserTurns(storeCtx, sess.meta.ID)
		})
		if sess.meta.Summary == "" {
			sess.meta.Summary = session.TruncateSummary(userText)
			m.runStoreOp(ctx, sess.meta.ID, "Update(summary)", func(storeCtx context.Context) error {
				return m.store.Update(storeCtx, sess.meta)
			})
		}
		m.runStoreOp(ctx, sess.meta.ID, "SetCurrent", func(storeCtx context.Context) error {
			return m.store.SetCurrent(storeCtx, sess.meta.ID)
		})
		m.runStoreOp(ctx, sess.meta.ID, "UpdateStatus(active)", func(storeCtx context.Context) error {
			return m.store.UpdateStatus(storeCtx, sess.meta.ID, session.StatusActive)
		})
	}

	// Collect assistant and tool-result messages via the turn callback.
	var (
		producedMu sync.Mutex
		produced   []llm.Message
	)
	sess.runtime.Engine.SetTurnCompletedCallback(func(cbCtx context.Context, _ int, msgs []llm.Message, metrics llm.TurnMetrics) error {
		producedMu.Lock()
		produced = append(produced, msgs...)
		producedMu.Unlock()
		if m.store != nil && sess.meta != nil {
			for _, msg := range msgs {
				sessionMsg := session.NewMessage(sess.meta.ID, msg, -1)
				m.runStoreOp(cbCtx, sess.meta.ID, "AddMessage(turn)", func(storeCtx context.Context) error {
					return m.store.AddMessage(storeCtx, sess.meta.ID, sessionMsg)
				})
			}
			m.runStoreOp(cbCtx, sess.meta.ID, "UpdateMetrics", func(storeCtx context.Context) error {
				return m.store.UpdateMetrics(storeCtx, sess.meta.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens)
			})
		}
		return nil
	})
	defer sess.runtime.Engine.SetTurnCompletedCallback(nil)

	req := llm.Request{
		SessionID: sessionID,
		Messages:  messages,
		MaxTurns:  m.settings.MaxTurns,
		Search:    m.settings.Search,
	}

	stream, err := sess.runtime.Engine.Stream(ctx, req)
	if err != nil {
		if m.store != nil && sess.meta != nil {
			m.runStoreOpWithTimeout(sess.meta.ID, "UpdateStatus(error)", func(storeCtx context.Context) error {
				return m.store.UpdateStatus(storeCtx, sess.meta.ID, session.StatusError)
			})
		}
		return fmt.Errorf("stream: %w", err)
	}
	defer stream.Close()

	// Send placeholder message to obtain a message ID for live editing.
	placeholder, err := bot.Send(tgbotapi.NewMessage(chatID, "⏳"))
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	var (
		textMu     sync.Mutex
		textBuf    strings.Builder
		streamDone = make(chan error, 1)
	)

	// Goroutine: consume stream events.
	go func() {
		for {
			ev, recvErr := stream.Recv()
			if recvErr == io.EOF {
				streamDone <- nil
				return
			}
			if recvErr != nil {
				streamDone <- recvErr
				return
			}
			if ev.Type == llm.EventTextDelta {
				textMu.Lock()
				textBuf.WriteString(ev.Text)
				textMu.Unlock()
			}
		}
	}()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	currentMsgID := placeholder.MessageID
	msgStart := 0 // byte offset in the full text where the current Telegram message begins

	sendEdit := func(msgID int, content string, withCursor bool) {
		if withCursor {
			content += "▌"
		}
		edit := tgbotapi.NewEditMessageText(chatID, msgID, content)
		_, _ = bot.Send(edit) // rate-limit errors are silently ignored
	}

	var streamErr error
loop:
	for {
		select {
		case err := <-streamDone:
			streamErr = err
			break loop
		case <-ticker.C:
			textMu.Lock()
			full := textBuf.String()
			textMu.Unlock()

			segment := full[msgStart:]
			if len(segment) == 0 {
				continue
			}

			if len(segment) >= telegramMaxMessageLen {
				// Finalize current message at the limit, then start a new one.
				sendEdit(currentMsgID, segment[:telegramMaxMessageLen], false)
				msgStart += telegramMaxMessageLen

				newMsg, sendErr := bot.Send(tgbotapi.NewMessage(chatID, "⏳"))
				if sendErr == nil {
					currentMsgID = newMsg.MessageID
				}
			} else {
				sendEdit(currentMsgID, segment, true)
			}
		case <-ctx.Done():
			if m.store != nil && sess.meta != nil {
				status := session.StatusInterrupted
				if ctx.Err() != context.Canceled {
					status = session.StatusError
				}
				m.runStoreOpWithTimeout(sess.meta.ID, "UpdateStatus(done)", func(storeCtx context.Context) error {
					return m.store.UpdateStatus(storeCtx, sess.meta.ID, status)
				})
			}
			return ctx.Err()
		}
	}

	if streamErr != nil {
		if m.store != nil && sess.meta != nil {
			m.runStoreOpWithTimeout(sess.meta.ID, "UpdateStatus(stream_error)", func(storeCtx context.Context) error {
				return m.store.UpdateStatus(storeCtx, sess.meta.ID, session.StatusError)
			})
		}
		return streamErr
	}

	// Final edit: show full remaining text without cursor.
	textMu.Lock()
	full := textBuf.String()
	textMu.Unlock()

	segment := full[msgStart:]
	if segment == "" && full == "" {
		segment = "(no response)"
	}
	if segment != "" {
		sendEdit(currentMsgID, segment, false)
	}

	// Persist history: base + user message + produced (assistant + tool results).
	newHistory := make([]llm.Message, 0, len(sess.history)+2+len(produced))
	newHistory = append(newHistory, sess.history...)
	newHistory = append(newHistory, llm.UserText(userText))
	producedMu.Lock()
	newHistory = append(newHistory, produced...)
	producedMu.Unlock()
	// Fallback: if the callback didn't fire (no tools), record the text directly.
	if len(produced) == 0 && full != "" {
		if m.store != nil && sess.meta != nil {
			assistantMsg := session.NewMessage(sess.meta.ID, llm.AssistantText(full), -1)
			m.runStoreOp(ctx, sess.meta.ID, "AddMessage(assistant_fallback)", func(storeCtx context.Context) error {
				return m.store.AddMessage(storeCtx, sess.meta.ID, assistantMsg)
			})
		}
		newHistory = append(newHistory, llm.AssistantText(full))
	}
	sess.history = newHistory
	sess.lastActivity = time.Now()
	if m.store != nil && sess.meta != nil {
		m.runStoreOp(ctx, sess.meta.ID, "UpdateStatus(active_end)", func(storeCtx context.Context) error {
			return m.store.UpdateStatus(storeCtx, sess.meta.ID, session.StatusActive)
		})
		m.runStoreOp(ctx, sess.meta.ID, "SetCurrent(end)", func(storeCtx context.Context) error {
			return m.store.SetCurrent(storeCtx, sess.meta.ID)
		})
	}

	return nil
}

func containsSystemMsg(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			return true
		}
	}
	return false
}
