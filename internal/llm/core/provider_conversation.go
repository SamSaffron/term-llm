package core

// ProviderConversation separates durable conversation identity from generated,
// request-only context. Messages has the same provider-safety and image
// transformations as Request.Messages, but excludes TurnContext. Native-session
// providers can validate a stable prefix without mistaking a regenerated roster
// for an edit to historical instructions. Other providers use Request.Messages,
// which already includes TurnContext at its normal policy position.
type ProviderConversation struct {
	Messages    []Message
	TurnContext []Message
}

// WithTurnContext inserts request-only policy before the latest user message,
// never after a trailing tool result. It does not mutate either input slice.
// Without a user message, context precedes the entire conversation.
func WithTurnContext(messages, turnContext []Message) []Message {
	if len(turnContext) == 0 {
		return messages
	}
	index := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			index = i
			break
		}
	}
	out := make([]Message, 0, len(messages)+len(turnContext))
	out = append(out, messages[:index]...)
	out = append(out, turnContext...)
	return append(out, messages[index:]...)
}
