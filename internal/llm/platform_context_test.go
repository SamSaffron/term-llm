package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestPlatformContextUsesLatestSourceAndSurvivesReconstruction(t *testing.T) {
	web := PlatformContextMessage("web context")
	chat := PlatformContextMessage("chat context")
	got := InsertPlatformContext([]core.Message{core.SystemText("system"), core.UserText("summary")}, []core.Message{web, core.UserText("old"), chat})
	if len(got) != 3 || !IsPlatformContextMessage(got[1]) || core.MessageText(got[1]) != "chat context" {
		t.Fatalf("messages = %#v", got)
	}
}

func TestProviderProjectionStripsPlatformMarkerButKeepsText(t *testing.T) {
	message := PlatformContextMessage("web context")
	got := providerProjection(t, []core.Message{message})
	if len(got) != 1 || len(got[0].Parts) != 1 || got[0].Parts[0].Type != core.PartText || got[0].Parts[0].Text != "web context" {
		t.Fatalf("provider messages = %#v", got)
	}
	if !IsPlatformContextMessage(message) {
		t.Fatalf("source mutated: %#v", message)
	}
}
