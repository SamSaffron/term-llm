package serve

import (
	"io"
	"net/http"

	"github.com/samsaffron/term-llm/internal/app/serve/authorship"
)

// webAuthor is presentation metadata, never model context or authorization.
type webAuthor struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

func (s *serveServer) withIdentity(next http.HandlerFunc) http.HandlerFunc {
	return authorship.Middleware(next, s.cfg.identityHeader, s.cfg.identityNameHeader, s.cfg.identityEmailHeader)
}

func (s *serveServer) logIdentity(w io.Writer) {
	authorship.Log(w, s.cfg.identityHeader, s.cfg.identityNameHeader, s.cfg.identityEmailHeader)
}

var addViewerCapability = authorship.AddViewerCapability
