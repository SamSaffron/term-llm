package serve

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/llm"
	memorystore "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

const telegramMaxMessageLen = 4000 // Telegram limit is 4096; leave margin
const minEditInterval = 3 * time.Second

// defaultStreamEventTimeout is used when telegramSessionMgr.streamEventTimeout is zero.
const defaultStreamEventTimeout = 10 * time.Minute

const telegramMaxConcurrentHandlers = 8
const telegramMaxPhotoDownloadBytes int64 = 25 << 20
const telegramMaxVoiceDownloadBytes int64 = 25 << 20
const telegramDownloadTimeout = 5 * time.Minute

var telegramDownloadHTTPClient = &http.Client{Timeout: telegramDownloadTimeout}

// botSender is the subset of tgbotapi.BotAPI used by streamReply and
// handleMessage, allowing tests to supply a fake without a live connection.
type botSender interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
}

// botFileGetter is the subset of tgbotapi.BotAPI used for downloading files.
type botFileGetter interface {
	GetFile(config tgbotapi.FileConfig) (tgbotapi.File, error)
	GetFileDirectURL(fileID string) (string, error)
}

// downloadTelegramPhoto downloads the largest photo from a Telegram photo array.
// It returns the media type, base64-encoded data, and a local temp file path.
// The caller is responsible for removing the temp file when it is no longer needed.
func downloadTelegramPhoto(fileGetter botFileGetter, photos []tgbotapi.PhotoSize) (mediaType, base64Data, filePath string, err error) {
	if len(photos) == 0 {
		return "", "", "", fmt.Errorf("no photos provided")
	}
	// Pick the largest photo (last in the array).
	photo := photos[len(photos)-1]
	if photo.FileSize > 0 && int64(photo.FileSize) > telegramMaxPhotoDownloadBytes {
		return "", "", "", fmt.Errorf("photo file too large: %d bytes (max %d)", photo.FileSize, telegramMaxPhotoDownloadBytes)
	}
	directURL, err := fileGetter.GetFileDirectURL(photo.FileID)
	if err != nil {
		return "", "", "", fmt.Errorf("get file URL: %w", err)
	}

	resp, err := telegramDownloadHTTPClient.Get(directURL)
	if err != nil {
		return "", "", "", fmt.Errorf("download photo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", "", "", fmt.Errorf("download photo: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.ContentLength > telegramMaxPhotoDownloadBytes {
		return "", "", "", fmt.Errorf("photo file too large: %d bytes (max %d)", resp.ContentLength, telegramMaxPhotoDownloadBytes)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, telegramMaxPhotoDownloadBytes+1))
	if err != nil {
		return "", "", "", fmt.Errorf("read photo data: %w", err)
	}
	if int64(len(data)) > telegramMaxPhotoDownloadBytes {
		return "", "", "", fmt.Errorf("photo file too large: exceeds %d bytes", telegramMaxPhotoDownloadBytes)
	}

	// Detect MIME type from content (Telegram can serve PNG, WebP, etc.)
	mimeType := http.DetectContentType(data)
	if mimeType == "application/octet-stream" {
		mimeType = "image/jpeg" // safe fallback
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return "", "", "", fmt.Errorf("download photo: unexpected content type %q", mimeType)
	}

	// Write to a temp file so tools (e.g. image_generate) can reference it by path.
	ext := mimeExtension(mimeType)
	tmp, err := os.CreateTemp("", "tg-img-*"+ext)
	if err != nil {
		return "", "", "", fmt.Errorf("create temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", "", "", fmt.Errorf("write temp file: %w", err)
	}
	tmp.Close()

	return mimeType, base64.StdEncoding.EncodeToString(data), tmp.Name(), nil
}

// downloadTelegramVoice downloads a Telegram voice message OGG file to a temp file.
// The caller is responsible for removing the temp file when done.
func downloadTelegramVoice(fileGetter botFileGetter, voice *tgbotapi.Voice) (filePath string, err error) {
	if voice == nil {
		return "", fmt.Errorf("no voice provided")
	}
	if voice.FileSize > 0 && int64(voice.FileSize) > telegramMaxVoiceDownloadBytes {
		return "", fmt.Errorf("voice file too large: %d bytes (max %d)", voice.FileSize, telegramMaxVoiceDownloadBytes)
	}

	directURL, err := fileGetter.GetFileDirectURL(voice.FileID)
	if err != nil {
		return "", fmt.Errorf("get voice file URL: %w", err)
	}

	resp, err := telegramDownloadHTTPClient.Get(directURL)
	if err != nil {
		return "", fmt.Errorf("download voice: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("download voice: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.ContentLength > telegramMaxVoiceDownloadBytes {
		return "", fmt.Errorf("voice file too large: %d bytes (max %d)", resp.ContentLength, telegramMaxVoiceDownloadBytes)
	}

	tmp, err := os.CreateTemp("", "tg-voice-*.ogg")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()

	written, copyErr := io.Copy(tmp, io.LimitReader(resp.Body, telegramMaxVoiceDownloadBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", fmt.Errorf("write voice temp file: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close voice temp file: %w", closeErr)
	}
	if written > telegramMaxVoiceDownloadBytes {
		return "", fmt.Errorf("voice file too large: exceeds %d bytes", telegramMaxVoiceDownloadBytes)
	}

	return tmp.Name(), nil
}

// mimeExtension returns a file extension for a MIME type.
func mimeExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".jpg"
	}
}

func recordTelegramUpload(cfg *config.Config, agent, sessionID, mimeType, caption, srcPath string) {
	if cfg == nil || strings.TrimSpace(srcPath) == "" {
		return
	}
	caption = strings.TrimSpace(caption)
	if caption == "" {
		caption = "[telegram photo upload]"
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		log.Printf("[telegram] failed to read uploaded photo %s: %v", srcPath, err)
		return
	}

	outputDir := strings.TrimSpace(cfg.Image.OutputDir)
	if outputDir == "" {
		outputDir = "~/Pictures/term-llm"
	}
	outputDir = filepath.Join(outputDir, "uploads")

	outputPath, err := image.SaveImage(data, outputDir, caption)
	if err != nil {
		log.Printf("[telegram] failed to save uploaded photo: %v", err)
		return
	}

	store, err := memorystore.NewStore(memorystore.Config{Path: os.Getenv("TERM_LLM_MEMORY_DB")})
	if err != nil {
		log.Printf("[telegram] failed to open memory store for image upload: %v", err)
		return
	}
	defer store.Close()

	rec := &memorystore.ImageRecord{
		Agent:      agent,
		SessionID:  sessionID,
		Prompt:     caption,
		OutputPath: outputPath,
		MimeType:   mimeType,
		Provider:   "telegram-upload",
		FileSize:   len(data),
	}
	_ = store.RecordImage(context.Background(), rec)
}

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
	fmt.Println("   - Visit https://api.telegram.org/bot<token>/getUpdates (paste your token locally, do not log it)")
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
		InterruptTimeout: p.cfg.InterruptTimeout,
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

	interruptTimeout := 3 * time.Second
	if p.cfg.InterruptTimeout > 0 {
		interruptTimeout = time.Duration(p.cfg.InterruptTimeout) * time.Second
	}
	mgr := &telegramSessionMgr{
		sessions:         make(map[int64]*telegramSession),
		cfg:              cfg,
		settings:         settings,
		store:            settings.Store,
		idleTimeout:      idleTimeout,
		interruptTimeout: interruptTimeout,
		allowedUserIDs:   buildAllowedSet(p.cfg.AllowedUserIDs),
		allowedUsernames: buildAllowedUsernameSet(p.cfg.AllowedUsernames),
		messageSlots:     make(chan struct{}, telegramMaxConcurrentHandlers),
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
			if !mgr.acquireMessageSlot(ctx) {
				mgr.closeAllSessions()
				bot.StopReceivingUpdates()
				return nil
			}
			go func(msg *tgbotapi.Message) {
				defer mgr.releaseMessageSlot()
				mgr.handleMessage(ctx, bot, msg)
			}(update.Message)
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

func (m *telegramSessionMgr) acquireMessageSlot(ctx context.Context) bool {
	slots := m.messageSlots
	if slots == nil {
		slots = make(chan struct{}, telegramMaxConcurrentHandlers)
		m.messageSlots = slots
	}
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *telegramSessionMgr) releaseMessageSlot() {
	<-m.messageSlots
}

// telegramSession holds per-chat conversation state.
type telegramSession struct {
	mu                    sync.Mutex
	runtime               *SessionRuntime
	history               []llm.Message
	systemPromptPersisted bool
	carryoverContext      string // one-time context carried from the previous replaced session
	carryoverContextLabel string
	meta                  *session.Session
	lastActivity          time.Time

	cancelMu      sync.Mutex         // protects streamCancel, replyDone and task/tool tracking
	streamCancel  context.CancelFunc // cancels the active stream's context
	replyDone     chan struct{}      // closed when streamReply exits
	currentTask   string             // text from the user message that started the active stream
	toolsRanNames []string           // tool names executed during the active stream

	streamProseLen atomic.Int64
	streamToolCnt  atomic.Int32
	streamToolName atomic.Value // string
}

// telegramSessionMgr manages per-chat sessions.
type telegramSessionMgr struct {
	mu               sync.Mutex
	sessions         map[int64]*telegramSession
	cfg              *config.Config
	settings         Settings
	store            session.Store
	idleTimeout      time.Duration
	interruptTimeout time.Duration
	allowedUserIDs   map[int64]struct{}
	allowedUsernames map[string]struct{}
	messageSlots     chan struct{}
	tickerInterval   time.Duration // 0 means use default (500ms); overridden in tests

	// streamEventTimeout bounds how long the stream watchdog waits between events
	// before declaring the stream dead. 0 means use defaultStreamEventTimeout.
	streamEventTimeout time.Duration
}

func (m *telegramSessionMgr) newFastProvider() llm.Provider {
	if m == nil || m.cfg == nil {
		return nil
	}
	fastProvider, err := llm.NewFastProvider(m.cfg, m.cfg.DefaultProvider)
	if err != nil {
		log.Printf("[telegram] fast provider unavailable: %v", err)
		return nil
	}
	return fastProvider
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
		closeTelegramSession(created)
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
		closeTelegramSession(created)
		return current, false, nil
	}
	m.sessions[chatID] = created
	m.mu.Unlock()

	if current != nil {
		closeTelegramSession(current)
	}
	return created, true, nil
}

// restoreHistoryFromDB loads the tail of message history from the most recent
// prior session for this chatID, capped at TelegramCarryoverChars worth of text.
// This ensures continuity after server restarts without bloating context.
func (m *telegramSessionMgr) restoreHistoryFromDB(ctx context.Context, chatID int64, sess *telegramSession) {
	if m.store == nil || sess.meta == nil {
		return
	}

	maxChars := m.settings.TelegramCarryoverChars
	if maxChars <= 0 {
		return // 0 (or negative) explicitly disables carryover
	}

	sessions, err := m.store.List(ctx, session.ListOptions{
		Name:  fmt.Sprintf("telegram:%d", chatID),
		Limit: 5,
	})
	if err != nil {
		log.Printf("[telegram] restore history: list failed for chat %d: %v", chatID, err)
		return
	}

	// Find the most recent prior session with messages.
	for i := range sessions {
		s := &sessions[i]
		if s.ID == sess.meta.ID || s.MessageCount == 0 {
			continue
		}

		msgs, loadErr := m.store.GetMessages(ctx, s.ID, 0, 0)
		if loadErr != nil {
			log.Printf("[telegram] restore history: get messages failed for session %s: %v", s.ID, loadErr)
			continue
		}

		// Convert all messages, then take only the tail that fits within maxChars.
		all := make([]llm.Message, 0, len(msgs))
		for _, msg := range msgs {
			all = append(all, msg.ToLLMMessage())
		}

		history := sanitizeCarryoverMessages(tailMessages(all, maxChars))
		if len(history) > 0 {
			sess.history = history
			log.Printf("[telegram] restored %d messages (of %d) from session %s for chat %d",
				len(history), len(all), s.ID, chatID)
		}
		return
	}
}

// tailMessages returns the largest suffix of msgs whose combined text fits
// within maxChars. It never splits a message — whole messages are included
// or excluded.
func tailMessages(msgs []llm.Message, maxChars int) []llm.Message {
	if maxChars <= 0 || len(msgs) == 0 {
		return nil
	}

	// Walk backwards, accumulating character count.
	total := 0
	start := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		text := extractMessageTextWithPlaceholders(msgs[i])
		n := len([]rune(text))
		if total+n > maxChars && start < len(msgs) {
			break // adding this message would exceed the budget
		}
		total += n
		start = i
	}
	return msgs[start:]
}

func sanitizeCarryoverMessages(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return nil
	}

	sanitized := make([]llm.Message, 0, len(msgs))
	for _, msg := range msgs {
		sanitized = append(sanitized, sanitizeCarryoverMessage(msg))
	}
	return sanitized
}

func sanitizeCarryoverMessage(msg llm.Message) llm.Message {
	sanitized := llm.Message{
		Role:        msg.Role,
		CacheAnchor: msg.CacheAnchor,
		Parts:       make([]llm.Part, 0, len(msg.Parts)),
	}

	for _, part := range msg.Parts {
		switch part.Type {
		case llm.PartImage:
			sanitized.Parts = append(sanitized.Parts, llm.Part{Type: llm.PartText, Text: "[image uploaded]"})
		case llm.PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			call := *part.ToolCall
			if len(call.Arguments) > 0 {
				call.Arguments = append([]byte(nil), call.Arguments...)
			}
			if len(call.ThoughtSig) > 0 {
				call.ThoughtSig = append([]byte(nil), call.ThoughtSig...)
			}
			sanitized.Parts = append(sanitized.Parts, llm.Part{Type: llm.PartToolCall, ToolCall: &call})
		case llm.PartToolResult:
			if part.ToolResult == nil {
				continue
			}
			result := &llm.ToolResult{
				ID:      part.ToolResult.ID,
				Name:    part.ToolResult.Name,
				Content: extractToolResultTextWithPlaceholders(part.ToolResult),
				IsError: part.ToolResult.IsError,
			}
			if len(part.ToolResult.ThoughtSig) > 0 {
				result.ThoughtSig = append([]byte(nil), part.ToolResult.ThoughtSig...)
			}
			sanitized.Parts = append(sanitized.Parts, llm.Part{Type: llm.PartToolResult, ToolResult: result})
		default:
			sanitized.Parts = append(sanitized.Parts, part)
		}
	}

	return sanitized
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
		Origin:    session.OriginTelegram,
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

	if m.store != nil {
		m.restoreHistoryFromDB(ctx, chatID, sess)
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
		closeTelegramSession(sess)
	}
}

func closeTelegramSession(sess *telegramSession) {
	if sess == nil {
		return
	}

	sess.cancelMu.Lock()
	cancelFn := sess.streamCancel
	doneCh := sess.replyDone
	sess.cancelMu.Unlock()

	if cancelFn != nil {
		cancelFn()
		if doneCh != nil {
			<-doneCh
		}
	}

	if sess.runtime != nil && sess.runtime.Cleanup != nil {
		sess.runtime.Cleanup()
	}
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
	if msg.From == nil {
		log.Printf("[telegram] ignoring message with no sender")
		return
	}
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

	// Build the user message: photo or text.
	var (
		userMsg         llm.Message
		tempImagePath   string // non-empty when we wrote a temp file for this message
		uploadMediaType string
		uploadCaption   string
	)
	if msg.Photo != nil && len(msg.Photo) > 0 {
		mediaType, base64Data, imgPath, err := downloadTelegramPhoto(bot, msg.Photo)
		if err != nil {
			log.Printf("[telegram] failed to download photo for chat %d: %v", chatID, err)
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Failed to process photo: "+err.Error()))
			return
		}
		tempImagePath = imgPath
		uploadMediaType = mediaType
		uploadCaption = strings.TrimSpace(msg.Caption)
		userMsg = llm.UserImageMessageWithPath(mediaType, base64Data, imgPath, uploadCaption)
	} else if msg.Voice != nil {
		voicePath, err := downloadTelegramVoice(bot, msg.Voice)
		if err != nil {
			log.Printf("telegram: download voice error: %v", err)
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Sorry, couldn't download your voice message."))
			return
		}
		defer os.Remove(voicePath)

		transcript, err := llm.TranscribeWithConfig(ctx, m.cfg, voicePath, "", "")
		if err != nil {
			log.Printf("telegram: transcribe error: %v", err)
			// Check if it's a config/key issue vs a transient error
			if strings.Contains(err.Error(), "not configured") || strings.Contains(err.Error(), "no API key") || strings.Contains(err.Error(), "not found in providers") {
				_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Voice transcription is not configured. Set up a transcription provider in your config."))
			} else {
				_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Sorry, couldn't transcribe your voice message."))
			}
			return
		}

		userMsg = llm.UserText("🎤 " + transcript)
	} else {
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			return
		}
		userMsg = llm.UserText(text)
	}
	// Clean up the temp image file once the reply is fully delivered.
	// We defer here so it covers all return paths below (error exits, interrupts, etc.).
	if tempImagePath != "" {
		defer os.Remove(tempImagePath)
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
		sess, _, err = m.resetSessionIfCurrent(ctx, chatID, sess)
		if err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Error resetting session: "+err.Error()))
			return
		}
	}

	// If a stream is active, decide whether to cancel, interject, or queue.
	sess.cancelMu.Lock()
	doneCh := sess.replyDone
	cancelFn := sess.streamCancel
	sess.cancelMu.Unlock()

	if cancelFn != nil && doneCh != nil {
		newMsgText := strings.TrimSpace(extractPlainTextFromMsg(msg))
		if newMsgText == "" {
			newMsgText = strings.TrimSpace(collectUserText(userMsg))
		}

		select {
		case <-doneCh:
			// Stream finished naturally within the grace period.
		case <-time.After(500 * time.Millisecond):
			sess.cancelMu.Lock()
			activity := llm.InterruptActivity{
				CurrentTask: strings.TrimSpace(sess.currentTask),
				ToolsRun:    append([]string(nil), sess.toolsRanNames...),
				ProseLen:    int(sess.streamProseLen.Load()),
				ActiveTool:  "",
			}
			if v := sess.streamToolName.Load(); v != nil {
				if name, ok := v.(string); ok {
					activity.ActiveTool = name
				}
			}
			sess.cancelMu.Unlock()

			fastProvider := m.newFastProvider()
			action := llm.ClassifyInterrupt(ctx, fastProvider, newMsgText, activity)
			switch action {
			case llm.InterruptCancel:
				_, _ = bot.Send(tgbotapi.NewMessage(chatID, "↩️ Stopping current work and switching to your new request."))
				cancelFn()
				stopWait := 5 * time.Second
				if m.interruptTimeout > stopWait {
					stopWait = m.interruptTimeout
				}
				select {
				case <-doneCh:
				case <-time.After(stopWait):
					log.Printf("[telegram] stream for chat %d did not stop within hard timeout", chatID)
					_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Still processing, please try again."))
					return
				case <-ctx.Done():
					return
				}
			case llm.InterruptInterject:
				sess.runtime.Engine.Interject(newMsgText)
				_, _ = bot.Send(tgbotapi.NewMessage(chatID, "📝 Noted. I will incorporate that while I continue."))
				// Do not persist here: the engine persists the interjection exactly once
				// when it drains it into the conversation via TurnCompletedCallback.
				return
			}
		case <-ctx.Done():
			return
		}
	}

	// Send "typing…" indicator.
	_, _ = bot.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

	sess.cancelMu.Lock()
	sess.currentTask = collectUserText(userMsg)
	sess.toolsRanNames = nil
	sess.streamProseLen.Store(0)
	sess.streamToolCnt.Store(0)
	sess.streamToolName.Store("")
	sess.cancelMu.Unlock()

	if err := m.streamReply(ctx, bot, sess, chatID, userMsg); err != nil {
		log.Printf("[telegram] error streaming reply for chat %d: %v", chatID, err)
		_, _ = bot.Send(tgbotapi.NewMessage(chatID, "Sorry, an error occurred: "+err.Error()))
	}

	if tempImagePath != "" {
		sessionID := ""
		if sess.meta != nil {
			sessionID = sess.meta.ID
		}
		recordTelegramUpload(m.cfg, m.settings.Agent, sessionID, uploadMediaType, uploadCaption, tempImagePath)
	}
}

