package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveAudioTextFromArgs(t *testing.T) {
	text, err := resolveAudioText(&cobra.Command{}, []string{"hello", "world"})
	if err != nil {
		t.Fatalf("resolveAudioText: %v", err)
	}
	if text != "hello world" {
		t.Fatalf("text = %q, want hello world", text)
	}
}

func TestSaveAudioOutputCustomPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp3")
	got, err := saveAudioOutput(dir, "hello", path, "mp3", []byte("audio-bytes"))
	if err != nil {
		t.Fatalf("saveAudioOutput: %v", err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "audio-bytes" {
		t.Fatalf("data = %q, want audio-bytes", string(data))
	}
}

func TestEmitAudioJSON(t *testing.T) {
	oldJSON := audioJSON
	audioJSON = true
	defer func() { audioJSON = oldJSON }()

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	err := emitAudioJSON(cmd, audioJSONResult{
		Provider: "venice",
		Text:     "hello",
		Model:    "tts-kokoro",
		Voice:    "af_sky",
		Format:   "mp3",
		Output:   &audioJSONOutput{Path: "clip.mp3", MimeType: "audio/mpeg", Bytes: 5},
	})
	if err != nil {
		t.Fatalf("emitAudioJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["provider"] != "venice" || got["voice"] != "af_sky" {
		t.Fatalf("unexpected json: %#v", got)
	}
}
