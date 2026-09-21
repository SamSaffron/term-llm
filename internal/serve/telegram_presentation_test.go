package serve

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
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
