// Command serve is a development-only static file server for the browser demo
// output. It is not the application backend: the page's API requests are
// intercepted in JavaScript and handled by the Go/WASM module.
package main

import (
	"flag"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	dir := flag.String("dir", "experiments/browser-bonsai/dist", "static output directory")
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	flag.Parse()
	if info, err := os.Stat(*dir); err != nil || !info.IsDir() {
		log.Fatalf("static directory %s not found; run experiments/browser-bonsai/build.sh first", *dir)
	}

	fileServer := http.FileServer(http.Dir(*dir))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.ToLower(filepath.Ext(r.URL.Path)) {
		case ".mjs", ".js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case ".wasm":
			w.Header().Set("Content-Type", "application/wasm")
		}
		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") || strings.HasSuffix(r.URL.Path, ".mjs") || strings.HasSuffix(r.URL.Path, ".wasm") {
			w.Header().Set("Cache-Control", "no-store")
		}
		fileServer.ServeHTTP(w, r)
	})
	if err := mime.AddExtensionType(".wasm", "application/wasm"); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("browser-bonsai static demo: http://%s/\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, handler))
}
