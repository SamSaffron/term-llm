package peerconnect

import "github.com/samsaffron/term-llm/internal/peerbroker"

// ReadCapabilityFile reads a bounded private regular capability file.
func ReadCapabilityFile(path string) (string, error) { return peerbroker.ReadCapabilityFile(path) }