// streamReply streams an LLM response back to the chat via live message editing.
func (m *telegramSessionMgr) streamReply(ctx context.Context, bot botSender, sess *telegramSession, chatID int64, userMsg llm.Message) error {
	// We acquire the session lock for the entire streaming call so that
	// concurrent messages from the same chat are serialised.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.runtime == nil || sess.runtime.Engine == nil {
		return fmt.Errorf("telegram runtime is not initialized")
	}

	// Create a cancellable child context so new messages can interrupt the stream.
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	replyDone := make(chan struct{})
	sess.cancelMu.Lock()
	sess.streamCancel = streamCancel
	sess.replyDone = replyDone
	sess.cancelMu.Unlock()
	defer func() {
		sess.cancelMu.Lock()
		sess.streamCancel = nil
		sess.replyDone = nil
		sess.currentTask = ""
		sess.toolsRanNames = nil
		sess.streamProseLen.Store(0)
		sess.streamToolCnt.Store(0)
		sess.streamToolName.Store("")
		sess.cancelMu.Unlock()
		close(replyDone)
	}()

	// Extract text from the user message for persistence and display.
	userText := collectUserText(userMsg)

	// Build full message list: system + history + new user turn.
	messages := make([]llm.Message, 0, len(sess.history)+3)
	historyHasSystem := containsSystemMsg(sess.history)
	if m.settings.SystemPrompt != "" && !historyHasSystem {
		messages = append(messages, llm.SystemText(m.settings.SystemPrompt))
	}
	if sess.carryoverContext != "" {
		label := sess.carryoverContextLabel
		if label == "" {
			label = "Context from previous session (tail):"
		}
		messages = append(messages, llm.SystemText(label+"\n"+sess.carryoverContext))
		sess.carryoverContext = ""
		sess.carryoverContextLabel = ""
	}
	// Inject platform developer message for telegram.
	if devText := m.settings.PlatformMessages.For("telegram"); devText != "" {
		messages = append(messages, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: devText}}})
	}
	messages = append(messages, sess.history...)
	messages = append(messages, userMsg)

	sessionID := ""
	if sess.meta != nil {
		sessionID = sess.meta.ID
	}

	// Persist incoming messages before streaming.
	if m.store != nil && sess.meta != nil {
		if m.settings.SystemPrompt != "" && !sess.systemPromptPersisted {
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
			sess.systemPromptPersisted = true
		}
		storeUserMsg := &session.Message{
			SessionID:   sess.meta.ID,
			Role:        llm.RoleUser,
			Parts:       userMsg.Parts,
			TextContent: userText,
			CreatedAt:   time.Now(),
			Sequence:    -1,
		}
		m.runStoreOp(ctx, sess.meta.ID, "AddMessage(user)", func(storeCtx context.Context) error {
			return m.store.AddMessage(storeCtx, sess.meta.ID, storeUserMsg)
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

	// Collect assistant and tool-result messages via callbacks.
	// ResponseCompletedCallback captures assistant messages (with tool call parts)
	// before tool execution. TurnCompletedCallback captures tool results after
	// execution, or the final assistant message when no tools are used.
	var (
		producedMu                 sync.Mutex
		produced                   []llm.Message
		assistantCapturedForTurnCB bool
	)
	sess.runtime.Engine.SetResponseCompletedCallback(func(cbCtx context.Context, _ int, assistantMsg llm.Message, _ llm.TurnMetrics) error {
		producedMu.Lock()
		produced = append(produced, assistantMsg)
		assistantCapturedForTurnCB = true
		producedMu.Unlock()
		if m.store != nil && sess.meta != nil {
			sessionMsg := session.NewMessage(sess.meta.ID, assistantMsg, -1)
			m.runStoreOp(cbCtx, sess.meta.ID, "AddMessage(response)", func(storeCtx context.Context) error {
				return m.store.AddMessage(storeCtx, sess.meta.ID, sessionMsg)
			})
		}
		return nil
	})
	defer sess.runtime.Engine.SetResponseCompletedCallback(nil)
	sess.runtime.Engine.SetTurnCompletedCallback(func(cbCtx context.Context, _ int, msgs []llm.Message, metrics llm.TurnMetrics) error {
		appendStart := 0
		producedMu.Lock()
		if assistantCapturedForTurnCB && len(msgs) > 0 && msgs[0].Role == llm.RoleAssistant {
			appendStart = 1
		}
		if appendStart < len(msgs) {
			produced = append(produced, msgs[appendStart:]...)
		}
		assistantCapturedForTurnCB = false
		producedMu.Unlock()
		if m.store != nil && sess.meta != nil {
			for _, msg := range msgs[appendStart:] {
				sessionMsg := session.NewMessage(sess.meta.ID, msg, -1)
				m.runStoreOp(cbCtx, sess.meta.ID, "AddMessage(turn)", func(storeCtx context.Context) error {
					return m.store.AddMessage(storeCtx, sess.meta.ID, sessionMsg)
				})
			}
			m.runStoreOp(cbCtx, sess.meta.ID, "UpdateMetrics", func(storeCtx context.Context) error {
				return m.store.UpdateMetrics(storeCtx, sess.meta.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens, metrics.CacheWriteTokens)
			})
			if total, count := sess.runtime.Engine.ContextEstimateBaseline(); total > 0 && count > 0 {
				sess.meta.LastTotalTokens = total
				sess.meta.LastMessageCount = count
				m.runStoreOp(cbCtx, sess.meta.ID, "UpdateContextEstimate", func(storeCtx context.Context) error {
					return m.store.UpdateContextEstimate(storeCtx, sess.meta.ID, total, count)
				})
			}
		}
		return nil
	})
	defer sess.runtime.Engine.SetTurnCompletedCallback(nil)

	req := llm.Request{
		SessionID:           sessionID,
		Messages:            messages,
		MaxTurns:            m.settings.MaxTurns,
		Debug:               m.settings.Debug,
		DebugRaw:            m.settings.DebugRaw,
		Search:              m.settings.Search,
		ForceExternalSearch: m.settings.ForceExternalSearch,
	}

	// Populate tools so the engine enters the agentic tool loop.
	if specs := llm.ToolSpecsForRequest(sess.runtime.Engine.Tools(), m.settings.Search); len(specs) > 0 {
		req.Tools = specs
		req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}

	stream, err := sess.runtime.Engine.Stream(streamCtx, req)
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
		textMu           sync.Mutex
		textBuf          strings.Builder
		activeTools      = make(map[string]string) // toolCallID → toolName
		activePhase      string                    // most-recent EventPhase text, "" when idle
		toolsRan         bool                      // true once any EventToolExecStart seen
		collectedImages  []string                  // image paths from tool executions
		textDeltas       int
		reasoningDeltas  int
		toolStarts       int
		toolEnds         int
		toolCalls        int
		phaseEvents      int
		usageEvents      int
		doneEvents       int
		retryEvents      int
		errorEvents      int
		otherEvents      int
		otherTypes       = make(map[llm.EventType]int)
		streamDone       = make(chan error, 1)
		lastEventPing    = make(chan struct{}, 1)
		watchdogTimedOut atomic.Bool
	)

	watchdogTimeout := m.streamEventTimeout
	if watchdogTimeout <= 0 {
		watchdogTimeout = defaultStreamEventTimeout
	}

	// Watchdog: cancel stream if no events arrive for watchdogTimeout.
	go func() {
		t := time.NewTimer(watchdogTimeout)
		defer t.Stop()
		for {
			select {
			case <-lastEventPing:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(watchdogTimeout)
			case <-t.C:
				watchdogTimedOut.Store(true)
				select {
				case streamDone <- fmt.Errorf("stream timed out: no events for %s", watchdogTimeout):
				default:
				}
				streamCancel()
				return
			case <-streamCtx.Done():
				return
			}
		}
	}()

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
			select {
			case lastEventPing <- struct{}{}:
			default:
			}
			switch ev.Type {
			case llm.EventTextDelta:
				textMu.Lock()
				textBuf.WriteString(ev.Text)
				textDeltas++
				proseLen := textBuf.Len()
				textMu.Unlock()
				sess.streamProseLen.Store(int64(proseLen))
			case llm.EventReasoningDelta:
				textMu.Lock()
				reasoningDeltas++
				textMu.Unlock()
			case llm.EventToolExecStart:
				textMu.Lock()
				activeTools[ev.ToolCallID] = ev.ToolName
				toolsRan = true
				toolStarts++
				textMu.Unlock()
				sess.streamToolCnt.Add(1)
				sess.streamToolName.Store(ev.ToolName)
				sess.cancelMu.Lock()
				sess.toolsRanNames = append(sess.toolsRanNames, ev.ToolName)
				sess.cancelMu.Unlock()
			case llm.EventToolExecEnd:
				textMu.Lock()
				delete(activeTools, ev.ToolCallID)
				toolEnds++
				if len(ev.ToolImages) > 0 {
					collectedImages = append(collectedImages, ev.ToolImages...)
				}
				textMu.Unlock()
				if sess.streamToolCnt.Load() > 0 {
					sess.streamToolCnt.Add(-1)
				}
				if sess.streamToolCnt.Load() <= 0 {
					sess.streamToolName.Store("")
				}
			case llm.EventHeartbeat:
				// No-op: presence of an event refreshes watchdog and keeps long tools alive.
			case llm.EventPhase:
				textMu.Lock()
				activePhase = ev.Text
				phaseEvents++
				textMu.Unlock()
			case llm.EventToolCall:
				textMu.Lock()
				toolCalls++
				textMu.Unlock()
			case llm.EventUsage:
				textMu.Lock()
				usageEvents++
				textMu.Unlock()
			case llm.EventDone:
				textMu.Lock()
				doneEvents++
				textMu.Unlock()
			case llm.EventRetry:
				textMu.Lock()
				retryEvents++
				activePhase = fmt.Sprintf("Retrying (%d/%d), waiting %.0fs", ev.RetryAttempt, ev.RetryMaxAttempts, ev.RetryWaitSecs)
				textMu.Unlock()
			case llm.EventInterjection:
				textMu.Lock()
				activePhase = "📝 Considering: " + tailRunes(strings.TrimSpace(ev.Text), 80)
				textMu.Unlock()
			case llm.EventError:
				textMu.Lock()
				errorEvents++
				textMu.Unlock()
				if ev.Err != nil {
					streamDone <- ev.Err
					return
				}
			default:
				textMu.Lock()
				otherEvents++
				otherTypes[ev.Type]++
				textMu.Unlock()
			}
		}
	}()

	interval := m.tickerInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	currentMsgID := placeholder.MessageID
	msgStart := 0       // byte offset in the full text where the current Telegram message begins
	needNewMsg := false // true when overflow happened but next placeholder not yet created

	var lastSentContent string
	var lastEditTime time.Time
	lastVisibleChange := time.Now()
	streamStart := time.Now()
	spinChars := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	spinIdx := 0

	sendEdit := func(msgID int, content string, force bool) bool {
		if !force && content == lastSentContent {
			return false
		}
		if !force && !lastEditTime.IsZero() && time.Since(lastEditTime) < minEditInterval {
			return false
		}
		edit := tgbotapi.NewEditMessageText(chatID, msgID, mdToTelegramHTML(content))
		edit.ParseMode = tgbotapi.ModeHTML
		if _, sendErr := bot.Send(edit); sendErr != nil {
			if strings.Contains(sendErr.Error(), "429") || strings.Contains(sendErr.Error(), "Too Many Requests") {
				log.Printf("[telegram] edit rate limited (chat %d): %v", chatID, sendErr)
			}
			return false
		}
		contentChanged := content != lastSentContent
		lastSentContent = content
		lastEditTime = time.Now()
		if contentChanged {
			lastVisibleChange = lastEditTime
		}
		return true
	}

	var streamErr error
	userInterrupted := false
	streamDoneDrained := false
