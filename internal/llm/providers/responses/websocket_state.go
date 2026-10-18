package responses

// WebSocketState reports whether WebSocket streaming has fallen back to HTTP
// and whether the last WebSocket request continued an earlier response.
// This is a diagnostic snapshot, not a request to change transport state.
func (c *ResponsesClient) WebSocketState() (disabled, continued bool) {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	return c.websocketDisabled, c.wsLastRequest != nil && c.wsLastRequest.PreviousResponseID != ""
}
