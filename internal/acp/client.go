package acp

import "context"

// Client provides typed ACP v1 client operations over a Connection.
type Client struct {
	conn *Connection
}

func NewClient(conn *Connection) *Client { return &Client{conn: conn} }

func (c *Client) Connection() *Connection { return c.conn }

func (c *Client) Initialize(ctx context.Context, request InitializeRequest) (InitializeResponse, error) {
	var response InitializeResponse
	err := c.conn.Call(ctx, "initialize", request, &response)
	return response, err
}

func (c *Client) Authenticate(ctx context.Context, request AuthenticateRequest) error {
	return c.conn.Call(ctx, "authenticate", request, &struct{}{})
}

func (c *Client) NewSession(ctx context.Context, request NewSessionRequest) (NewSessionResponse, error) {
	var response NewSessionResponse
	err := c.conn.Call(ctx, "session/new", request, &response)
	return response, err
}

func (c *Client) LoadSession(ctx context.Context, request LoadSessionRequest) (LoadSessionResponse, error) {
	var response LoadSessionResponse
	err := c.conn.Call(ctx, "session/load", request, &response)
	return response, err
}

func (c *Client) ResumeSession(ctx context.Context, request ResumeSessionRequest) (ResumeSessionResponse, error) {
	var response ResumeSessionResponse
	err := c.conn.Call(ctx, "session/resume", request, &response)
	return response, err
}

func (c *Client) CloseSession(ctx context.Context, request CloseSessionRequest) error {
	return c.conn.Call(ctx, "session/close", request, &struct{}{})
}

func (c *Client) Prompt(ctx context.Context, request PromptRequest) (PromptResponse, error) {
	var response PromptResponse
	err := c.conn.Call(ctx, "session/prompt", request, &response)
	return response, err
}

func (c *Client) CancelSession(ctx context.Context, sessionID string) error {
	return c.conn.Notify(ctx, "session/cancel", CancelSessionRequest{SessionID: sessionID})
}

func (c *Client) CancelSessionWithDelivery(ctx context.Context, sessionID string) (bool, error) {
	return c.conn.NotifyWithDelivery(ctx, "session/cancel", CancelSessionRequest{SessionID: sessionID})
}
