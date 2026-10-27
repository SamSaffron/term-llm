package peer

import (
	"strings"
	"time"
)

// DiagnosticObserver receives closed, privacy-safe transport lifecycle events.
// Implementations must not block transport progress.
type DiagnosticObserver func(DiagnosticEvent)

// DiagnosticStage identifies one finite transport lifecycle point.
type DiagnosticStage string

const (
	DiagnosticBrokerConnected    DiagnosticStage = "broker_connected"
	DiagnosticBrokerDisconnected DiagnosticStage = "broker_disconnected"
	DiagnosticRecoveryAttempt    DiagnosticStage = "recovery_attempt"
	DiagnosticIdentityVerified   DiagnosticStage = "identity_verified"
	DiagnosticICEConnected       DiagnosticStage = "ice_connected"
	DiagnosticChannelOpen        DiagnosticStage = "channel_open"
	DiagnosticRequestStart       DiagnosticStage = "request_start"
	DiagnosticResponseHeaders    DiagnosticStage = "response_headers"
	DiagnosticRequestEnd         DiagnosticStage = "request_end"
	DiagnosticRequestCancel      DiagnosticStage = "request_cancel"
	DiagnosticRequestFailure     DiagnosticStage = "request_failure"
)

// DiagnosticOutcome is a finite terminal classification.
type DiagnosticOutcome string

const (
	DiagnosticOutcomeConnected    DiagnosticOutcome = "connected"
	DiagnosticOutcomeDisconnected DiagnosticOutcome = "disconnected"
	DiagnosticOutcomeStarted      DiagnosticOutcome = "started"
	DiagnosticOutcomeFinished     DiagnosticOutcome = "finished"
	DiagnosticOutcomeCanceled     DiagnosticOutcome = "canceled"
	DiagnosticOutcomeTruncated    DiagnosticOutcome = "truncated"
	DiagnosticOutcomeFailed       DiagnosticOutcome = "failed"
)

// DiagnosticReason is a finite failure or cancellation reason.
type DiagnosticReason string

const (
	DiagnosticReasonNone            DiagnosticReason = ""
	DiagnosticReasonChannelClosed   DiagnosticReason = "channel_closed"
	DiagnosticReasonPeerCanceled    DiagnosticReason = "peer_canceled"
	DiagnosticReasonContextCanceled DiagnosticReason = "context_canceled"
	DiagnosticReasonSendFailed      DiagnosticReason = "send_failed"
	DiagnosticReasonHandlerFailed   DiagnosticReason = "handler_failed"
)

// DiagnosticMethod is an allowlisted HTTP method.
type DiagnosticMethod string

const (
	DiagnosticMethodGET     DiagnosticMethod = "GET"
	DiagnosticMethodPOST    DiagnosticMethod = "POST"
	DiagnosticMethodPUT     DiagnosticMethod = "PUT"
	DiagnosticMethodPATCH   DiagnosticMethod = "PATCH"
	DiagnosticMethodDELETE  DiagnosticMethod = "DELETE"
	DiagnosticMethodHEAD    DiagnosticMethod = "HEAD"
	DiagnosticMethodOPTIONS DiagnosticMethod = "OPTIONS"
	DiagnosticMethodOther   DiagnosticMethod = "OTHER"
)

// DiagnosticRoute is a finite route template that never contains caller data.
type DiagnosticRoute string

const (
	DiagnosticRouteHealth         DiagnosticRoute = "/healthz"
	DiagnosticRouteCapabilities   DiagnosticRoute = "/v1/capabilities"
	DiagnosticRouteProviders      DiagnosticRoute = "/v1/providers"
	DiagnosticRouteModels         DiagnosticRoute = "/v1/models"
	DiagnosticRouteSidebar        DiagnosticRoute = "/v1/sidebar"
	DiagnosticRouteProjects       DiagnosticRoute = "/v1/projects"
	DiagnosticRouteSessions       DiagnosticRoute = "/v1/sessions"
	DiagnosticRouteSessionStatus  DiagnosticRoute = "/v1/sessions/status"
	DiagnosticRouteSessionSearch  DiagnosticRoute = "/v1/sessions/search"
	DiagnosticRouteSession        DiagnosticRoute = "/v1/sessions/:id"
	DiagnosticRouteResponses      DiagnosticRoute = "/v1/responses"
	DiagnosticRouteResponse       DiagnosticRoute = "/v1/responses/:id"
	DiagnosticRouteEvents         DiagnosticRoute = "/v1/events"
	DiagnosticRouteEventsPoll     DiagnosticRoute = "/v1/events/poll"
	DiagnosticRouteAttention      DiagnosticRoute = "/v1/attention"
	DiagnosticRouteMentionsSearch DiagnosticRoute = "/v1/mentions/search"
	DiagnosticRouteAsset          DiagnosticRoute = "/assets"
	DiagnosticRouteShell          DiagnosticRoute = "/shell"
	DiagnosticRouteOther          DiagnosticRoute = "other"
)

