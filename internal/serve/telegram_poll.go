package serve

import (
	"context"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// telegramPollState is the next Telegram acknowledgement boundary. An update
// advances it only after the platform accepts ownership; no hidden library
// goroutine may acknowledge a prefetched batch during process drain.
type telegramPollState struct {
	NextOffset int `json:"next_offset"`
}

type telegramPollClient struct {
	ctx    context.Context
	client tgbotapi.HTTPClient
}

func (c telegramPollClient) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(request.Clone(c.ctx))
}

// runTelegramPolling has one synchronous fetch/admission owner. The bot copy
// shares its HTTP client but binds ONLY polling requests to this context; normal
// message delivery retains the original bot and its own existing lifetime.
// accept=false leaves that update unacknowledged for a later poll/resumption.
func runTelegramPolling(ctx context.Context, bot *tgbotapi.BotAPI, state *telegramPollState, accept func(tgbotapi.Update) bool) error {
	polling := *bot
	polling.Client = telegramPollClient{ctx: ctx, client: bot.Client}
	for ctx.Err() == nil {
		config := tgbotapi.NewUpdate(state.NextOffset)
		config.Timeout = telegramUpdateLongPollSeconds
		updates, err := polling.GetUpdates(config)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Transport errors can contain the bot token in their URL. Do not log it.
			log.Printf("[telegram] update polling failed (%T); retrying", err)
			timer := time.NewTimer(3 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			continue
		}
		for _, update := range updates {
			if update.UpdateID < state.NextOffset {
				continue
			}
			if ctx.Err() != nil || !accept(update) {
				return nil
			}
			state.NextOffset = update.UpdateID + 1
		}
	}
	return nil
}
