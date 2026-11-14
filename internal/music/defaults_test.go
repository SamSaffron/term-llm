package music

import (
	"testing"

	"github.com/term-llm/term-llm/internal/config"
)

func TestMusicDefaultsMatchConfig(t *testing.T) {
	if veniceDefaultModel != config.DefaultMusicVeniceModel {
		t.Fatalf("veniceDefaultModel = %q, want %q", veniceDefaultModel, config.DefaultMusicVeniceModel)
	}
	if veniceDefaultFormat != config.DefaultMusicVeniceFormat {
		t.Fatalf("veniceDefaultFormat = %q, want %q", veniceDefaultFormat, config.DefaultMusicVeniceFormat)
	}
	if elevenLabsDefaultModel != config.DefaultMusicElevenLabsModel {
		t.Fatalf("elevenLabsDefaultModel = %q, want %q", elevenLabsDefaultModel, config.DefaultMusicElevenLabsModel)
	}
	if elevenLabsDefaultFormat != config.DefaultMusicElevenLabsFormat {
		t.Fatalf("elevenLabsDefaultFormat = %q, want %q", elevenLabsDefaultFormat, config.DefaultMusicElevenLabsFormat)
	}
}
