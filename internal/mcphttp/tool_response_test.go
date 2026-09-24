package mcphttp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestToolResponseConcurrentWrites(t *testing.T) {
	recorder := httptest.NewRecorder()
	response := &toolResponse{ResponseWriter: recorder}
	response.Header().Set("Content-Type", "text/event-stream")
	var workers sync.WaitGroup
	for i := range 20 {
		workers.Go(func() {
			response.start()
			_, _ = fmt.Fprintf(response, "data: %d\n\n", i)
			_ = response.FlushError()
		})
	}
	workers.Wait()
	body := recorder.Body.String()
	if strings.Count(body, ": tool execution started\n\n") != 1 {
		t.Fatalf("expected one initial comment: %q", body)
	}
	if strings.Count(body, "data:") != 20 {
		t.Fatalf("lost events: %q", body)
	}
}

func TestToolResponseDoesNotFlushAfterHandlerReturns(t *testing.T) {
	var toolContext context.Context
	handler := toolResponseMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		toolContext = r.Context()
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	toolContext.Value(toolResponseKey{}).(*toolResponse).start()
	if recorder.Flushed || recorder.Body.Len() != 0 {
		t.Fatal("late tool start wrote to a completed response")
	}
}
