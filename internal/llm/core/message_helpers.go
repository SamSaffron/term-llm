package core

import "strings"

func CollectTextParts(parts []Part) string {
	var b strings.Builder
	for _, part := range parts {
		if (part.Type == PartText || part.Type == PartFile) && part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func CollectToolResultText(parts []Part) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type == PartToolResult && part.ToolResult != nil {
			b.WriteString(ToolResultTextContent(part.ToolResult))
		}
	}
	return b.String()
}

func FlattenSystemUser(messages []Message) (string, string) {
	var systemParts []string
	var userParts []string
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			systemParts = append(systemParts, CollectTextParts(msg.Parts))
		case RoleUser:
			userParts = append(userParts, CollectTextParts(msg.Parts))
		case RoleTool:
			userParts = append(userParts, CollectToolResultText(msg.Parts))
		}
	}
	return strings.Join(systemParts, "\n\n"), strings.Join(userParts, "\n\n")
}
