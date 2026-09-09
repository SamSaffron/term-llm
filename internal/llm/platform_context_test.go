package llm

import "testing"

func TestPlatformContextUsesLatestSourceAndSurvivesReconstruction(t *testing.T) {
	web := PlatformContextMessage("web context")
	chat := PlatformContextMessage("chat context")
	got := InsertPlatformContext([]Message{SystemText("system"), UserText("summary")}, []Message{web, UserText("old"), chat})
	if len(got) != 3 || !IsPlatformContextMessage(got[1]) || MessageText(got[1]) != "chat context" {
		t.Fatalf("messages = %#v", got)
	}
}

func TestProviderProjectionStripsPlatformMarkerButKeepsText(t *testing.T) {
	message := PlatformContextMessage("web context")
	got := providerSafeRequestMessages([]Message{message})
	if len(got) != 1 || len(got[0].Parts) != 1 || got[0].Parts[0].Type != PartText || got[0].Parts[0].Text != "web context" {
		t.Fatalf("provider messages = %#v", got)
	}
	if !IsPlatformContextMessage(message) {
		t.Fatalf("source mutated: %#v", message)
	}
}
