package session

import (
	"time"

	"github.com/samsaffron/term-llm/internal/agents/gist"
)

// ShareState records the current external share for a session. The generic
// fields are authoritative; legacy Gist fields remain for one compatibility
// path and are normalized/dual-written without a database migration.
type ShareState struct {
	Provider   string     `json:"provider,omitempty"`
	ID         string     `json:"id,omitempty"`
	URL        string     `json:"url,omitempty"`
	SourceURL  string     `json:"source_url,omitempty"`
	Visibility string     `json:"visibility,omitempty"`
	Scope      ShareScope `json:"scope,omitempty"`

	GistID     string `json:"gist_id,omitempty"`
	GistURL    string `json:"gist_url,omitempty"`
	PreviewURL string `json:"preview_url,omitempty"`
	Public     bool   `json:"public,omitempty"`

	SharedAt  time.Time `json:"shared_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Normalize upgrades legacy Gist state in memory and dual-writes compatibility
// fields for GitHub shares. It intentionally performs no database rewrite.
func (s *ShareState) Normalize() {
	if s == nil {
		return
	}
	if s.ID == "" && s.GistID != "" {
		s.Provider = gist.ProviderID
		s.ID = s.GistID
		s.URL = s.PreviewURL
		if s.URL == "" {
			s.URL = gist.PreviewURL(s.GistID)
		}
		s.SourceURL = s.GistURL
		if s.Public {
			s.Visibility = "public"
		} else {
			s.Visibility = "unlisted"
		}
		s.Scope = ShareScopeSession
	}
	if s.Provider == gist.ProviderID && s.ID != "" {
		if s.Visibility == "" {
			if s.Public {
				s.Visibility = "public"
			} else {
				s.Visibility = "unlisted"
			}
		}
		s.GistID = s.ID
		if s.URL == "" {
			s.URL = gist.PreviewURL(s.ID)
		}
		s.PreviewURL = s.URL
		if s.SourceURL == "" {
			s.SourceURL = gist.GetURL(s.ID)
		}
		s.GistURL = s.SourceURL
		s.Public = s.Visibility == "public"
	}
	if s.Scope == "" && s.ID != "" {
		s.Scope = ShareScopeSession
	}
}

// Clone returns a normalized copy that callers may mutate safely.
func (s *ShareState) Clone() *ShareState {
	if s == nil {
		return nil
	}
	clone := *s
	clone.Normalize()
	return &clone
}

// Exists reports whether this state identifies a generic or legacy share.
func (s *ShareState) Exists() bool {
	return s != nil && (s.ID != "" || s.GistID != "")
}
