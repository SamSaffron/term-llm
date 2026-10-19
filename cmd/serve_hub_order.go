package cmd

import (
	"errors"
	"log"
	"net/http"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/samsaffron/term-llm/internal/hub"
)

const (
	hubNodeOrderPath      = apphub.NodeOrderPath
	hubNodeOrderBodyLimit = apphub.NodeOrderBodyLimit
)

type hubNodeOrderRequest = apphub.NodeOrderRequest

// hubNodeIDPresented reports whether a request claims a node identity.
func hubNodeIDPresented(r *http.Request) bool { return apphub.NodeIDPresented(r) }

// hubNodeOrderNodeRequest reports a node saving the dashboard order with its
// own credentials; see apphub.NodeOrderNodeRequest.
func hubNodeOrderNodeRequest(r *http.Request) bool { return apphub.NodeOrderNodeRequest(r) }

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

func decodeHubNodeOrderRequest(w http.ResponseWriter, r *http.Request, dst *hubNodeOrderRequest) error {
	return apphub.DecodeNodeOrderRequest(w, r, dst)
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
	s.nodeOrderLog.Report(err)
	return arranged
}

type hubRepeatLog = apphub.RepeatLog
