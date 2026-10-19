package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mediatype"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

// telegramPresentation owns the message window and send/edit policy for one
// reply. Stream/session lifecycle, persistence, timers, and cancellation remain
// with streamReplyContinuation.
type telegramPresentation struct {
	bot                    botSender
	chatID                 int64
	editInterval           time.Duration
	currentMsgID           int
	msgStart               int // byte offset in the accumulated text
	needNewMsg             bool
	lastSentContent        string
	lastEditTime           time.Time
	lastSuccessfulEditTime time.Time
	lastVisibleChange      time.Time
	streamStart            time.Time
	spinChars              []rune
	spinIdx                int
}

func newTelegramPresentation(bot botSender, chatID int64, messageID int, resume *telegramContinuation, editInterval time.Duration) *telegramPresentation {
	if editInterval <= 0 {
		editInterval = minEditInterval
	}
	p := &telegramPresentation{bot: bot, chatID: chatID, editInterval: editInterval, currentMsgID: messageID, lastVisibleChange: time.Now(), streamStart: time.Now(), spinChars: []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")}
	if resume != nil {
		p.msgStart = resume.MessageStart
		p.needNewMsg = resume.NeedNewMessage
	}
	return p
}

func (p *telegramPresentation) checkpoint() (messageID, messageStart int, needNew bool) {
	return p.currentMsgID, p.msgStart, p.needNewMsg
}
func (p *telegramPresentation) remaining(full string) string {
	if p.msgStart < len(full) {
		return full[p.msgStart:]
	}
	return ""
}

func (p *telegramPresentation) sendEdit(msgID int, content string, force bool) bool {
	content = telegramMediaMarkdownPattern.ReplaceAllString(content, "$1")
	if !force && content == p.lastSentContent {
		return false
	}
	if !force && !p.lastEditTime.IsZero() && time.Since(p.lastEditTime) < p.editInterval {
		return false
	}
	edit := tgbotapi.NewEditMessageText(p.chatID, msgID, mdToTelegramHTML(content))
	edit.ParseMode = tgbotapi.ModeHTML
	if _, err := p.bot.Send(edit); err != nil {
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "Too Many Requests") {
			log.Printf("[telegram] edit rate limited (chat %d): %v", p.chatID, err)
		}
		return false
	}
	changed := content != p.lastSentContent
	p.lastSentContent = content
	p.lastEditTime = time.Now()
	p.lastSuccessfulEditTime = p.lastEditTime
	if changed {
		p.lastVisibleChange = p.lastEditTime
	}
	return true
}
func (p *telegramPresentation) ensureCurrentMessage() bool {
	if !p.needNewMsg {
		return true
	}
	msg, err := p.bot.Send(tgbotapi.NewMessage(p.chatID, "⏳"))
	if err != nil {
		log.Printf("[telegram] failed to send continuation placeholder (chat %d): %v", p.chatID, err)
		return false
	}
	p.currentMsgID = msg.MessageID
	p.lastSentContent = ""
	p.lastEditTime = time.Time{}
	p.lastVisibleChange = time.Now()
	p.needNewMsg = false
	return true
}
func (p *telegramPresentation) sendProseChunks(prose string, force bool) (string, bool) {
	for utf8.RuneCountInString(prose) > telegramMaxMessageLen {
		if !p.ensureCurrentMessage() {
			return prose, false
		}
		chunk, split := telegramProseChunk(prose)
		if !p.sendEdit(p.currentMsgID, chunk, force) {
			return prose, false
		}
		p.msgStart += split
		prose = prose[split:]
		p.needNewMsg = true
	}
	return prose, true
}
func (p *telegramPresentation) renderTick(full, toolDisplay, phase string) {
	prose := p.remaining(full)
	force := time.Since(p.lastVisibleChange) >= 12*time.Second
	if prose == "" && toolDisplay == "" && phase == "" {
		if !force {
			return
		}
		spin := string(p.spinChars[p.spinIdx%len(p.spinChars)])
		p.spinIdx++
		p.sendEdit(p.currentMsgID, buildHeartbeatSegment("", toolDisplay, phase, spin, time.Since(p.streamStart)), true)
		return
	}
	var ok bool
	prose, ok = p.sendProseChunks(prose, false)
	if !ok {
		return
	}
	rendered := buildSegment(prose, toolDisplay, phase, true)
	if force {
		spin := string(p.spinChars[p.spinIdx%len(p.spinChars)])
		p.spinIdx++
		rendered = buildHeartbeatSegment(prose, toolDisplay, phase, spin, time.Since(p.streamStart))
	}
	if p.ensureCurrentMessage() {
		p.sendEdit(p.currentMsgID, rendered, force)
	}
}
func waitForTelegramDelivery(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *telegramPresentation) sendFinal(ctx context.Context, value tgbotapi.Chattable) (tgbotapi.Message, error) {
	var lastErr error
	for attempt := 1; attempt <= telegramFinalDeliveryMaxAttempts; attempt++ {
		if !p.lastSuccessfulEditTime.IsZero() {
			if err := waitForTelegramDelivery(ctx, time.Until(p.lastSuccessfulEditTime.Add(p.editInterval))); err != nil {
				return tgbotapi.Message{}, fmt.Errorf("pace final Telegram delivery: %w", err)
			}
		}
		msg, err := p.bot.Send(value)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		if !telegramSendErrorRetryable(err) {
			return tgbotapi.Message{}, fmt.Errorf("final Telegram delivery failed: %w", err)
		}
		if attempt == telegramFinalDeliveryMaxAttempts {
			break
		}
		delay := telegramSendRetryDelay(err, p.editInterval)
		log.Printf("[telegram] transient final delivery failure (chat %d, attempt %d/%d), retrying in %s: %v", p.chatID, attempt, telegramFinalDeliveryMaxAttempts, delay, err)
		if err := waitForTelegramDelivery(ctx, delay); err != nil {
			return tgbotapi.Message{}, fmt.Errorf("final Telegram delivery retry interrupted (%v): %w", lastErr, err)
		}
	}
	return tgbotapi.Message{}, fmt.Errorf("final Telegram delivery failed after %d attempts: %w", telegramFinalDeliveryMaxAttempts, lastErr)
}
func (p *telegramPresentation) sendFinalEdit(ctx context.Context, content string) error {
	content = telegramMediaMarkdownPattern.ReplaceAllString(content, "$1")
	if content == p.lastSentContent {
		return nil
	}
	edit := tgbotapi.NewEditMessageText(p.chatID, p.currentMsgID, mdToTelegramHTML(content))
	edit.ParseMode = tgbotapi.ModeHTML
	if _, err := p.sendFinal(ctx, edit); err != nil {
		return err
	}
	changed := content != p.lastSentContent
	p.lastSentContent = content
	p.lastEditTime = time.Now()
	p.lastSuccessfulEditTime = p.lastEditTime
	if changed {
		p.lastVisibleChange = p.lastEditTime
	}
	return nil
}
func (p *telegramPresentation) ensureFinalCurrentMessage(ctx context.Context) error {
	if !p.needNewMsg {
		return nil
	}
	msg, err := p.sendFinal(ctx, tgbotapi.NewMessage(p.chatID, "⏳"))
	if err != nil {
		return fmt.Errorf("send continuation placeholder: %w", err)
	}
	p.currentMsgID = msg.MessageID
	p.lastSentContent = ""
	p.lastEditTime = time.Time{}
	p.lastVisibleChange = time.Now()
	p.needNewMsg = false
	return nil
}
func (p *telegramPresentation) sendFinalProseChunks(ctx context.Context, prose string) (string, error) {
	for utf8.RuneCountInString(prose) > telegramMaxMessageLen {
		if err := p.ensureFinalCurrentMessage(ctx); err != nil {
			return prose, err
		}
		chunk, split := telegramProseChunk(prose)
		if err := p.sendFinalEdit(ctx, chunk); err != nil {
			return prose, err
		}
		p.msgStart += split
		prose = prose[split:]
		p.needNewMsg = true
	}
	return prose, nil
}
func (p *telegramPresentation) renderInterrupted(partial string) {
	display := p.remaining(partial)
	if display == "" {
		display = "(interrupted)"
	} else {
		display += "\n\n_(interrupted)_"
	}
	if p.needNewMsg {
		if msg, err := p.bot.Send(tgbotapi.NewMessage(p.chatID, "⏳")); err == nil {
			p.currentMsgID = msg.MessageID
		}
	}
	p.sendEdit(p.currentMsgID, display, true)
}
func (p *telegramPresentation) deliverMedia(images []string, mediaItems []core.MediaArtifact) {
	for _, path := range images {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[telegram] failed to read image %s: %v", path, err)
			continue
		}
		message := tgbotapi.NewPhoto(p.chatID, tgbotapi.FileBytes{Name: path, Bytes: data})
		if _, err := p.bot.Send(message); err != nil {
			log.Printf("[telegram] failed to send image %s: %v", path, err)
		}
	}
	for _, media := range mediaItems {
		p.deliverArtifact(media)
	}
}

