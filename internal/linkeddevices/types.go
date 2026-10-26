package linkeddevices

import "time"

const (
	DefaultBrokerURL = ""
	BrokerURLEnv     = "TERM_LLM_LINKED_DEVICES_BROKER_URL"
	AdmissionEnv     = "TERM_LLM_LINKED_DEVICES_ADMISSION"
)

type State string

const (
	StateUnavailable  State = "unavailable"
	StateOff          State = "off"
	StateInviting     State = "inviting"
	StatePending      State = "pending"
	StateSharing      State = "sharing"
	StateReconnecting State = "reconnecting"
	StateStopped      State = "stopped"
)

type InvitationStatus struct {
	URL       string    `json:"url"`
	QRDataURL string    `json:"qr_data_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type RequestStatus struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	RequestedAt time.Time `json:"requested_at,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

type DeviceStatus struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	ApprovedAt time.Time  `json:"approved_at,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	Connection string     `json:"connection"`
}

type Status struct {
	State           State              `json:"state"`
	Reason          string             `json:"reason,omitempty"`
	ResetRequired   bool               `json:"reset_required,omitempty"`
	Enabled         bool               `json:"enabled"`
	BrokerConnected bool               `json:"broker_connected"`
	HostFingerprint string             `json:"host_fingerprint,omitempty"`
	Invitation      *InvitationStatus  `json:"invitation,omitempty"`
	PendingRequests []RequestStatus    `json:"pending_requests"`
	Devices         []DeviceStatus     `json:"devices"`
	Warnings        []string           `json:"warnings,omitempty"`
	Authority       AuthorityStatement `json:"authority"`
}

type AuthorityStatement struct {
	AgentName   string `json:"agent_name,omitempty"`
	Description string `json:"description"`
	SameUIDNote string `json:"same_uid_note"`
}

type approveRequest struct {
	Code string `json:"code"`
}
