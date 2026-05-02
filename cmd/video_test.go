package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestLoadVideoInputFromStdin(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader([]byte("image-bytes")))

	data, err := loadVideoInput(cmd, "-")
	if err != nil {
		t.Fatalf("loadVideoInput stdin: %v", err)
	}
	if string(data) != "image-bytes" {
		t.Fatalf("data = %q, want image-bytes", string(data))
	}
}

func TestLoadVideoInputFromEmptyStdin(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(nil))

	_, err := loadVideoInput(cmd, "-")
	if err == nil {
		t.Fatal("expected empty stdin error")
	}
}

func TestRunVideoRequiresArgPromptWhenInputUsesStdin(t *testing.T) {
	oldInput := videoInput
	videoInput = "-"
	defer func() { videoInput = oldInput }()

	err := runVideo(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("expected prompt-required error")
	}
	if !strings.Contains(err.Error(), "prompt required") {
		t.Fatalf("error = %v, want prompt required", err)
	}
}

func TestLoadVideoReferences(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.png")
	second := filepath.Join(dir, "second.jpg")
	if err := os.WriteFile(first, []byte("one"), 0o644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(second, []byte("two"), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}

	references, err := loadVideoReferences(&cobra.Command{}, []string{first, second})
	if err != nil {
		t.Fatalf("loadVideoReferences: %v", err)
	}
	if len(references) != 2 {
		t.Fatalf("len(references) = %d, want 2", len(references))
	}
	if references[0].Path != first || string(references[0].Data) != "one" {
		t.Fatalf("unexpected first reference: %+v", references[0])
	}
	if references[1].Path != second || string(references[1].Data) != "two" {
		t.Fatalf("unexpected second reference: %+v", references[1])
	}
}

func TestLoadVideoReferencesFromStdin(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader([]byte("reference-bytes")))

	references, err := loadVideoReferences(cmd, []string{"-"})
	if err != nil {
		t.Fatalf("loadVideoReferences stdin: %v", err)
	}
	if len(references) != 1 {
		t.Fatalf("len(references) = %d, want 1", len(references))
	}
	if references[0].Path != "-" || string(references[0].Data) != "reference-bytes" {
		t.Fatalf("unexpected reference: %+v", references[0])
	}
}

func TestLoadVideoReferencesFromEmptyStdin(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(nil))

	_, err := loadVideoReferences(cmd, []string{"-"})
	if err == nil {
		t.Fatal("expected empty stdin error")
	}
}

func TestLoadVideoReferencesRejectsMultipleStdinReferences(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader([]byte("reference-bytes")))

	_, err := loadVideoReferences(cmd, []string{"-", "-"})
	if err == nil {
		t.Fatal("expected multiple stdin references error")
	}
	if !strings.Contains(err.Error(), "only one --reference -") {
		t.Fatalf("error = %v, want only one --reference -", err)
	}
}

func TestRunVideoRequiresArgPromptWhenReferenceUsesStdin(t *testing.T) {
	oldReferences := videoReferences
	videoReferences = []string{"-"}
	defer func() { videoReferences = oldReferences }()

	err := runVideo(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("expected prompt-required error")
	}
	if !strings.Contains(err.Error(), "prompt required") {
		t.Fatalf("error = %v, want prompt required", err)
	}
}

func TestRunVideoRejectsInputAndReferenceBothUsingStdin(t *testing.T) {
	oldInput := videoInput
	oldReferences := videoReferences
	videoInput = "-"
	videoReferences = []string{"-"}
	defer func() {
		videoInput = oldInput
		videoReferences = oldReferences
	}()

	err := runVideo(&cobra.Command{}, []string{"animate"})
	if err == nil {
		t.Fatal("expected stdin conflict error")
	}
	if !strings.Contains(err.Error(), "stdin can only be used for one media input") {
		t.Fatalf("error = %v, want stdin conflict", err)
	}
}

func TestEmitVideoJSON(t *testing.T) {
	oldJSON := videoJSON
	oldAspectRatio := videoAspectRatio
	videoJSON = true
	videoAspectRatio = "9:16"
	defer func() {
		videoJSON = oldJSON
		videoAspectRatio = oldAspectRatio
	}()

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	err := emitVideoJSON(cmd, videoJSONResult{
		Provider:   "venice",
		Prompt:     "romeo is adorable",
		Model:      "kling-o3-pro-image-to-video",
		Duration:   "5s",
		Resolution: "720p",
		Status:     "queued",
		Quote:      &videoJSONQuote{Amount: 1.06},
		Job:        &videoJSONJob{QueueID: "queue-123"},
		References: []string{"a.png", "b.png"},
	})
	if err != nil {
		t.Fatalf("emitVideoJSON: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if got["aspect_ratio"] != "9:16" {
		t.Fatalf("aspect_ratio = %v, want 9:16", got["aspect_ratio"])
	}
	if got["status"] != "queued" {
		t.Fatalf("status = %v, want queued", got["status"])
	}
	job, ok := got["job"].(map[string]any)
	if !ok || job["queue_id"] != "queue-123" {
		t.Fatalf("job = %#v", got["job"])
	}
	references, ok := got["references"].([]any)
	if !ok || len(references) != 2 {
		t.Fatalf("references = %#v", got["references"])
	}
}
