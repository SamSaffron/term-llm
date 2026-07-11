package session

import "time"

// ShareState records the current external share for a session.
type ShareState struct {
	GistID     string    `json:"gist_id,omitempty"`
	GistURL    string    `json:"gist_url,omitempty"`
	PreviewURL string    `json:"preview_url,omitempty"`
	Public     bool      `json:"public,omitempty"`
	SharedAt   time.Time `json:"shared_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

// Clone returns a copy that callers may mutate safely.
func (s *ShareState) Clone() *ShareState {
	if s == nil {
		return nil
	}
	clone := *s
	return &clone
}

// Exists reports whether this state identifies a share.
func (s *ShareState) Exists() bool {
	return s != nil && s.GistID != ""
}
