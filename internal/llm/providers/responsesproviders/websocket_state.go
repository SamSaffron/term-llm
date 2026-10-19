package responsesproviders

// WebSocketState reports ChatGPT's WebSocket transport diagnostic snapshot.
func (p *ChatGPTProvider) WebSocketState() (disabled, continued bool) {
	if p.responsesClient == nil {
		return false, false
	}
	return p.responsesClient.WebSocketState()
}
