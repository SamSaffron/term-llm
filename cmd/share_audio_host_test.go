package cmd

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/share"
)

func TestCreateSessionShareAudioConsent(t *testing.T) {
	for _, tc := range []struct {
		name, scope, choice string
		include             bool
	}{
		{"default response", "response", "", false},
		{"explicit exclusion", "response", `,"include_audio":false`, false},
		{"opt-in response", "response", `,"include_audio":true`, true},
		{"opt-in conversation without tools or images", "conversation", `,"include_audio":true,"include_tools":false,"include_images":false`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newServeShareImageFixture(t)
			anchor := addShareAudioFixture(t, server.store, "share-session")
			mock := assetProviderMock()
			server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
			rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"`+tc.scope+`"`+tc.choice+`}`)
			if rr.Code != 201 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if strings.Contains(mock.files["index.html"], "<audio controls") != tc.include {
				t.Fatalf("include_audio forwarding failed: %s", tc.name)
			}
			if strings.Contains(mock.files["session.md"], "data:audio/") || !strings.Contains(mock.files["session.md"], "Audio: narration.mp3") {
				t.Fatal("Markdown audio fallback missing")
			}
			if strings.Contains(tc.choice, "include_images") && strings.Contains(mock.files["index.html"], "<img") {
				t.Fatal("image exclusion ignored")
			}
		})
	}
}
