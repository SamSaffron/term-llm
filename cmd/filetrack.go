package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/tools"
)

func applyServeFileTrackingOverride(cfg *config.Config, enabled bool) {
	runtime.ApplyServeFileTrackingOverride(cfg, enabled)
}
func fileTrackingStore(cfg *config.Config) *filetrack.Store { return runtime.FileTrackingStore(cfg) }
func closeFileTrackingStore()                               { runtime.CloseFileTrackingStore() }
func wireFileRecorder(registry *tools.LocalToolRegistry, cfg *config.Config) {
	runtime.WireFileRecorder(registry, cfg)
}
