package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestTelegramReceiverPauseKeepsPlatformAliveAndResumesOffset(t *testing.T) {
	var polls21 atomic.Int32
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"fixture"}}`)
			return
		}
		r.ParseForm()
		offset, _ := strconv.Atoi(r.Form.Get("offset"))
		switch offset {
		case 20:
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":20}]}`)
		case 21:
			if polls21.Add(1) == 1 {
				close(blocked)
				<-r.Context().Done()
				return
			}
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":21}]}`)
		default:
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("fixture", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var receiver telegramReceiver
	if err := receiver.restore(20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := make(chan int, 4)
	done := make(chan error, 1)
	go func() {
		done <- receiver.run(ctx, bot, func(_ context.Context, update tgbotapi.Update) bool { accepted <- update.UpdateID; return true })
	}()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("poll did not reach offset 21")
	}
	pauseCtx, pauseCancel := context.WithTimeout(ctx, time.Second)
	defer pauseCancel()
	offset, err := receiver.pause(pauseCtx)
	if err != nil || offset != 21 {
		t.Fatalf("pause=%d %v", offset, err)
	}
	if ctx.Err() != nil {
		t.Fatal("receive pause cancelled platform handlers")
	}
	select {
	case <-done:
		t.Fatal("receive pause shut down platform")
	default:
	}
	if err := receiver.restore(99); err == nil {
		t.Fatal("live receive offset overwritten")
	}
	receiver.resume()
	for _, expected := range []int{20, 21} {
		select {
		case id := <-accepted:
			if id != expected {
				t.Fatalf("update=%d want=%d", id, expected)
			}
		case <-time.After(time.Second):
			t.Fatal("resumption did not deliver update")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("platform shutdown did not finish")
	}
}

type receiverIgnoringClient struct {
	entered, release chan struct{}
	calls            atomic.Int32
}

func (c *receiverIgnoringClient) Do(r *http.Request) (*http.Response, error) {
	body := `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"fixture"}}`
	if !strings.HasSuffix(r.URL.Path, "getMe") {
		if c.calls.Add(1) == 1 {
			close(c.entered)
			<-c.release
		}
		body = `{"ok":true,"result":[{"update_id":7}]}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestTelegramReceiverFailedPauseCanResumeBeforeOldPollReturns(t *testing.T) {
	client := &receiverIgnoringClient{entered: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(client.release)
		}
	}()
	bot, err := tgbotapi.NewBotAPIWithClient("fixture", "http://fixture/bot%s/%s", client)
	if err != nil {
		t.Fatal(err)
	}
	var receiver telegramReceiver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		done <- receiver.run(ctx, bot, func(_ context.Context, update tgbotapi.Update) bool {
			accepted <- update.UpdateID
			cancel()
			return true
		})
	}()
	<-client.entered
	pauseCtx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if _, err := receiver.pause(pauseCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unsettled poll reported success: %v", err)
	}
	receiver.resume() // rollback must not turn the late poll return into shutdown
	close(client.release)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rolled-back receiver did not resume")
	}
	select {
	case id := <-accepted:
		if id != 7 {
			t.Fatal(id)
		}
	default:
		t.Fatal("failed pause dropped unowned update")
	}
	if client.calls.Load() != 2 {
		t.Fatalf("poll ownership/retry calls=%d", client.calls.Load())
	}
}

func TestTelegramCleanupAcknowledgementWaitsForRuntimeExit(t *testing.T) {
	runnerDone := make(chan struct{})
	cleanupEntered := make(chan struct{})
	release := make(chan struct{})
	sess := &telegramSession{runnerDone: runnerDone, runtime: &SessionRuntime{Cleanup: func() { close(cleanupEntered); <-release }}}
	done := closeTelegramSessionWithTimeout(sess, time.Millisecond)
	select {
	case <-done:
		t.Fatal("cleanup acknowledged while runner owns runtime")
	default:
	}
	close(runnerDone)
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		t.Fatal("cleanup not started after runner exit")
	}
	select {
	case <-done:
		t.Fatal("cleanup acknowledged before Cleanup returned")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup acknowledgement missing")
	}
	again := closeTelegramSessionWithTimeout(sess, time.Millisecond)
	if again != done {
		t.Fatal("cleanup completion identity changed")
	}
}
