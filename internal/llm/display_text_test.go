package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestPlatformModeSurvivesReconstructionButNotProviderProjection(t *testing.T) {
	source := []core.Message{PlatformContextMessageForMode("voice rules", "live"), core.UserText("spoken request")}
	reconstructed := InsertPlatformContext([]core.Message{core.UserText("compacted summary")}, source)
	context, ok := PlatformContextFrom(reconstructed)
	if !ok || PlatformContextMode(context) != "live" {
		t.Fatalf("reconstructed mode = %+v", reconstructed)
	}
	wire := providerProjection(t, []core.Message{context})
	if len(wire) != 1 || len(wire[0].Parts) != 1 || core.MessageText(wire[0]) != "voice rules" {
		t.Fatalf("provider projection = %+v", wire)
	}
	if PlatformContextMode(wire[0]) != "" {
		t.Fatal("mode metadata reached provider")
	}
}
