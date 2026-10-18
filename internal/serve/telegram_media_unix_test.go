//go:build unix

package serve

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
)

func assertTelegramRejectsFIFO(t *testing.T, fifo string, p *telegramPresentation, media core.MediaArtifact) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.deliverArtifact(media)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Release a buggy blocking open so the test does not leak a goroutine.
		fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = syscall.Close(fd)
		}
		<-done
		t.Fatal("Telegram delivery blocked opening a FIFO")
	}
}

func TestTelegramRejectsFIFOBeforeOpen(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "recording.mp3")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	bot := &mediaBotSender{fakeBotSender: &fakeBotSender{}}
	p := &telegramPresentation{bot: bot}
	assertTelegramRejectsFIFO(t, fifo, p, core.MediaArtifact{SourcePath: fifo, MediaType: "audio/mpeg"})
	if len(bot.uploads) != 0 {
		t.Fatal("FIFO uploaded")
	}
}

func TestTelegramFallbackRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recording.mp3")
	if err := os.WriteFile(path, mediafixture.MP3(), 0600); err != nil {
		t.Fatal(err)
	}
	var replaceErr error
	bot := &mediaBotSender{
		fakeBotSender: &fakeBotSender{}, failNative: true,
		onNativeFailure: func() {
			replaceErr = os.Remove(path)
			if replaceErr == nil {
				replaceErr = syscall.Mkfifo(path, 0600)
			}
		},
	}
	p := &telegramPresentation{bot: bot}
	assertTelegramRejectsFIFO(t, path, p, core.MediaArtifact{SourcePath: path, MediaType: "audio/mpeg"})
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	if len(bot.uploads) != 1 {
		t.Fatalf("uploads=%d want only native attempt", len(bot.uploads))
	}
}
