//go:build js || wasip1

package tea

// WebAssembly hosts have no terminal: no resize signals, no suspend, and
// input comes only from explicitly supplied readers.

func (p *Program) listenForResize(done chan struct{}) { close(done) }

func (p *Program) initInput() error { return nil }

const suspendSupported = false

func suspendProcess() {}
