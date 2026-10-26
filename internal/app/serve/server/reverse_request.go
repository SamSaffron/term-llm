// Package server owns serve HTTP/platform behavior extracted from the command layer.
package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/restart"
)

const (
	ReverseChunkSize          = 32 * 1024
	ReverseFrameResponseStart = "response_start"
	ReverseFrameResponseBody  = "response_body"
	ReverseFrameResponseEnd   = "response_end"
)

// ReverseRequest is a request sent from the Hub to a reverse-connected node.
type ReverseRequest struct {
	Type   string      `json:"type,omitempty"`
	ID     string      `json:"id"`
	Method string      `json:"method,omitempty"`
	Path   string      `json:"path,omitempty"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// ReverseResponse is a response frame sent by the reverse-connected node.
type ReverseResponse struct {
	Type   string      `json:"type,omitempty"`
	ID     string      `json:"id"`
	Status int         `json:"status,omitempty"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// ContainsEncodedSeparator rejects path separators smuggled into escaped paths.
func ContainsEncodedSeparator(path string) bool {
	lower := strings.ToLower(path)
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

// HasDotDotSegment rejects decoded path traversal segments.
func HasDotDotSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// ResponseIsPassive identifies an SSE subscription rather than active node work.
func ResponseIsPassive(method string, header http.Header) bool {
	return strings.HasPrefix(strings.ToLower(header.Get("Content-Type")), "text/event-stream") &&
		(method == http.MethodGet || (header.Get("X-Response-ID") != "" && header.Get("X-Term-LLM-Response-Status") != ""))
}

// HandleHubReverseRequest validates and relays one Hub request over local HTTP.
func HandleHubReverseRequest(ctx context.Context, frame ReverseRequest, token, localBase, allowedBasePath string, client *http.Client, writeFrame func(ReverseResponse) error) {
	sendError := func(status int, msg string) {
		_ = writeFrame(ReverseResponse{Type: ReverseFrameResponseStart, ID: frame.ID, Status: status, Error: msg})
	}
	if frame.ID == "" {
		_ = writeFrame(ReverseResponse{Type: ReverseFrameResponseStart, Status: http.StatusBadRequest, Error: "missing request id"})
		return
	}
	pathOnly := frame.Path
	if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	// Validate only the URI path. Query parameters may legitimately contain
	// encoded slashes (for example file-change diff paths like path=a%2Fb) and
	// are interpreted by node handlers as parameter values, not route segments.
	if !strings.HasPrefix(pathOnly, "/") || ContainsEncodedSeparator(pathOnly) || HasDotDotSegment(pathOnly) {
		sendError(http.StatusBadRequest, "invalid reverse request path")
		return
	}
	allowedBasePath = strings.TrimRight(allowedBasePath, "/")
	if allowedBasePath != "" && pathOnly != allowedBasePath && !strings.HasPrefix(pathOnly, allowedBasePath+"/") {
		sendError(http.StatusForbidden, "reverse request outside node base path")
		return
	}
	localURL := strings.TrimRight(localBase, "/") + frame.Path
	req, err := http.NewRequestWithContext(ctx, frame.Method, localURL, bytes.NewReader(frame.Body))
	if err != nil {
		sendError(http.StatusBadRequest, err.Error())
		return
	}
	req.Header = frame.Header.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		status := http.StatusBadGateway
		if ctx.Err() != nil {
			// Local reload interrupts HTTP work, not the healthy Hub connection.
			if !errors.Is(context.Cause(ctx), restart.ErrInterrupt) {
				return
			}
			status = http.StatusServiceUnavailable
		}
		sendError(status, err.Error())
		return
	}
	defer resp.Body.Close()
	if ResponseIsPassive(frame.Method, resp.Header) {
		restart.Passive(ctx)
	}
	if err := writeFrame(ReverseResponse{Type: ReverseFrameResponseStart, ID: frame.ID, Status: resp.StatusCode, Header: resp.Header.Clone()}); err != nil {
		return
	}
	buf := make([]byte, ReverseChunkSize)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := writeFrame(ReverseResponse{Type: ReverseFrameResponseBody, ID: frame.ID, Body: chunk}); err != nil {
				return
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				_ = writeFrame(ReverseResponse{Type: ReverseFrameResponseEnd, ID: frame.ID})
				return
			}
			if ctx.Err() == nil || errors.Is(context.Cause(ctx), restart.ErrInterrupt) {
				_ = writeFrame(ReverseResponse{Type: ReverseFrameResponseEnd, ID: frame.ID, Error: readErr.Error()})
			}
			return
		}
	}
}
