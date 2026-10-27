package linkeddevices

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/samsaffron/term-llm/internal/peerbroker"
)

// User-facing text for the linked-devices service (the rendezvous broker).
// Messages name the configured service so custom broker_url deployments point
// the owner at the right setting, and say whether the server retries by itself.

const (
	msgNoService            = "No linked-devices service is configured."
	msgInvitationReconnect  = "The connection was briefly lost, so the open invitation was cancelled. Select Connect to create a new one."
	msgRevokeSyncPending    = "Device revoked on this server; the linked-devices service will be updated when it's reachable."
	msgCancelledSyncPending = "The cancelled pairing will be removed from the linked-devices service when it's reachable."
)

// brokerDisplayName returns host[:port][/path] for a broker URL, which is
// specific enough to identify a custom deployment without credentials.
func brokerDisplayName(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "the configured linked-devices service"
	}
	return u.Host + strings.TrimRight(u.EscapedPath(), "/")
}

func msgUnreachable(broker string) string {
	return fmt.Sprintf("Can't reach the linked-devices service at %s. Devices can't connect until it's reachable; retrying automatically.", brokerDisplayName(broker))
}

func msgNotAService(broker string, status int) string {
	return fmt.Sprintf("%s isn't responding as a linked-devices service (HTTP %d). Retrying automatically.", brokerDisplayName(broker), status)
}

func msgReconnecting(broker string) string {
	return fmt.Sprintf("Lost connection to the linked-devices service at %s. Reconnecting…", brokerDisplayName(broker))
}

func msgRejected(broker, detail string) string {
	return fmt.Sprintf("The linked-devices service at %s rejected this server's settings: %s. Fix the configuration, then select Resume.", brokerDisplayName(broker), detail)
}

func msgServiceChanged(pinned, configured string) string {
	return fmt.Sprintf("This server's devices were paired through the linked-devices service at %s, but it is now configured for %s. Select Forget all to switch, or restore the previous broker_url.", brokerDisplayName(pinned), brokerDisplayName(configured))
}

// connectionProblem describes a transient broker failure for the owner.
func connectionProblem(broker string, err error) string {
	var apiErr *peerbroker.APIError
	// A non-broker 4xx means something else answers at this address (for
	// example a placeholder site); 5xx usually means a proxy is up but the
	// broker behind it is not, which is simply unreachable.
	if errors.As(err, &apiErr) && strings.HasPrefix(apiErr.Code, "http_") && apiErr.StatusCode < 500 {
		return msgNotAService(broker, apiErr.StatusCode)
	}
	return msgUnreachable(broker)
}

// setBrokerProblem replaces the single current broker connection problem; an
// empty message clears it once the service is reachable again.
func (r *Runtime) setBrokerProblem(message string) {
	r.mu.Lock()
	r.brokerProblem = message
	r.mu.Unlock()
}
