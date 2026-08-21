// Package acp implements the client side of Agent Client Protocol v1.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sync"
	"sync/atomic"
	"time"
)

const defaultMaxFrameBytes = 64 << 20

var ErrFrameTooLarge = errors.New("ACP frame exceeds maximum size")

// RPCError is a JSON-RPC error returned by either ACP endpoint.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("ACP RPC error %d: %s", e.Code, e.Message)
}

// MethodNotFound creates the standard JSON-RPC method-not-found response.
func MethodNotFound(method string) *RPCError {
	return &RPCError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)}
}

// Handler receives requests and notifications initiated by the ACP agent.
type Handler interface {
	HandleNotification(ctx context.Context, method string, params json.RawMessage)
	HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, *RPCError)
}

// Options configures a Connection.
type Options struct {
	MaxFrameBytes int
}

type wireEnvelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type callResult struct {
	result json.RawMessage
	err    error
}

// Connection is a bidirectional ACP v1 JSON-RPC connection. It does not own or
// close the supplied streams; subprocess owners remain responsible for doing so.
type Connection struct {
	reader  io.Reader
	writer  io.Writer
	handler Handler
	maxSize int

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan callResult

	dispatch chan wireEnvelope
	done     chan struct{}
	close    sync.Once

	errMu   sync.Mutex
	err     error
	readErr error
}

// NewConnection starts an ACP connection over newline-delimited JSON streams.
func NewConnection(reader io.Reader, writer io.Writer, handler Handler, options Options) *Connection {
	maxSize := options.MaxFrameBytes
	if maxSize <= 0 {
		maxSize = defaultMaxFrameBytes
	}
	c := &Connection{
		reader:   reader,
		writer:   writer,
		handler:  handler,
		maxSize:  maxSize,
		pending:  make(map[int64]chan callResult),
		dispatch: make(chan wireEnvelope, 256),
		done:     make(chan struct{}),
	}
	go c.dispatchLoop()
	go c.readLoop()
	return c
}

// Done closes when the transport can no longer process messages.
func (c *Connection) Done() <-chan struct{} { return c.done }

// Err returns the terminal transport or protocol error, if any.
func (c *Connection) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// Call invokes an ACP method and decodes its result. Calls may run concurrently.
func (c *Connection) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, nil)
}

// CallAfterWrite is Call plus a hook invoked after the request is written and
// before waiting for the response. Grok-bin uses this so session/cancel cannot
// overtake session/prompt on the wire.
func (c *Connection) CallAfterWrite(ctx context.Context, method string, params, result any, afterWrite func()) error {
	return c.call(ctx, method, params, result, afterWrite)
}

