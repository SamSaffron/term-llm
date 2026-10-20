//go:build js && wasm

// Command browser-bonsai runs the real term-llm engine, tool registry, and web
// chat UI inside a browser page. There is no backend: the Go/WASM process
// handles the page's HTTP surface in memory, and JavaScript owns inference.
package main

import (
	"time"

	"syscall/js"
)

func main() {
	// js/wasm derives a local zone from the host; the demo is UTC everywhere so
	// transcript timestamps and session ordering stay deterministic.
	time.Local = time.UTC

	inference := &jsInference{}
	app, err := NewApp(inference)
	if err != nil {
		publishFatal(err)
		select {}
	}
	bridge := newRequestBridge(app)
	installExports(app, bridge)
	js.Global().Set("termLLMBooted", true)

	// Keep the Go runtime alive for the page lifetime.
	select {}
}
