package llm

import "testing"

func TestProviderProjectionStripsDisplayMetadata(t *testing.T) {
	message := UserText("provider context")
	message.Parts = append(message.Parts, Part{Type: PartDisplayText, Text: "visible request"})
	got := providerSafeRequestMessages([]Message{message})
	if len(got) != 1 || len(got[0].Parts) != 1 || MessageText(got[0]) != "provider context" {
		t.Fatalf("provider messages = %#v", got)
	}
	if len(message.Parts) != 2 {
		t.Fatal("source metadata mutated")
	}
}

func TestPlatformModeSurvivesReconstructionButNotProviderProjection(t *testing.T) {
	source := []Message{PlatformContextMessageForMode("voice rules", "live"), UserText("spoken request")}
	reconstructed := InsertPlatformContext([]Message{UserText("compacted summary")}, source)
	context, ok := PlatformContextFrom(reconstructed)
	if !ok || PlatformContextMode(context) != "live" {
		t.Fatalf("reconstructed mode = %+v", reconstructed)
	}
	wire := providerSafeRequestMessages([]Message{context})
	if len(wire) != 1 || len(wire[0].Parts) != 1 || MessageText(wire[0]) != "voice rules" {
		t.Fatalf("provider projection = %+v", wire)
	}
	if PlatformContextMode(wire[0]) != "" {
		t.Fatal("mode metadata reached provider")
	}
}
