package skills

import (
	"fmt"
	"os"
	"strings"
)

// GenerateAvailableSkillsXML generates the <available_skills> prompt injection.
// Returns empty string if no skills are available.
func GenerateAvailableSkillsXML(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("<available_skills>\n\n")

	// Usage instructions
	sb.WriteString(`<usage>
When users ask you to perform tasks, check if any of the available skills below can help complete the task more effectively. Skills provide specialized capabilities and domain knowledge.

How to invoke:
- Use the activate_skill tool with the skill name
- The skill content will load with detailed instructions
- Base directory provided in output for resolving bundled resources (references/, scripts/, assets/)

Usage notes:
- Only use skills listed below
- Do not invoke a skill that is already loaded in your context
</usage>

`)

	// Skill entries
	for _, skill := range skills {
		sb.WriteString("<skill>\n")
		sb.WriteString(fmt.Sprintf("<name>%s</name>\n", escapeXML(skill.Name)))
		sb.WriteString(fmt.Sprintf("<description>%s</description>\n", escapeXML(skill.Description)))
		sb.WriteString(fmt.Sprintf("<source>%s</source>\n", skill.Source.SourceName()))
		sb.WriteString("</skill>\n\n")
	}

	sb.WriteString("</available_skills>")

	return sb.String()
}

// GenerateActivationResponse generates the tool response when a skill is activated.
func GenerateActivationResponse(skill *Skill, prompt string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# Skill: %s\n\n", skill.Name))
	sb.WriteString(fmt.Sprintf("**Source:** %s\n", skill.SourcePath))
	sb.WriteString(fmt.Sprintf("**Description:** %s\n\n", skill.Description))

	if prompt != "" {
		sb.WriteString(fmt.Sprintf("**Task context:** %s\n\n", prompt))
	}

	sb.WriteString("---\n\n")
	sb.WriteString(skill.Body)

	// Add resource tree if present
	if skill.HasResources() {
		sb.WriteString("\n\n---\n\n")
		sb.WriteString(skill.ResourceTree())
	}

	return sb.String()
}

// EstimateTokens provides a rough token estimate (chars / 3.5).
// This is a placeholder until a proper tokenizer is added.
func EstimateTokens(s string) int {
	return int(float64(len(s)) / 3.5)
}

// TruncateSkillsToTokenBudget returns skills that fit within the token budget.
// Always includes always_enabled skills, then fills with remaining skills.
func TruncateSkillsToTokenBudget(skills []*Skill, alwaysEnabled []string, budgetTokens, maxSkills int) []*Skill {
	if len(skills) == 0 {
		return nil
	}

	// Build set of always-enabled names
	alwaysSet := make(map[string]bool)
	for _, name := range alwaysEnabled {
		alwaysSet[name] = true
	}

	// Separate always-enabled and others
	var always, others []*Skill
	for _, skill := range skills {
		if alwaysSet[skill.Name] {
			always = append(always, skill)
		} else {
			others = append(others, skill)
		}
	}

	// Start with always-enabled
	result := append([]*Skill{}, always...)
	currentTokens := 0

	// Estimate tokens for always-enabled
	for _, skill := range always {
		currentTokens += estimateSkillMetadataTokens(skill)
	}

	// Add others until budget exhausted
	for _, skill := range others {
		if len(result) >= maxSkills {
			break
		}

		tokens := estimateSkillMetadataTokens(skill)
		if currentTokens+tokens > budgetTokens {
			break
		}

		result = append(result, skill)
		currentTokens += tokens
	}

	// Warn if any skills were dropped due to limits
	if len(result) < len(skills) {
		dropped := len(skills) - len(result)
		fmt.Fprintf(os.Stderr, "warning: %d skill(s) not shown (max_active=%d, token_budget=%d); increase skills.max_active or skills.metadata_budget_tokens in config\n",
			dropped, maxSkills, budgetTokens)
	}

	return result
}

// estimateSkillMetadataTokens estimates tokens for a skill's metadata in the prompt.
func estimateSkillMetadataTokens(skill *Skill) int {
	// Approximate: name + description + XML overhead
	content := skill.Name + skill.Description + skill.Source.SourceName()
	return EstimateTokens(content) + 20 // 20 tokens overhead for XML tags
}

// escapeXML escapes special characters for XML content.
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
