package notification

import "strings"

// completionChatTitleMaxRunes keeps a spoken announcement short. Generated
// titles are 2–8 words, but an untitled chat falls back to its first prompt.
const completionChatTitleMaxRunes = 60

// CompletionText returns the display text for a completion event naming the
// chat that finished. The OS already attributes the alert to the app (and Siri
// says the app name before reading it), so neither line repeats "term-llm".
// Without a chat title the outcome moves into the title and the body is empty.
func CompletionText(label, chatTitle string, failed bool) (title, body string) {
	status := "Response ready"
	if failed {
		status = "Response failed"
	}
	title, body = shortenChatTitle(chatTitle), status
	if title == "" {
		title, body = status, ""
	}
	if label = strings.TrimSpace(label); label != "" {
		title = label + ": " + title
	}
	return title, body
}

func shortenChatTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	runes := []rune(title)
	if len(runes) <= completionChatTitleMaxRunes {
		return title
	}
	cut := string(runes[:completionChatTitleMaxRunes])
	if space := strings.LastIndex(cut, " "); space > len(cut)/2 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " .,;:-") + "…"
}
