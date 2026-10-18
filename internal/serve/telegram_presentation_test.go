package serve

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
)

// codeHTMLBotSender validates the code/pre subset used by these fixtures. The
// strict decoder rejects unbalanced tags and unescaped ampersands; the allowlist
// rejects literal HTML/XML accidentally emitted as markup.
type codeHTMLBotSender struct{ *fakeBotSender }

func (f *codeHTMLBotSender) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	if edit, ok := c.(tgbotapi.EditMessageTextConfig); ok {
		if edit.ParseMode != tgbotapi.ModeHTML {
			return tgbotapi.Message{}, fmt.Errorf("expected HTML parse mode")
		}
		d := xml.NewDecoder(strings.NewReader(edit.Text))
		for {
			token, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return tgbotapi.Message{}, err
			}
			if tag, ok := token.(xml.StartElement); ok && tag.Name.Local != "code" && tag.Name.Local != "pre" {
				return tgbotapi.Message{}, fmt.Errorf("unsupported tag %q", tag.Name.Local)
			}
		}
	}
	return f.fakeBotSender.Send(c)
}

func TestTelegramPresentationDeliversEscapedCode(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(fmt.Sprintf("final=%t", final), func(t *testing.T) {
			bot := &codeHTMLBotSender{&fakeBotSender{}}
			p := newTelegramPresentation(bot, 123, 456, nil, time.Nanosecond)
			const content = "Use `<div>` here\n\n```xml\n<item>A & B &amp;</item>\n```"
			const want = "Use <code>&lt;div&gt;</code> here\n\n<pre>&lt;item&gt;A &amp; B &amp;amp;&lt;/item&gt;\n</pre>"
			if final {
				if err := p.finalizeText(context.Background(), content, false); err != nil {
					t.Fatalf("final delivery failed: %v", err)
				}
			} else if !p.sendEdit(456, content, true) {
				t.Fatal("streaming edit failed")
			}
			if edits := bot.allEditTexts(); len(edits) != 1 || edits[0] != want {
				t.Fatalf("delivered edits = %q, want [%q]", edits, want)
			}
			if p.lastSentContent != content {
				t.Fatalf("successful delivery not recorded: %q", p.lastSentContent)
			}
		})
	}
}

type recordedTelegramUpload struct {
	kind, name, caption, title string
	data                       []byte
	reader                     io.Reader
}
type mediaBotSender struct {
	*fakeBotSender
	uploads         []recordedTelegramUpload
	failNative      bool
	onNativeFailure func()
	beforeUpload    func(string)
}

