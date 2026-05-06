package cmd

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/widgets"
)

// widgetsRoute returns the widgets sub-route, e.g. "/ui/widgets/".
func (c serveServerConfig) widgetsRoute() string { return c.basePath + "/widgets/" }

// adminWidgetsRoute returns the admin widgets sub-route, e.g. "/ui/admin/widgets/".
func (c serveServerConfig) adminWidgetsRoute() string { return c.basePath + "/admin/widgets/" }

// registerWidgetRoutes mounts widget proxy and admin routes on inner mux.
// inner must be an http.ServeMux with basePath already stripped.
func (s *serveServer) registerWidgetRoutes(inner *http.ServeMux) {
	inner.HandleFunc("/widgets/", s.auth(s.handleWidgetProxy))
	inner.HandleFunc("/admin/widgets/reload", s.auth(s.cors(s.handleAdminWidgetsReload)))
	inner.HandleFunc("/admin/widgets/status", s.auth(s.cors(s.handleAdminWidgetsStatus)))
	inner.HandleFunc("/admin/widgets/", s.auth(s.cors(s.handleAdminWidgetStop)))
	inner.HandleFunc("/widgets", s.auth(s.handleWidgetIndex))
}

// handleWidgetIndex serves a simple HTML index page listing all widgets.
func (s *serveServer) handleWidgetIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/widgets" && r.URL.Path != "/widgets/" {
		http.NotFound(w, r)
		return
	}
	statuses := s.widgetsMgr.Status()
	errs := s.widgetsMgr.LoadErrors()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Widgets</title></head><body>`)
	fmt.Fprintf(w, `<h1>Widgets</h1>`)

	if len(errs) > 0 {
		fmt.Fprintf(w, `<h2>Load Errors</h2><ul>`)
		for _, e := range errs {
			fmt.Fprintf(w, `<li>%s</li>`, html.EscapeString(e.Error()))
		}
		fmt.Fprintf(w, `</ul>`)
	}

	if len(statuses) == 0 {
		fmt.Fprintf(w, `<p>No widgets loaded.</p>`)
	} else {
		fmt.Fprintf(w, `<table border="1" cellpadding="4"><tr><th>Mount</th><th>Title</th><th>State</th><th>Link</th></tr>`)
		for _, st := range statuses {
			link := s.cfg.basePath + "/widgets/" + st.Mount + "/"
			fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td><a href="%s">open</a></td></tr>`,
				html.EscapeString(st.Mount),
				html.EscapeString(st.Title),
				html.EscapeString(st.State),
				html.EscapeString(link),
			)
		}
		fmt.Fprintf(w, `</table>`)
	}
	fmt.Fprintf(w, `</body></html>`)
}

// handleWidgetProxy proxies requests to the appropriate widget process.
// Path: /widgets/<mount>/...
func (s *serveServer) handleWidgetProxy(w http.ResponseWriter, r *http.Request) {
	// Strip leading /widgets/ to get "mount/rest..."
	rest := strings.TrimPrefix(r.URL.Path, "/widgets/")
	if rest == "" {
		// /widgets/ with no mount - redirect to index
		http.Redirect(w, r, s.cfg.basePath+"/widgets", http.StatusTemporaryRedirect)
		return
	}

	parts := strings.SplitN(rest, "/", 2)
	mount := parts[0]
	if mount == "" {
		http.NotFound(w, r)
		return
	}

	// /widgets/<mount> (no trailing slash) → redirect so relative asset URLs work.
	if len(parts) == 1 {
		http.Redirect(w, r, s.cfg.basePath+"/widgets/"+mount+"/", http.StatusTemporaryRedirect)
		return
	}

	// Build the widget-relative path: "/" + everything after "mount/"
	widgetPath := "/" + parts[1]

	// Rewrite request path to the widget-relative path before proxying.
	r2 := r.Clone(r.Context())
	r2.URL.Path = widgetPath
	if r2.URL.RawPath != "" {
		rawRest := strings.TrimPrefix(r.URL.RawPath, "/widgets/"+mount)
		if rawRest == "" || rawRest == "/" {
			rawRest = "/"
		} else if !strings.HasPrefix(rawRest, "/") {
			rawRest = "/" + rawRest
		}
		r2.URL.RawPath = rawRest
	}

	s.widgetsMgr.Proxy(mount, w, r2)
}

// handleAdminWidgetsReload handles POST /admin/widgets/reload.
func (s *serveServer) handleAdminWidgetsReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	errs := s.widgetsMgr.Reload()
	type result struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors,omitempty"`
	}
	res := result{OK: len(errs) == 0}
	for _, e := range errs {
		res.Errors = append(res.Errors, e.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// handleAdminWidgetsStatus handles GET /admin/widgets/status.
func (s *serveServer) handleAdminWidgetsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type statusResponse struct {
		Widgets    []widgets.WidgetStatus `json:"widgets"`
		LoadErrors []string               `json:"load_errors,omitempty"`
	}
	resp := statusResponse{
		Widgets: s.widgetsMgr.Status(),
	}
	for _, e := range s.widgetsMgr.LoadErrors() {
		resp.LoadErrors = append(resp.LoadErrors, e.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAdminWidgetStop handles POST /admin/widgets/<mount>/stop.
func (s *serveServer) handleAdminWidgetStop(w http.ResponseWriter, r *http.Request) {
	// Path under inner mux: /admin/widgets/<mount>/stop
	rest := strings.TrimPrefix(r.URL.Path, "/admin/widgets/")
	mount, action, ok := strings.Cut(rest, "/")
	if !ok || action != "stop" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.widgetsMgr.StopMount(mount); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}