var errTelegramMediaTooLarge = errors.New("media exceeds Telegram's 50 MiB upload limit")

// openTelegramMedia rejects non-regular files before opening (in particular,
// FIFOs), then verifies that the handle still refers to the checked file.
func openTelegramMedia(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat media: %w", err)
	}
	if err := checkTelegramMediaInfo(info); err != nil {
		return nil, nil, err
	}
	upload, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open media: %w", err)
	}
	opened, err := upload.Stat()
	if err == nil {
		err = checkTelegramMediaInfo(opened)
	}
	if err == nil && !os.SameFile(info, opened) {
		err = errors.New("media changed while opening")
	}
	if err != nil {
		_ = upload.Close()
		return nil, nil, err
	}
	return upload, opened, nil
}

func checkTelegramMediaInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("media is not a regular file")
	}
	if info.Size() > telegramMaxMediaUploadBytes {
		return errTelegramMediaTooLarge
	}
	return nil
}

func (p *telegramPresentation) uploadArtifact(media core.MediaArtifact, document bool) (string, error) {
	upload, info, err := openTelegramMedia(media.PublishedPath())
	if err != nil {
		return "", err
	}
	defer upload.Close()
	kind := "document"
	if !document {
		kind = telegramUploadKind(media.MediaType, upload, info.Size())
	}
	return kind, p.sendMediaUpload(media, upload, kind)
}

