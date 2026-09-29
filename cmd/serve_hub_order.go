package cmd

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/hub"
)

const (
	// hubNodeOrderPath saves the dashboard's node order. It is the only Hub
	// operator route a node may also call with its own credentials.
	hubNodeOrderPath = "/api/nodes/order"
	// hubNodeOrderBodyLimit bounds a reorder request; hub.MaxNodeOrderIDs
	// quoted IDs of at most 64 characters fit well within it.
	hubNodeOrderBodyLimit = 128 << 10
)

// hubNodeOrderRequest lists node IDs in their new relative order.
type hubNodeOrderRequest struct {
	NodeIDs []string `json:"node_ids"`
}

// hubNodeIDPresented reports whether a request claims a node identity. Such
// a request is authenticated as that node or rejected; it never falls back
// to Hub operator credentials.
func hubNodeIDPresented(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(hubNodeIDHeader)) != ""
}

// hubNodeOrderNodeRequest reports a node saving the dashboard order with its
// own credentials. Only PATCH of this exact path skips Hub authentication for
// a node, and handleNodesOrder then authenticates the node itself, so a node
// token reaches no other operator route or method.
func hubNodeOrderNodeRequest(r *http.Request) bool {
	return r.URL.Path == hubNodeOrderPath && r.Method == http.MethodPatch && hubNodeIDPresented(r)
}

// handleNodesOrder serves PATCH /api/nodes/order for the Hub operator and
// for individual nodes authenticated with their own token. The listed nodes
// move, in the given order, into the dashboard positions they already
// occupy; nodes the caller did not list, including ones the Hub does not
// currently list, keep theirs. The request is atomic and idempotent, and the
// response is the complete committed order of the nodes the Hub lists.
func (s *hubServer) handleNodesOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		// "order" is a valid node ID. DELETE never skips Hub authentication,
		// so removing a local node of that name keeps working.
		s.handleNodeItem(w, r)
		return
	}
	if r.Method != http.MethodPatch {
		w.Header().Set("Allow", http.MethodPatch)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !hubBrowserRequestAllowed(r, false) {
		writeOpenAIError(w, http.StatusForbidden, "forbidden", "forbidden cross-site hub request")
		return
	}
	caller := ""
	if hubNodeIDPresented(r) {
		node, err := s.authenticateNode(r)
		if err != nil {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_node_credentials", "node authentication failed")
			return
		}
		caller = node.ID
	}
	s.saveNodeOrder(w, r, caller)
}

// saveNodeOrder validates and commits an authenticated reorder request.
// caller names the node that made it, or is empty for the Hub operator.
func (s *hubServer) saveNodeOrder(w http.ResponseWriter, r *http.Request, caller string) {
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_content_type", err.Error())
		return
	}
	if s.nodeOrder == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "node_order_unavailable", "node ordering is unavailable")
		return
	}
	var req hubNodeOrderRequest
	if err := decodeHubNodeOrderRequest(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "node order request is too large")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_node_order", "invalid node order request: "+err.Error())
		return
	}
	// A failing resolver only hides its own nodes, which then cannot be listed.
	nodes, _ := s.registry.Nodes()
	order, err := s.nodeOrder.Reorder(nodes, req.NodeIDs)
	if err != nil {
		writeHubNodeOrderError(w, err)
		return
	}
	if caller != "" {
		log.Printf("hub: node %q saved the dashboard node order", caller)
	}
	writeJSON(w, http.StatusOK, map[string]any{"node_ids": order})
}

// decodeHubNodeOrderRequest reads exactly one bounded JSON object with no
// unknown fields.
func decodeHubNodeOrderRequest(w http.ResponseWriter, r *http.Request, dst *hubNodeOrderRequest) error {
	r.Body = http.MaxBytesReader(w, r.Body, hubNodeOrderBodyLimit)
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

func writeHubNodeOrderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrNodeOrderInvalid):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_node_order", err.Error())
	case errors.Is(err, hub.ErrNodeOrderUnknownNode):
		writeOpenAIError(w, http.StatusNotFound, "node_not_found", "a listed node was not found; refresh and try again")
	default:
		log.Printf("hub: save node order: %v", err)
		writeOpenAIError(w, http.StatusInternalServerError, "node_order_error", "failed to save the node order")
	}
}

// arrangeNodes puts registry nodes in the saved dashboard order. Without a
// node order store they keep the registry's name order.
func (s *hubServer) arrangeNodes(nodes []hub.Node) []hub.Node {
	if s.nodeOrder == nil {
		return nodes
	}
	arranged, err := s.nodeOrder.Arrange(nodes)
	s.nodeOrderLog.report(err)
	return arranged
}

// hubRepeatLog logs each distinct error once until an operation succeeds
// again, so a problem seen on every dashboard poll cannot flood the log.
type hubRepeatLog struct {
	mu   sync.Mutex
	last string
}

func (l *hubRepeatLog) report(err error) {
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
