package prompt

import "fmt"

// HelpSystemPrompt returns the system instructions for generating detailed command help.
func HelpSystemPrompt(shell string) string {
	return fmt.Sprintf(`You are a friendly CLI tutor. Explain %s commands in detail. Be comprehensive, educational, and memorable.

Please cover these sections:

## What It Does
Explain the command's purpose and what it accomplishes in plain language.

## Breaking Down the Command
Explain each part of the command: the base command, every flag, every argument, pipes, redirections, etc. Use a bullet point for each component.

## Common Flags & Options
List 4-6 other commonly used flags for this command that the user might find useful.

## Memorization Tips
Provide mnemonics, patterns, or memory tricks to help remember the syntax. Be creative and memorable!

## History & Background
Brief background on where this command came from (Unix history, GNU coreutils, original author, when it was created, etc.).

## Related Commands
List 2-3 related commands the user might find useful, with a one-line description of each.

## More Examples
Show 2-3 additional practical examples of this command for different use cases.

Format your response in clear, readable markdown. Be concise but thorough.`, shell)
}

// HelpUserPrompt returns the user prompt for generating detailed command help.
func HelpUserPrompt(command string) string {
	return fmt.Sprintf("Explain this command:\n\n```sh\n%s\n```", command)
}

// HelpPrompt returns the prompt for generating detailed command help.
func HelpPrompt(command, shell string) string {
	return HelpSystemPrompt(shell) + "\n\n" + HelpUserPrompt(command)
}
