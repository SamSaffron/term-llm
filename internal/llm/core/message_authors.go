package core

// WithoutMessageAuthors strips display-only attribution at the provider boundary
// without mutating persistence/history or allocating for unattributed messages.
func WithoutMessageAuthors(messages []Message) []Message {
	for _, message := range messages {
		if message.AuthorID != "" {
			out := append([]Message(nil), messages...)
			for i := range out {
				out[i].AuthorID = ""
			}
			return out
		}
	}
	return messages
}
