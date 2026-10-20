package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime/progressive"
)

// Ask command-body compatibility remains until C3.
type progressiveStopWhen = progressive.StopWhen

type askProgressiveOptions = progressive.AskOptions

type progressiveRunResult = progressive.RunResult

func validateAskProgressiveOptions(opts *askProgressiveOptions) error {
	return progressive.ValidateAskOptions(opts)
}
