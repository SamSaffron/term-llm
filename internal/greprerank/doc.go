// Package greprerank scores grep match blocks against a search intent with a
// classify backend and returns them in relevance order.
//
// It is pure logic with no tool or config dependencies: the backend is opened
// lazily (once, sticky on failure), requests are chunked, a single deadline
// covers every step, results are validated all-or-nothing, and a circuit
// breaker stops repeated failures from slowing grep down.
package greprerank
