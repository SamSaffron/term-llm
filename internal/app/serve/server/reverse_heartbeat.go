package server

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/app/serve/wire"
)

const (
	ReversePingInterval = 20 * time.Second
	ReversePongWait     = 60 * time.Second
)

// SetReverseHeartbeat installs the reverse tunnel pong handler and read deadline.
func SetReverseHeartbeat(conn *websocket.Conn, touch func()) error {
	if touch != nil {
		touch()
	}
	if err := conn.SetReadDeadline(time.Now().Add(ReversePongWait)); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error {
		if touch != nil {
			touch()
		}
		return conn.SetReadDeadline(time.Now().Add(ReversePongWait))
	})
	return nil
}

// RunReversePingLoop sends connector pings serialized with its data writer.
func RunReversePingLoop(conn *websocket.Conn, writeMu *sync.Mutex, done <-chan struct{}) {
	ticker := time.NewTicker(ReversePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(wire.JSONChunkWriteWait))
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wire.JSONChunkWriteWait))
			_ = conn.SetWriteDeadline(time.Time{})
			writeMu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// RunReverseControlPingLoop sends Hub pings without mutating writer deadlines.
func RunReverseControlPingLoop(conn *websocket.Conn, done <-chan struct{}, fail func(error)) {
	ticker := time.NewTicker(ReversePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// WriteControl is Gorilla's concurrency-safe control API and has its own
			// deadline, including waiting for a data writer. Do not race SetWriteDeadline.
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wire.JSONChunkWriteWait)); err != nil {
				fail(err)
				return
			}
		}
	}
}
