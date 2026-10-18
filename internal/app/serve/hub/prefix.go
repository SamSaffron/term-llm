package hub

import (
	"net/http"
	"strings"
)

// Path maps an internal hub route ("/api/nodes", "/node/id/") to the
// externally mounted route under basePath. A root-mounted hub (empty basePath)
// keeps paths unchanged; a hub served with --base-path /hub emits
// "/hub/api/nodes" and "/hub/node/id/".
func Path(basePath, p string) string {
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if basePath == "" {
		return p
	}
	if p == "/" {
		return basePath + "/"
	}
	return basePath + p
}

// MountBasePath lets the hub run behind a reverse-proxy sub-path without nginx
// rewriting internal routes. Requests outside the mount are 404; the exact
// mount path redirects to its slash form so browser-relative fetches like
// "api/nodes" resolve under the mount.
func MountBasePath(base string, next http.Handler) http.Handler {
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == base {
			target := base + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, base+"/") {
			http.NotFound(w, r)
			return
		}

		stripped := strings.TrimPrefix(r.URL.Path, base)
		if stripped == "" {
			stripped = "/"
		}
		r2 := r.Clone(r.Context())
		u := *r.URL
		u.Path = stripped
		if u.RawPath != "" {
			if raw, ok := stripRawPathPrefix(u.RawPath, base); ok {
				u.RawPath = raw
			} else {
				u.RawPath = ""
			}
		}
		r2.URL = &u
		next.ServeHTTP(w, r2)
	})
}

func stripRawPathPrefix(rawPath, base string) (string, bool) {
	if rawPath == base {
		return "/", true
	}
	prefix := base + "/"
	if strings.HasPrefix(rawPath, prefix) {
		return strings.TrimPrefix(rawPath, base), true
	}
	return "", false
}
