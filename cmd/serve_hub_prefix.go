package cmd

import (
	"net/http"

	"github.com/samsaffron/term-llm/internal/app/serve/hub"
)

func (s *hubServer) hubPath(p string) string {
	return hub.Path(s.basePath, p)
}

func (s *hubServer) hubNodeMount(id string) string {
	return s.hubPath("/node/" + id)
}

func (s *hubServer) mountBasePath(next http.Handler) http.Handler {
	return hub.MountBasePath(s.basePath, next)
}