// DiagnosticCandidateClass is a finite ICE candidate class.
type DiagnosticCandidateClass string

const (
	DiagnosticCandidateHost    DiagnosticCandidateClass = "host"
	DiagnosticCandidateSrflx   DiagnosticCandidateClass = "srflx"
	DiagnosticCandidatePrflx   DiagnosticCandidateClass = "prflx"
	DiagnosticCandidateRelay   DiagnosticCandidateClass = "relay"
	DiagnosticCandidateUnknown DiagnosticCandidateClass = "unknown"
)

// DiagnosticEvent contains only finite classifications, monotonic durations,
// bounded counters, and process-local numeric sequence values.
type DiagnosticEvent struct {
	Stage      DiagnosticStage
	Outcome    DiagnosticOutcome
	Reason     DiagnosticReason
	Method     DiagnosticMethod
	Route      DiagnosticRoute
	Candidate  DiagnosticCandidateClass
	Generation uint64
	Connection uint64
	Request    uint64
	Status     int
	Bytes      uint64
	Duration   time.Duration
}

func diagnosticMethod(method string) DiagnosticMethod {
	switch strings.ToUpper(method) {
	case "GET":
		return DiagnosticMethodGET
	case "POST":
		return DiagnosticMethodPOST
	case "PUT":
		return DiagnosticMethodPUT
	case "PATCH":
		return DiagnosticMethodPATCH
	case "DELETE":
		return DiagnosticMethodDELETE
	case "HEAD":
		return DiagnosticMethodHEAD
	case "OPTIONS":
		return DiagnosticMethodOPTIONS
	default:
		return DiagnosticMethodOther
	}
}

func diagnosticRoute(target string) DiagnosticRoute {
	path, _, _ := strings.Cut(target, "?")
	if index := strings.Index(path, "/v1/"); index >= 0 {
		path = path[index:]
	} else if strings.HasSuffix(path, "/healthz") {
		path = "/healthz"
	}
	switch path {
	case "/healthz":
		return DiagnosticRouteHealth
	case "/v1/capabilities":
		return DiagnosticRouteCapabilities
	case "/v1/providers":
		return DiagnosticRouteProviders
	case "/v1/models":
		return DiagnosticRouteModels
	case "/v1/sidebar":
		return DiagnosticRouteSidebar
	case "/v1/projects":
		return DiagnosticRouteProjects
	case "/v1/sessions":
		return DiagnosticRouteSessions
	case "/v1/sessions/status":
		return DiagnosticRouteSessionStatus
	case "/v1/sessions/search":
		return DiagnosticRouteSessionSearch
	case "/v1/responses":
		return DiagnosticRouteResponses
	case "/v1/events":
		return DiagnosticRouteEvents
	case "/v1/events/poll":
		return DiagnosticRouteEventsPoll
	case "/v1/attention":
		return DiagnosticRouteAttention
	case "/v1/mentions/search":
		return DiagnosticRouteMentionsSearch
	}
	switch {
	case strings.HasPrefix(path, "/v1/sessions/"):
		return DiagnosticRouteSession
	case strings.HasPrefix(path, "/v1/responses/"):
		return DiagnosticRouteResponse
	case strings.HasPrefix(path, "/dist/"), strings.HasPrefix(path, "/images/"), strings.HasPrefix(path, "/media/"), strings.HasPrefix(path, "/files/"), strings.HasPrefix(path, "/uploads/"):
		return DiagnosticRouteAsset
	case strings.HasPrefix(path, "/v1/shell"), strings.HasPrefix(path, "/v1/terminals"):
		return DiagnosticRouteShell
	default:
		return DiagnosticRouteOther
	}
}
