package llm

import "testing"

func TestPlatformModeSurvivesReconstructionButNotProviderProjection(t *testing.T) {
	source := []Message{PlatformContextMessageForMode("voice rules", "live"), UserText("spoken request")}
	reconstructed := InsertPlatformContext([]Message{UserText("compacted summary")}, source)
	context, ok := PlatformContextFrom(reconstructed)
	if !ok || PlatformContextMode(context) != "live" {
		t.Fatalf("reconstructed mode = %+v", reconstructed)
	}
	wire := providerProjection(t, []Message{context})
	if len(wire) != 1 || len(wire[0].Parts) != 1 || MessageText(wire[0]) != "voice rules" {
		t.Fatalf("provider projection = %+v", wire)
	}
	if PlatformContextMode(wire[0]) != "" {
		t.Fatal("mode metadata reached provider")
	}
}
