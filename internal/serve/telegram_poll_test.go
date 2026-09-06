package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestTelegramPollingAcknowledgesOnlyOwnedUpdates(t *testing.T) {
	offsets := make(chan int, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"fixture"}}`)
			return
		}
		r.ParseForm()
		offset, _ := strconv.Atoi(r.Form.Get("offset"))
		offsets <- offset
		if offset == 0 {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":10},{"update_id":11}]}`)
		} else {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":10},{"update_id":11},{"update_id":12}]}`)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("fixture", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	state := &telegramPollState{}
	blocked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- runTelegramPolling(context.Background(), bot, state, func(update tgbotapi.Update) bool {
			if update.UpdateID == 11 {
				close(blocked)
				<-release
				return false
			}
			return true
		})
	}()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("admission not reached")
	}
	if offset := <-offsets; offset != 0 {
		t.Fatalf("initial offset=%d", offset)
	}
	select {
	case offset := <-offsets:
		close(release)
		t.Fatalf("prefetched/acknowledged blocked update: offset=%d", offset)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("polling did not stop at admission boundary")
	}
	if state.NextOffset != 11 {
		t.Fatalf("unowned update acknowledged: %d", state.NextOffset)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var accepted []int
	if err := runTelegramPolling(ctx, bot, state, func(update tgbotapi.Update) bool {
		accepted = append(accepted, update.UpdateID)
		if update.UpdateID == 12 {
			cancel()
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if offset := <-offsets; offset != 11 {
		t.Fatalf("resumed offset=%d", offset)
	}
	if len(accepted) != 2 || accepted[0] != 11 || accepted[1] != 12 || state.NextOffset != 13 {
		t.Fatalf("resumption dropped/replayed updates: accepted=%v next=%d", accepted, state.NextOffset)
	}
}

func TestTelegramPollingCancellationJoinsLongPoll(t *testing.T) {
	entered, left, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"fixture"}}`)
			return
		}
		r.ParseForm()
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		close(left)
	}))
	defer server.Close()
	defer close(release)
	bot, err := tgbotapi.NewBotAPIWithClient("fixture", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runTelegramPolling(ctx, bot, &telegramPollState{}, func(tgbotapi.Update) bool { t.Error("unexpected update"); return true })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("long poll not started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("long poll retained after owner cancellation")
	}
	select {
	case <-left:
	case <-time.After(time.Second):
		t.Fatal("HTTP long poll not cancelled")
	}
}
