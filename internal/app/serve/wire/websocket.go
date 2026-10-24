package wire

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// JSONChunkWriteWait also bounds ping/control writes while waiting for a data fragment.
	JSONChunkWriteWait   = 10 * time.Second
	jsonWriteBaseBudget  = JSONChunkWriteWait
	jsonWriteMaxBudget   = 3 * time.Minute
	jsonWriteBytesPerSec = 256 << 10
	jsonWriteChunkBytes  = 32 << 10
)

func jsonChunkDeadline(total, timeNow time.Time) time.Time {
	chunk := timeNow.Add(JSONChunkWriteWait)
	if total.Before(chunk) {
		return total
	}
	return chunk
}

func jsonWriteBudget(encodedBytes int) time.Duration {
	return min(jsonWriteMaxBudget, jsonWriteBaseBudget+time.Duration(encodedBytes)*time.Second/jsonWriteBytesPerSec)
}

// WriteFragmentedJSON sends one unchanged JSON WebSocket message in bounded
// fragments. Cancellation is honored before the first wire write, including
// encoding time, but never truncates an in-flight message shared with siblings.
// started reports whether an error can have poisoned the WebSocket writer.
func WriteFragmentedJSON(ctx context.Context, conn *websocket.Conn, payload any) (started bool, err error) {
	w := &jsonMessageWriter{ctx: ctx, conn: conn}
	err = json.NewEncoder(w).Encode(payload)
	if w.message != nil {
		w.refreshDeadline()
		closeErr := w.message.Close()
		if err == nil {
			err = closeErr
		}
		_ = conn.SetWriteDeadline(time.Time{})
	}
	return w.started, err
}

type jsonMessageWriter struct {
	ctx           context.Context
	conn          *websocket.Conn
	message       io.WriteCloser
	started       bool
	totalDeadline time.Time
}

func (w *jsonMessageWriter) Write(p []byte) (int, error) {
	if w.message == nil {
		if err := w.ctx.Err(); err != nil {
			return 0, err
		}
		// Encoder.Encode hands the encoded message to Write at once, so the
		// budget includes base64 expansion and all JSON/header bytes, not just
		// the original request body. No second encoded copy is needed.
		w.totalDeadline = time.Now().Add(jsonWriteBudget(len(p)))
		w.refreshDeadline()
		w.started = true
		var err error
		w.message, err = w.conn.NextWriter(websocket.TextMessage)
		if err != nil {
			return 0, err
		}
	}
	return (boundedJSONWriter{Writer: w.message, beforeWrite: w.refreshDeadline}).Write(p)
}

func (w *jsonMessageWriter) refreshDeadline() {
	// Cap each fragment's socket mutex hold while retaining the absolute
	// encoded-size budget: a trickling peer cannot extend this message forever.
	_ = w.conn.SetWriteDeadline(jsonChunkDeadline(w.totalDeadline, time.Now()))
}

type boundedJSONWriter struct {
	io.Writer
	beforeWrite func()
}

func (w boundedJSONWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		// Gorilla's large-write fast path otherwise holds its socket mutex for
		// the entire base64-expanded upload. Bounded writes release that mutex
		// between fragments so ping/pong/control frames can interleave.
		chunk := p[:min(len(p), jsonWriteChunkBytes)]
		if w.beforeWrite != nil {
			w.beforeWrite()
		}
		n, err := w.Writer.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		if n != len(chunk) {
			return written, io.ErrShortWrite
		}
		p = p[n:]
	}
	return written, nil
}
