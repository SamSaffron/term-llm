package runtime

import (
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/skills"
)

// PreloadSkillInstructions adds only explicitly named skill bodies to a command
// without a skill-tool runtime. Empty/all selections do not scan the catalog or
// advertise discovery tools. Config always_enabled remains metadata-only.
func PreloadSkillInstructions(cfg *config.SkillsConfig, flag, instructions string, warnings io.Writer) string {
	names := ApplySkillsFlag(&config.SkillsConfig{}, flag).AlwaysEnabled
	if len(names) == 0 {
		return instructions
	}
	selected := ApplySkillsFlag(cfg, flag)
	setup, err := skills.NewSetupWithOptions(selected, skills.SetupOptions{
		PreloadNames: names, PromptMetadataSuppressed: true, PromptMetadataSuppressionKnown: true,
	})
	if err != nil {
		fmt.Fprintf(warnings, "warning: skills initialization failed: %v\n", err)
		return instructions
	}
	preloaded, err := setup.PreloadedInstructions()
	if err != nil {
		fmt.Fprintf(warnings, "warning: skills preloading failed: %v\n", err)
	}
	if preloaded == "" || strings.Contains(instructions, preloaded) {
		return instructions
	}
	if strings.TrimSpace(instructions) == "" {
		return preloaded
	}
	return instructions + "\n\n" + preloaded
}