func (b *mediaBotSender) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	var file tgbotapi.RequestFileData
	record := recordedTelegramUpload{}
	switch v := c.(type) {
	case tgbotapi.AudioConfig:
		record.kind = "audio"
		file = v.File
		record.caption = v.Caption
		record.title = v.Title
	case tgbotapi.VoiceConfig:
		record.kind = "voice"
		file = v.File
		record.caption = v.Caption
	case tgbotapi.DocumentConfig:
		record.kind = "document"
		file = v.File
		record.caption = v.Caption
	case tgbotapi.VideoConfig:
		record.kind = "video"
		file = v.File
		record.caption = v.Caption
	case tgbotapi.PhotoConfig:
		record.kind = "photo"
		file = v.File
		record.caption = v.Caption
	default:
		return b.fakeBotSender.Send(c)
	}
	upload, ok := file.(tgbotapi.FileReader)
	if !ok {
		return tgbotapi.Message{}, fmt.Errorf("not a reader: %T", file)
	}
	if b.beforeUpload != nil {
		b.beforeUpload(record.kind)
	}
	record.name = upload.Name
	record.reader = upload.Reader
	data, err := io.ReadAll(upload.Reader)
	if err != nil {
		return tgbotapi.Message{}, err
	}
	record.data = data
	b.uploads = append(b.uploads, record)
	if b.failNative && record.kind != "document" {
		b.failNative = false
		if b.onNativeFailure != nil {
			b.onNativeFailure()
		}
		return tgbotapi.Message{}, fmt.Errorf("native upload rejected")
	}
	return tgbotapi.Message{}, nil
}
func TestTelegramAudioDispatch(t *testing.T) {
	cases := []struct {
		mime, kind string
		data       []byte
	}{
		{"audio/mpeg", "audio", mediafixture.MP3()}, {"audio/mp4", "audio", mediafixture.MP4("M4A ", "soun")},
		{"audio/ogg", "voice", mediafixture.Ogg(true)}, {"audio/ogg", "document", mediafixture.Ogg(false)},
		{"audio/wav", "document", mediafixture.WAV()}, {"audio/flac", "document", mediafixture.FLAC()},
		{"audio/webm", "document", mediafixture.WebM(2)}, {"audio/ogg", "document", []byte("OggS not Opus")},
		{"video/mp4", "video", mediafixture.MP4("isom", "vide")}, {"image/png", "photo", []byte("\x89PNG\r\n\x1a\n")},
	}
	for _, c := range cases {
		t.Run(c.mime+"/"+c.kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.bin")
			if err := os.WriteFile(path, c.data, 0600); err != nil {
				t.Fatal(err)
			}
			bot := &mediaBotSender{fakeBotSender: &fakeBotSender{}}
			p := &telegramPresentation{bot: bot, chatID: 123}
			p.deliverMedia(nil, []core.MediaArtifact{{SourcePath: path, MediaType: c.mime, Name: "/private/narration.bin", Caption: "A caption"}})
			if len(bot.uploads) != 1 {
				t.Fatalf("uploads=%+v", bot.uploads)
			}
			u := bot.uploads[0]
			if u.kind != c.kind || u.caption != "A caption" || !bytes.Equal(u.data, c.data) || strings.ContainsAny(u.name, "/\\") || !strings.HasPrefix(u.name, "narration.") {
				t.Fatalf("upload=%+v", u)
			}
			if c.kind == "audio" && u.title == "" {
				t.Fatal("audio title missing")
			}
			if _, err := u.reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("upload handle not closed: %v", err)
			}
		})
	}
}
func TestTelegramNativeMediaFallbackReopens(t *testing.T) {
	for mime, data := range map[string][]byte{"audio/mpeg": mediafixture.MP3(), "audio/mp4": mediafixture.MP4("M4A "), "audio/ogg": mediafixture.Ogg(true), "video/mp4": mediafixture.MP4("isom")} {
		t.Run(mime, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audio")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			bot := &mediaBotSender{fakeBotSender: &fakeBotSender{}, failNative: true}
			p := &telegramPresentation{bot: bot}
			p.deliverMedia(nil, []core.MediaArtifact{{SourcePath: path, MediaType: mime, Caption: "plain <caption>"}})
			if len(bot.uploads) != 2 || bot.uploads[1].kind != "document" || !bytes.Equal(bot.uploads[1].data, data) {
				t.Fatalf("fallback=%+v", bot.uploads)
			}
			for _, u := range bot.uploads {
				if _, err := u.reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("unclosed reader: %v", err)
				}
			}
		})
	}
}
func TestTelegramAudioOversizeAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.mp3")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 51<<20); err != nil {
		t.Fatal(err)
	}
	bot := &mediaBotSender{fakeBotSender: &fakeBotSender{}}
	p := &telegramPresentation{bot: bot}
	p.deliverMedia(nil, []core.MediaArtifact{{SourcePath: path, MediaType: "audio/mpeg"}, {SourcePath: "missing", MediaType: "audio/wav"}})
	if len(bot.uploads) != 0 || !strings.Contains(bot.lastText(), "media.mp3 was not sent") || !strings.Contains(bot.lastText(), "50 MiB") || strings.Contains(bot.lastText(), path) {
		t.Fatalf("oversize=%s", bot.lastText())
	}
	if got := extractToolResultTextWithPlaceholders(&core.ToolResult{Media: []core.MediaArtifact{{MediaType: "audio/mpeg"}}}); got != "[audio uploaded]" {
		t.Fatalf("placeholder=%q", got)
	}
}

func TestTelegramFallbackRechecksFile(t *testing.T) {
	for _, replacement := range []string{"oversized", "directory", "missing"} {
		t.Run(replacement, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recording.mp3")
			if err := os.WriteFile(path, mediafixture.MP3(), 0600); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			bot := &mediaBotSender{
				fakeBotSender: &fakeBotSender{}, failNative: true,
				beforeUpload: func(string) { attempts++ },
				onNativeFailure: func() {
					switch replacement {
					case "oversized":
						if err := os.Truncate(path, telegramMaxMediaUploadBytes+1); err != nil {
							t.Fatal(err)
						}
					case "directory":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					case "missing":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					}
				},
			}
			p := &telegramPresentation{bot: bot}
			p.deliverArtifact(core.MediaArtifact{SourcePath: path, MediaType: "audio/mpeg"})
			if attempts != 1 {
				t.Fatalf("unsafe document fallback attempted; uploads=%d", attempts)
			}
			for _, u := range bot.uploads {
				if _, err := u.reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("unclosed reader: %v", err)
				}
			}
		})
	}
}
