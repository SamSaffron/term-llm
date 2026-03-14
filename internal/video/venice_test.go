package video

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveModel(t *testing.T) {
	if got := ResolveModel("", false); got != veniceDefaultTextModel {
		t.Fatalf("ResolveModel text default = %q, want %q", got, veniceDefaultTextModel)
	}
	if got := ResolveModel("", true); got != veniceDefaultImageModel {
		t.Fatalf("ResolveModel image default = %q, want %q", got, veniceDefaultImageModel)
	}
	if got := ResolveModel("custom-model", true); got != "custom-model" {
		t.Fatalf("ResolveModel explicit = %q, want custom-model", got)
	}
}

func TestValidateEnums(t *testing.T) {
	if err := ValidateDuration("5s"); err != nil {
		t.Fatalf("ValidateDuration(5s): %v", err)
	}
	if err := ValidateDuration("3s"); err == nil {
		t.Fatal("expected invalid duration error")
	}
	if err := ValidateResolution("720p"); err != nil {
		t.Fatalf("ValidateResolution(720p): %v", err)
	}
	if err := ValidateResolution("4k"); err == nil {
		t.Fatal("expected invalid resolution error")
	}
	if err := ValidateAspectRatio("16:9"); err != nil {
		t.Fatalf("ValidateAspectRatio(16:9): %v", err)
	}
	if err := ValidateAspectRatio("wide"); err == nil {
		t.Fatal("expected invalid aspect ratio error")
	}
}

func TestVeniceProviderQuoteQueueRetrieve(t *testing.T) {
	var queueSawImageURL bool
	retrieveCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case veniceVideoQuoteEndpoint:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != "longcat-distilled-image-to-video" {
				t.Fatalf("quote model = %v", body["model"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quote":0.09}`))
		case veniceVideoQueueEndpoint:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if imageURL, ok := body["image_url"].(string); ok && strings.HasPrefix(imageURL, "data:image/png;base64,") {
				queueSawImageURL = true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"venice-video-model","queue_id":"queue-123"}`))
		case veniceVideoRetrieveEndpoint:
			retrieveCalls++
			if retrieveCalls == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"PROCESSING","average_execution_time":120000,"execution_duration":15000}`))
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("fake-mp4-data"))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := NewVeniceProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()

	quote, err := provider.Quote(context.Background(), Request{
		Model:       veniceDefaultImageModel,
		Duration:    "5s",
		AspectRatio: "16:9",
		Resolution:  "720p",
	})
	if err != nil {
		t.Fatalf("Quote error: %v", err)
	}
	if quote.Amount != 0.09 {
		t.Fatalf("quote = %v, want 0.09", quote.Amount)
	}

	job, err := provider.Queue(context.Background(), Request{
		Model:       veniceDefaultImageModel,
		Prompt:      "romeo being adorable",
		Duration:    "5s",
		AspectRatio: "16:9",
		Resolution:  "720p",
		ImagePath:   "romeo.png",
		ImageData:   []byte{0x89, 'P', 'N', 'G'},
	})
	if err != nil {
		t.Fatalf("Queue error: %v", err)
	}
	if job.QueueID != "queue-123" || job.Model != "venice-video-model" {
		t.Fatalf("job = %+v", job)
	}
	if !queueSawImageURL {
		t.Fatal("expected queue request to include data URL image")
	}

	processing, err := provider.Retrieve(context.Background(), *job, true, false)
	if err != nil {
		t.Fatalf("Retrieve processing error: %v", err)
	}
	if processing.Done || processing.Status != "PROCESSING" {
		t.Fatalf("processing response = %+v", processing)
	}

	complete, err := provider.Retrieve(context.Background(), *job, true, false)
	if err != nil {
		t.Fatalf("Retrieve complete error: %v", err)
	}
	if !complete.Done {
		t.Fatal("expected completed retrieval")
	}
	if complete.MimeType != "video/mp4" {
		t.Fatalf("MimeType = %q, want video/mp4", complete.MimeType)
	}
	if string(complete.Data) != "fake-mp4-data" {
		t.Fatalf("unexpected data %q", string(complete.Data))
	}
}
