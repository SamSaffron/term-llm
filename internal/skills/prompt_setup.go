package skills

import (
	"strings"

	"github.com/term-llm/term-llm/internal/runtimeoutput"
)

// PromptAdditions returns explicit instructions and any discovery metadata not
// already present in instructions. AGENTS.md catalog suppression never cancels
// the user's explicit skill selection.
func (s *Setup) PromptAdditions(instructions string) string {
	var additions []string
	preloaded, err := s.PreloadedInstructions()
	if err != nil {
		runtimeoutput.Printf("warning: skills preloading failed: %v\n", err)
	}
	if preloaded != "" && !strings.Contains(instructions, preloaded) {
		additions = append(additions, preloaded)
	}
	if strings.Contains(instructions, "<available_skills>") {
		return strings.Join(additions, "\n\n")
	}
	if suppressed, known := s.PromptMetadataSuppressed(); suppressed || (!known && CheckAgentsMdForSkills()) {
		return strings.Join(additions, "\n\n")
	}
	if err := s.EnsurePromptMetadata(); err != nil {
		runtimeoutput.Printf("warning: skills metadata generation failed: %v\n", err)
	} else if s.HasSkillsXML() {
		additions = append(additions, s.XML)
	}
	return strings.Join(additions, "\n\n")
}