func (c *Connection) call(ctx context.Context, method string, params, result any, afterWrite func()) error {
	if err := c.Err(); err != nil {
		return err
	}
	id := c.nextID.Add(1)
	response := make(chan callResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = response
	c.pendingMu.Unlock()

	if err := c.writeObject(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		c.removePending(id)
		c.fail(fmt.Errorf("write ACP %s request: %w", method, err))
		return err
	}
	if afterWrite != nil {
		afterWrite()
	}

	select {
	case reply := <-response:
		if reply.err != nil {
			return reply.err
		}
		if result == nil || len(reply.result) == 0 || bytes.Equal(reply.result, []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(reply.result, result); err != nil {
			return fmt.Errorf("decode ACP %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		if c.removePending(id) {
			go func() {
				cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = c.Notify(cancelCtx, "$/cancel_request", map[string]any{"requestId": id})
			}()
		}
		return ctx.Err()
	case <-c.done:
		if err := c.Err(); err != nil {
			return err
		}
		return io.EOF
	}
}

// Notify sends an ACP notification.
func (c *Connection) Notify(ctx context.Context, method string, params any) error {
	_, err := c.NotifyWithDelivery(ctx, method, params)
	return err
}

// NotifyWithDelivery sends an ACP notification and reports whether the complete
// notification was written. A writer may return both len(data) and an error; in
// that case the peer can still act on the delivered notification.
func (c *Connection) NotifyWithDelivery(ctx context.Context, method string, params any) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.done:
		if err := c.Err(); err != nil {
			return false, err
		}
		return false, io.EOF
	default:
	}
	return c.writeObjectWithDelivery(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *Connection) writeObject(value any) error {
	_, err := c.writeObjectWithDelivery(value)
	return err
}

func (c *Connection) writeObjectWithDelivery(value any) (bool, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("encode ACP message: %w", err)
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	for len(data) > 0 {
		n, err := c.writer.Write(data)
		if n < 0 || n > len(data) {
			return false, io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return len(data) == 0, err
		}
		if n == 0 {
			return false, io.ErrShortWrite
		}
	}
	return true, nil
}

func (c *Connection) readLoop() {
	defer close(c.dispatch)
	reader := bufio.NewReaderSize(c.reader, 64<<10)
	for {
		frame, err := readFrame(reader, c.maxSize)
		if err != nil {
			c.setReadError(err)
			return
		}
		var message wireEnvelope
		if err := json.Unmarshal(frame, &message); err != nil {
			c.setReadError(fmt.Errorf("decode ACP frame: %w", err))
			return
		}
		if message.JSONRPC != "2.0" {
			c.setReadError(fmt.Errorf("invalid ACP jsonrpc version %q", message.JSONRPC))
			return
		}
		select {
		case c.dispatch <- message:
		case <-c.done:
			return
		}
	}
}

func readFrame(reader *bufio.Reader, maxSize int) ([]byte, error) {
	var frame []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(frame)+len(part) > maxSize {
			return nil, ErrFrameTooLarge
		}
		frame = append(frame, part...)
		switch {
		case err == nil:
			return bytes.TrimSuffix(frame, []byte{'\n'}), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(frame) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

func (c *Connection) dispatchLoop() {
	for message := range c.dispatch {
		c.dispatchMessage(message)
	}
	c.errMu.Lock()
	readErr := c.readErr
	c.errMu.Unlock()
	c.fail(readErr)
}

func (c *Connection) dispatchMessage(message wireEnvelope) {
	if message.Method != "" {
		if len(message.ID) == 0 || bytes.Equal(message.ID, []byte("null")) {
			if c.handler != nil {
				c.handler.HandleNotification(context.Background(), message.Method, message.Params)
			}
			return
		}
		var result any
		var rpcErr *RPCError
		if c.handler == nil {
			rpcErr = MethodNotFound(message.Method)
		} else {
			result, rpcErr = c.handler.HandleRequest(context.Background(), message.Method, message.Params)
		}
		response := map[string]any{"jsonrpc": "2.0", "id": message.ID}
		if rpcErr != nil {
			response["error"] = rpcErr
		} else {
			if result == nil {
				result = struct{}{}
			}
			response["result"] = result
		}
		if err := c.writeObject(response); err != nil {
			c.fail(fmt.Errorf("write ACP response to %s: %w", message.Method, err))
		}
		return
	}

	id, ok := numericID(message.ID)
	if !ok {
		// Some agents emit internal responses whose IDs belong to their own
		// extension traffic. They are not responses to our monotonically numeric IDs.
		return
	}
	c.pendingMu.Lock()
	response, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	if message.Error != nil {
		response <- callResult{err: message.Error}
	} else {
		response <- callResult{result: message.Result}
	}
}

func numericID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	value, ok := new(big.Rat).SetString(string(raw))
	if !ok || value.Denom().Cmp(big.NewInt(1)) != 0 || !value.Num().IsInt64() {
		return 0, false
	}
	return value.Num().Int64(), true
}

func (c *Connection) removePending(id int64) bool {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if _, ok := c.pending[id]; !ok {
		return false
	}
	delete(c.pending, id)
	return true
}

func (c *Connection) setReadError(err error) {
	c.errMu.Lock()
	c.readErr = err
	c.errMu.Unlock()
}

func (c *Connection) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	c.close.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.done)

		c.pendingMu.Lock()
		pending := c.pending
		c.pending = make(map[int64]chan callResult)
		c.pendingMu.Unlock()
		for _, response := range pending {
			response <- callResult{err: err}
		}
	})
}
