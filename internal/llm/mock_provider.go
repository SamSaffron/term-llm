package llm

import "github.com/samsaffron/term-llm/internal/llm/mockprovider"

// The scripted mock provider lives in the leaf mockprovider package so engine
// tests can use it without importing root llm.

type (
	// MockTurn represents a single response turn from the mock provider.
	MockTurn = mockprovider.MockTurn
	// MockProvider is a configurable provider for testing.
	MockProvider = mockprovider.MockProvider
)

// NewMockProvider creates a new mock provider with the given name.
func NewMockProvider(name string) *MockProvider { return mockprovider.NewMockProvider(name) }
