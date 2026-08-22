package cmd

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
)

// The dashboard markup and styles live in editable .html/.css files so they
// keep editor support while the hub stays a single go:embed binary, matching
// the serveui convention. The CSS is inlined as template.CSS so the page is
// self-contained (no extra stylesheet request).
//
//go:embed templates/hub_index.html
var hubIndexHTML string

//go:embed templates/hub.css
var hubIndexCSS string

var hubIndexTmpl = template.Must(template.New("hub-index").Parse(hubIndexHTML))

type hubIndexView struct {
	CSS          template.CSS
	BasePathJSON template.JS
	BasePath     string
	// CanAddNodes toggles the Add Node UI.
	CanAddNodes   bool
	PasskeyAuth   bool
	AuthScriptURL string
}

func (s *hubServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	baseJSON, _ := json.Marshal(s.basePath)
	// Headers are committed if Execute fails mid-stream; nothing useful to
	// surface to the client at that point.
	_ = hubIndexTmpl.Execute(w, hubIndexView{
		CSS:           template.CSS(hubIndexCSS),
		BasePathJSON:  template.JS(string(baseJSON)),
		BasePath:      s.basePath,
		CanAddNodes:   s.store != nil,
		PasskeyAuth:   s.passkey != nil,
		AuthScriptURL: s.publicPath("/auth/hub_auth.js"),
	})
}
