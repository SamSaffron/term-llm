package cmd

import appserve "github.com/samsaffron/term-llm/internal/app/serve"

// ServeOptions is the application configuration bound by the Cobra serve flags.
type ServeOptions = appserve.Options

// serveFlags is the Cobra-bound instance; startup and runtime helpers accept
// Options explicitly so independent callers need not mutate command globals.
var serveFlags ServeOptions
