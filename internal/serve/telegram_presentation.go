package serve

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/llm"
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
func (p *telegramPresentation) deliverMedia(images []string, mediaItems []llm.MediaArtifact) {
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
		path := media.PublishedPath()
		info, err := os.Stat(path)
		if err != nil {
			log.Printf("[telegram] cannot access media %s: %v", path, err)
			continue
		}
		if !info.Mode().IsRegular() {
			log.Printf("[telegram] media is not a regular file: %s", path)
			continue
		}
		if info.Size() > telegramMaxMediaUploadBytes {
			label := strings.TrimSpace(media.Name)
			if label == "" {
				label = "video"
			}
			if _, err := p.bot.Send(tgbotapi.NewMessage(p.chatID, fmt.Sprintf("%s was not sent because it exceeds Telegram's 50 MiB upload limit.", label))); err != nil {
				log.Printf("[telegram] failed to report oversized media %s: %v", path, err)
			}
			continue
		}
		upload, err := os.Open(path)
		if err != nil {
			log.Printf("[telegram] cannot open media %s: %v", path, err)
			continue
		}
		file := tgbotapi.FileReader{Name: telegramMediaFilename(media.MediaType), Reader: upload}
		if strings.HasPrefix(strings.ToLower(media.MediaType), "image/") {
			message := tgbotapi.NewPhoto(p.chatID, file)
			message.Caption = media.Caption
			_, err = p.bot.Send(message)
			_ = upload.Close()
			if err != nil {
				log.Printf("[telegram] failed to send image media %s: %v", path, err)
			}
			continue
		}
		message := tgbotapi.NewVideo(p.chatID, file)
		message.Caption = media.Caption
		_, err = p.bot.Send(message)
		_ = upload.Close()
		if err != nil {
			log.Printf("[telegram] video send failed for %s, retrying as document: %v", path, err)
			fallback, openErr := os.Open(path)
			if openErr != nil {
				log.Printf("[telegram] cannot reopen media document %s: %v", path, openErr)
				continue
			}
			document := tgbotapi.NewDocument(p.chatID, tgbotapi.FileReader{Name: telegramMediaFilename(media.MediaType), Reader: fallback})
			document.Caption = media.Caption
			_, documentErr := p.bot.Send(document)
			_ = fallback.Close()
			if documentErr != nil {
				log.Printf("[telegram] failed to send media document %s: %v", path, documentErr)
			}
		}
	}
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
