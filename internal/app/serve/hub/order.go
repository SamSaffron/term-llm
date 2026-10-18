package hub

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

const (
	// NodeOrderPath saves the dashboard's node order. It is the only Hub
	// operator route a node may also call with its own credentials.
	NodeOrderPath = "/api/nodes/order"
	// NodeOrderBodyLimit bounds a reorder request.
	NodeOrderBodyLimit = 128 << 10
	// NodeIDHeader names the node a request claims to be.
	NodeIDHeader = "X-Term-LLM-Node-ID"
)

// NodeOrderRequest lists node IDs in their new relative order.
type NodeOrderRequest struct {
	NodeIDs []string `json:"node_ids"`
}

// NodeIDPresented reports whether a request claims a node identity. Such a
// request is authenticated as that node or rejected; it never falls back to
// Hub operator credentials.
func NodeIDPresented(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(NodeIDHeader)) != ""
}

// NodeOrderNodeRequest reports a node saving the dashboard order with its
// own credentials. Only PATCH of this exact path skips Hub authentication.
func NodeOrderNodeRequest(r *http.Request) bool {
	return r.URL.Path == NodeOrderPath && r.Method == http.MethodPatch && NodeIDPresented(r)
}

// DecodeNodeOrderRequest reads exactly one bounded JSON object with no
// unknown fields.
func DecodeNodeOrderRequest(w http.ResponseWriter, r *http.Request, dst *NodeOrderRequest) error {
	r.Body = http.MaxBytesReader(w, r.Body, NodeOrderBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

// RepeatLog logs each distinct error once until an operation succeeds again,
// so a problem seen on every dashboard poll cannot flood the log.
type RepeatLog struct {
	mu   sync.Mutex
	last string
}

func (l *RepeatLog) Report(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		l.last = ""
		return
	}
	if message := err.Error(); message != l.last {
		l.last = message
		log.Printf("hub: dashboard node order: %v", err)
	}
}
