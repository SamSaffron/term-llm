package mcp

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const defaultCommandTerminateDuration = 5 * time.Second

// commandTransport is the stdio transport for MCP server processes. It mirrors
// the SDK's CommandTransport but starts commands through procutil so Windows
// children are assigned to an owned Job Object before they can spawn
// descendants.
type commandTransport struct {
	Command           *procutil.Command
	TerminateDuration time.Duration
	MaxLineLength     int
}

func (t *commandTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	cmd := t.Command.Cmd()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := t.Command.Start(); err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		return nil, err
	}
	conn, err := (&sdkmcp.IOTransport{
		Reader:        nopReadCloser{Reader: stdout},
		Writer:        stdin,
		MaxLineLength: t.MaxLineLength,
	}).Connect(ctx)
	if err != nil {
		_ = t.Command.Close()
		return nil, err
	}
	terminate := t.TerminateDuration
	if terminate <= 0 {
		terminate = defaultCommandTerminateDuration
	}
	return &ownedCommandConn{Connection: conn, cmd: t.Command, terminate: terminate}, nil
}

type nopReadCloser struct {
	io.Reader
}

func (nopReadCloser) Close() error { return nil }

// ownedCommandConn closes the stdio connection, waits for the server, and then
// reaps/releases the owned process tree. Close is idempotent and safe alongside
// context cancellation.
type ownedCommandConn struct {
	sdkmcp.Connection
	cmd       *procutil.Command
	terminate time.Duration
	once      sync.Once
	err       error
}

func (c *ownedCommandConn) Close() error {
	c.once.Do(func() {
		if err := c.Connection.Close(); err != nil {
			c.err = err
		}
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case err := <-done:
			if c.err == nil {
				c.err = err
			}
		case <-time.After(c.terminate):
			_ = c.cmd.Close()
			select {
			case err := <-done:
				if c.err == nil {
					c.err = err
				}
			case <-time.After(c.terminate):
				if c.err == nil {
					c.err = fmt.Errorf("unresponsive subprocess")
				}
			}
		}
		_ = c.cmd.Close()
	})
	return c.err
}
