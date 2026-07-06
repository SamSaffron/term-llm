package serve

import (
	"context"
	"fmt"
)

// Options describes the resolved inputs required to run the serve service.
//
// The long-term goal for the serve command is for cmd/ to perform only flag
// parsing/validation and pass concrete values here. During the extraction, the
// legacy runner hook keeps behavior stable while serve orchestration is moved
// behind this package boundary incrementally.
type Options struct {
	Host string
	Port int

	AuthMode    string
	Token       string
	TokenSource string
	BasePath    string
	Title       string

	Platforms []string

	// Runner executes the current serve implementation. It is intentionally a
	// temporary seam: internal packages can be extracted without changing CLI
	// behavior, and cmd/serve.go can already be reduced to constructing a Service
	// and invoking Run.
	Runner func(context.Context) error
}

// Service owns serve orchestration.
type Service struct {
	opts Options
}

// NewService constructs a serve service from resolved options.
func NewService(opts Options) (*Service, error) {
	if opts.Runner == nil {
		return nil, fmt.Errorf("serve runner is required")
	}
	return &Service{opts: opts}, nil
}

// Run starts the service and blocks until ctx is cancelled or the service exits.
func (s *Service) Run(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("serve service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.opts.Runner(ctx)
}
