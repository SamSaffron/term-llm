package prompt

import "fmt"

// HelpPrompt returns the prompt for generating detailed command help
func HelpPrompt(command, shell string) string {
	return fmt.Sprintf(`You are a friendly CLI tutor. Explain the following %s command in detail. Be comprehensive, educational, and memorable.

Command: %s

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

Format your response in clear, readable markdown. Be concise but thorough.`, shell, command)
}