loop:
	for {
		select {
		case err := <-streamDone:
			streamDoneDrained = true
			// If streamCtx was cancelled by user interrupt (not parent ctx, not
			// watchdog), the error from Recv is a consequence of the cancel, not
			// a real stream failure. Treat as interrupt instead.
			if ctx.Err() == nil && !watchdogTimedOut.Load() && streamCtx.Err() != nil {
				userInterrupted = true
				break loop
			}
			streamErr = err
			break loop
		case <-ticker.C:
			textMu.Lock()
			full, toolDisplay, phase := textBuf.String(), activeToolDisplay(activeTools), activePhase
			textMu.Unlock()

			prose := ""
			if msgStart < len(full) {
				prose = full[msgStart:]
			}

			forceProgress := time.Since(lastVisibleChange) >= 12*time.Second
			if prose == "" && toolDisplay == "" && phase == "" {
				if !forceProgress {
					continue
				}
				elapsed := time.Since(streamStart)
				spin := string(spinChars[spinIdx%len(spinChars)])
				spinIdx++
				heartbeat := buildHeartbeatSegment("", toolDisplay, phase, spin, elapsed)
				sendEdit(currentMsgID, heartbeat, true)
				continue
			}

			rendered := buildSegment(prose, toolDisplay, phase, true)
			if forceProgress {
				elapsed := time.Since(streamStart)
				spin := string(spinChars[spinIdx%len(spinChars)])
				spinIdx++
				rendered = buildHeartbeatSegment(prose, toolDisplay, phase, spin, elapsed)
			}

			if utf8.RuneCountInString(prose) > telegramMaxMessageLen {
				// Keep chunk splitting based on prose, not status line rendering.
				splitAtRunes := telegramMaxMessageLen
				proseRunes := utf8.RuneCountInString(prose)
				if splitAtRunes > proseRunes {
					splitAtRunes = proseRunes
				}
				chunk, splitAtBytes := prefixRunes(prose, splitAtRunes)
				sendEdit(currentMsgID, chunk, false)
				msgStart += splitAtBytes
				needNewMsg = true
				continue
			}

			if needNewMsg {
				// Lazily create the next placeholder now that we have content for it.
				newMsg, sendErr := bot.Send(tgbotapi.NewMessage(chatID, "⏳"))
				if sendErr == nil {
					currentMsgID = newMsg.MessageID
					lastSentContent = ""
					lastEditTime = time.Time{}
					lastVisibleChange = time.Now()
				}
				needNewMsg = false
			}
			sendEdit(currentMsgID, rendered, forceProgress)
		case <-streamCtx.Done():
			// Distinguish server shutdown (parent ctx cancelled) from watchdog timeouts and user interrupt.
			if ctx.Err() != nil {
				// Server shutdown — existing behavior.
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

			if watchdogTimedOut.Load() {
				select {
				case streamErr = <-streamDone:
					streamDoneDrained = true
				default:
					streamErr = fmt.Errorf("stream timed out: no events for %s", watchdogTimeout)
				}
				break loop
			}

			// User interrupt: handled below after the loop exits.
			userInterrupted = true
			break loop
		}
	}

	if userInterrupted {
		// Close the stream and wait for the Recv goroutine to finish draining
		// anything already in-flight so history snapshots include the final
		// partial text and callback-produced messages from the interrupted turn.
		stream.Close()
		if !streamDoneDrained {
			<-streamDone
		}

		textMu.Lock()
		partial := textBuf.String()
		textMu.Unlock()
		producedMu.Lock()
		producedSnapshot := append([]llm.Message(nil), produced...)
		producedMu.Unlock()

		// Edit the Telegram message to show partial text + interrupted marker.
		display := ""
		if msgStart < len(partial) {
			display = partial[msgStart:]
		}
		if display == "" {
			display = "(interrupted)"
		} else {
			display += "\n\n_(interrupted)_"
		}
		if needNewMsg {
			newMsg, sendErr := bot.Send(tgbotapi.NewMessage(chatID, "⏳"))
			if sendErr == nil {
				currentMsgID = newMsg.MessageID
			}
		}
		sendEdit(currentMsgID, display, true)

		// Preserve partial history so conversation context isn't lost.
		newHistory := make([]llm.Message, 0, len(sess.history)+2+len(producedSnapshot))
		newHistory = append(newHistory, sess.history...)
		newHistory = append(newHistory, normalizeUserMessageForHistory(userMsg))
		newHistory = append(newHistory, producedSnapshot...)
		// If we have partial text but no tool turns completed, save it.
		if len(producedSnapshot) == 0 && partial != "" {
			newHistory = append(newHistory, llm.AssistantText(partial))
		}
		sess.history = newHistory
		sess.lastActivity = time.Now()

		if m.store != nil && sess.meta != nil {
			m.runStoreOpWithTimeout(sess.meta.ID, "UpdateStatus(interrupted)", func(storeCtx context.Context) error {
				return m.store.UpdateStatus(storeCtx, sess.meta.ID, session.StatusInterrupted)
			})
		}
		return nil
	}

	if streamErr != nil {
		if strings.Contains(streamErr.Error(), "stream timed out") {
			_, _ = bot.Send(tgbotapi.NewMessage(chatID, "⌛ Response timed out — please try again."))
		}
		if m.store != nil && sess.meta != nil {
			m.runStoreOpWithTimeout(sess.meta.ID, "UpdateStatus(stream_error)", func(storeCtx context.Context) error {
				return m.store.UpdateStatus(storeCtx, sess.meta.ID, session.StatusError)
			})
		}
		return streamErr
	}

	// Final edit: show full remaining text without cursor.
	textMu.Lock()
	full, ran := textBuf.String(), toolsRan
	finalTextDeltas := textDeltas
	finalReasoningDeltas := reasoningDeltas
	finalToolStarts := toolStarts
	finalToolEnds := toolEnds
	finalToolCalls := toolCalls
	finalPhaseEvents := phaseEvents
	finalUsageEvents := usageEvents
	finalDoneEvents := doneEvents
	finalRetryEvents := retryEvents
	finalErrorEvents := errorEvents
	finalOtherEvents := otherEvents
	finalOtherTypes := make(map[llm.EventType]int, len(otherTypes))
	for k, v := range otherTypes {
		finalOtherTypes[k] = v
	}
	imagesToSend := append([]string(nil), collectedImages...)
	textMu.Unlock()

	prose := ""
	if msgStart < len(full) {
		prose = full[msgStart:]
	}
	switch {
	case prose != "":
		// There is new content to show in the current window.
		// If a lazy placeholder was pending, create it first.
		if needNewMsg {
			newMsg, sendErr := bot.Send(tgbotapi.NewMessage(chatID, "⏳"))
			if sendErr == nil {
				currentMsgID = newMsg.MessageID
			}
		}
		sendEdit(currentMsgID, prose, true)
	case full == "":
		// Nothing was produced at all — show a fallback in the original placeholder.
		if ran {
			sendEdit(currentMsgID, "(done)", true)
		} else {
			sendEdit(currentMsgID, "(no response)", true)
		}
		if m.settings.Debug || m.settings.DebugRaw {
			log.Printf("[telegram] empty assistant text for chat %d (toolsRan=%v, text_delta=%d, reasoning_delta=%d, tool_start=%d, tool_end=%d, tool_call=%d, phase=%d, usage=%d, done=%d, retry=%d, error=%d, other=%d, other_types=%v)",
				chatID,
				ran,
				finalTextDeltas,
				finalReasoningDeltas,
				finalToolStarts,
				finalToolEnds,
				finalToolCalls,
				finalPhaseEvents,
				finalUsageEvents,
				finalDoneEvents,
				finalRetryEvents,
				finalErrorEvents,
				finalOtherEvents,
				finalOtherTypes,
			)
		}
		// else: prose=="" but full!="", all content already shown in previous message(s).
	}

	// Send collected images as Telegram photo messages.
	for _, imgPath := range imagesToSend {
		imgData, readErr := os.ReadFile(imgPath)
		if readErr != nil {
			log.Printf("[telegram] failed to read image %s: %v", imgPath, readErr)
			continue
		}
		photoMsg := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{
			Name:  imgPath,
			Bytes: imgData,
		})
		if _, sendErr := bot.Send(photoMsg); sendErr != nil {
			log.Printf("[telegram] failed to send image %s: %v", imgPath, sendErr)
		}
	}

	// Persist history: base + user message + produced (assistant + tool results).
	newHistory := make([]llm.Message, 0, len(sess.history)+2+len(produced))
	newHistory = append(newHistory, sess.history...)
	newHistory = append(newHistory, normalizeUserMessageForHistory(userMsg))
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

