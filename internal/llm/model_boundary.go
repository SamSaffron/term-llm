package llm

import "context"

// ModelBoundaryCallback runs before an engine-managed model turn, after the
// preceding turn's tools and persistence callbacks have returned. The callback
// may block to park the invocation, but must honor ctx cancellation. Returning
// nil continues the same invocation; returning an error ends it without issuing
// another provider request. It is not called on a naturally completed response.
//
// This is a cooperation point, not proof of a durable checkpoint: callers must
// separately validate their transcript fence and any independently owned work.
// Native inline tool loops cannot offer this boundary and are rejected.
type ModelBoundaryCallback func(context.Context) error