func (p *telegramPresentation) reportOversizedMedia(media core.MediaArtifact) {
	label := telegramArtifactFilename(media)
	if _, err := p.bot.Send(tgbotapi.NewMessage(p.chatID, fmt.Sprintf("%s was not sent because it exceeds Telegram's 50 MiB upload limit.", label))); err != nil {
		log.Printf("[telegram] failed to report oversized media: %v", err)
	}
}

func (p *telegramPresentation) deliverArtifact(media core.MediaArtifact) {
	kind, err := p.uploadArtifact(media, false)
	if err == nil {
		return
	}
	if errors.Is(err, errTelegramMediaTooLarge) {
		p.reportOversizedMedia(media)
		return
	}
	log.Printf("[telegram] media upload (%s) failed: %v", kind, err)
	if kind != "video" && kind != "audio" && kind != "voice" {
		return
	}
	// Reopen and revalidate rather than reusing a consumed native upload reader.
	if _, err := p.uploadArtifact(media, true); err != nil {
		if errors.Is(err, errTelegramMediaTooLarge) {
			p.reportOversizedMedia(media)
		}
		log.Printf("[telegram] document fallback failed: %v", err)
	}
}

func telegramUploadKind(mime string, upload *os.File, size int64) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch mime {
	case "audio/mpeg", "audio/mp4":
		return "audio"
	case "audio/ogg":
		if opus, err := mediatype.IsOggOpus(upload, size); err == nil && opus {
			return "voice"
		}
		return "document"
	}
	if strings.HasPrefix(mime, "image/") {
		return "photo"
	}
	if strings.HasPrefix(mime, "video/") {
		return "video"
	}
	return "document"
}

func (p *telegramPresentation) sendMediaUpload(media core.MediaArtifact, upload *os.File, kind string) error {
	file := tgbotapi.FileReader{Name: telegramArtifactFilename(media), Reader: upload}
	caption := telegramMediaText(media.Caption, 1024)
	var message tgbotapi.Chattable
	switch kind {
	case "photo":
		c := tgbotapi.NewPhoto(p.chatID, file)
		c.Caption = caption
		message = c
	case "video":
		c := tgbotapi.NewVideo(p.chatID, file)
		c.Caption = caption
		message = c
	case "audio":
		c := tgbotapi.NewAudio(p.chatID, file)
		c.Caption = caption
		if strings.TrimSpace(media.Name) != "" {
			c.Title = telegramMediaText(filepath.Base(strings.ReplaceAll(media.Name, "\\", "/")), 64)
		}
		if c.Title == "" {
			c.Title = telegramMediaText(media.Caption, 64)
		}
		if c.Title == "" {
			c.Title = "Audio recording"
		}
		message = c
	case "voice":
		c := tgbotapi.NewVoice(p.chatID, file)
		c.Caption = caption
		message = c
	default:
		c := tgbotapi.NewDocument(p.chatID, file)
		c.Caption = caption
		message = c
	}
	_, err := p.bot.Send(message)
	return err
}

func telegramMediaText(value string, limit int) string {
	value = terminaltext.SanitizeSingleLine(strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

func telegramArtifactFilename(media core.MediaArtifact) string {
	name := telegramMediaText(filepath.Base(strings.ReplaceAll(media.Name, "\\", "/")), 180)
	if name == "" || name == "." || name == ".." {
		return telegramMediaFilename(media.MediaType)
	}
	if format, ok := mediatype.Lookup(media.MediaType); ok {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + format.Extension
	}
	return name
}

func (p *telegramPresentation) finalizeText(ctx context.Context, full string, toolsRan bool) error {
	prose := telegramMediaMarkdownPattern.ReplaceAllString(p.remaining(full), "$1")
	if prose != "" {
		var err error
		prose, err = p.sendFinalProseChunks(ctx, prose)
		if err != nil {
			return err
		}
		if err = p.ensureFinalCurrentMessage(ctx); err != nil {
			return err
		}
		return p.sendFinalEdit(ctx, prose)
	}
	if full == "" {
		fallback := "(no response)"
		if toolsRan {
			fallback = "(done)"
		}
		return p.sendFinalEdit(ctx, fallback)
	}
	return nil
}