// extractPlainTextFromMsg returns text/caption from a Telegram message.
func extractPlainTextFromMsg(msg *tgbotapi.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Text != "" {
		return msg.Text
	}
	if msg.Caption != "" {
		return msg.Caption
	}
	return ""
}

// collectUserText extracts the text content from a user message for persistence.
func collectUserText(msg llm.Message) string {
	var parts []string
	for _, p := range msg.Parts {
		if p.Type == llm.PartText && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func normalizeUserMessageForHistory(msg llm.Message) llm.Message {
	text := extractMessageTextWithPlaceholders(msg)
	if text == "" {
		return llm.UserText("")
	}
	return llm.UserText(text)
}

func buildHistoryContextTail(history []llm.Message, maxChars int) string {
	if maxChars <= 0 || len(history) == 0 {
		return ""
	}
	var lines []string
	for _, msg := range history {
		text := strings.TrimSpace(extractMessageTextWithPlaceholders(msg))
		if text == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] %s", msg.Role, text))
	}
	if len(lines) == 0 {
		return ""
	}
	return tailRunes(strings.Join(lines, "\n"), maxChars)
}

func extractToolResultTextWithPlaceholders(result *llm.ToolResult) string {
	if result == nil {
		return ""
	}

	var parts []string
	// Prefer structured ContentParts when present; fall back to flat Content
	// only when absent. Tools that populate ContentParts typically set
	// Content to the flattened text form of the text parts (see view_image),
	// so consuming both would duplicate text and waste carryover budget.
	if len(result.ContentParts) > 0 {
		for _, p := range result.ContentParts {
			switch p.Type {
			case llm.ToolContentPartText:
				if strings.TrimSpace(p.Text) != "" {
					parts = append(parts, p.Text)
				}
			case llm.ToolContentPartImageData:
				parts = append(parts, "[image uploaded]")
			}
		}
	} else if strings.TrimSpace(result.Content) != "" {
		parts = append(parts, result.Content)
	}
	for range result.Images {
		parts = append(parts, "[image uploaded]")
	}
	return strings.Join(parts, "\n")
}

func extractMessageTextWithPlaceholders(msg llm.Message) string {
	var parts []string
	for _, part := range msg.Parts {
		switch part.Type {
		case llm.PartText:
			if strings.TrimSpace(part.Text) != "" {
				parts = append(parts, part.Text)
			}
		case llm.PartImage:
			parts = append(parts, "[image uploaded]")
		case llm.PartToolResult:
			if text := extractToolResultTextWithPlaceholders(part.ToolResult); strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func tailRunes(s string, maxRunes int) string {
	if maxRunes <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[len(runes)-maxRunes:])
}

func prefixRunes(s string, maxRunes int) (string, int) {
	if maxRunes <= 0 || s == "" {
		return "", 0
	}

	runeCount := 0
	for idx := range s {
		if runeCount == maxRunes {
			return s[:idx], idx
		}
		runeCount++
	}

	return s, len(s)
}

// activeToolDisplay returns a short human-readable string describing which tools
// are currently executing. It is called under textMu.
func activeToolDisplay(tools map[string]string) string {
	switch len(tools) {
	case 0:
		return ""
	case 1:
		for _, name := range tools {
			return name
		}
	}
	return fmt.Sprintf("%d tools running...", len(tools))
}

// buildSegment formats the display string for a Telegram message window.
// prose is the accumulated text for this window.
// tool is the currently-running tool display string ("" if none).
// phase is the most-recent phase string ("" if none).
// withCursor appends the streaming cursor ▌.
func buildSegment(prose, tool, phase string, withCursor bool) string {
	var sb strings.Builder
	sb.WriteString(prose)
	if tool != "" {
		if prose != "" {
			sb.WriteString("\n\n")
		}
		sb.WriteString("🔧 ")
		sb.WriteString(tool)
		sb.WriteString("...")
	} else if phase != "" {
		if prose != "" {
			sb.WriteString("\n\n")
		}
		sb.WriteString(phase)
	}
	if withCursor {
		sb.WriteString("▌")
	}
	return sb.String()
}

// buildHeartbeatSegment is like buildSegment but adds a spinner and elapsed timer.
func buildHeartbeatSegment(prose, tool, phase, spin string, elapsed time.Duration) string {
	var sb strings.Builder
	sb.WriteString(prose)

	statusLine := ""
	if tool != "" {
		statusLine = "🔧 " + tool + "..."
	} else if phase != "" {
		statusLine = phase
	} else {
		statusLine = "⏳ Thinking"
	}
	if statusLine != "" {
		if prose != "" {
			sb.WriteString("\n\n")
		}
		sb.WriteString(statusLine)
	}

	if prose != "" || statusLine != "" {
		sb.WriteString("\n\n")
	}
	sb.WriteString(spin)
	sb.WriteString(" ")
	sb.WriteString(formatElapsed(elapsed))

	return sb.String()
}

func formatElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	totalSec := int(elapsed.Seconds())
	hours := totalSec / 3600
	mins := (totalSec % 3600) / 60
	secs := totalSec % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}
