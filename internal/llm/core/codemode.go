package core

import (
	"context"
	"encoding/json"
)

// CodeModeModules resolves an immutable source snapshot, without executing it.
type CodeModeModules interface {
	Normalize(base, specifier string) (string, error)
	Source(name string) (string, error)
}

// CodeModeCallOutcome is bounded audit metadata, not an extra provider tool call.
type CodeModeCallOutcome struct {
	Arguments        json.RawMessage  `json:"arguments,omitempty"`
	ArgumentsOmitted bool             `json:"arguments_omitted,omitempty"`
	ID               string           `json:"call_id"`
	Name             string           `json:"name"`
	Status           string           `json:"status"`
	Outcome          string           `json:"outcome"`
	Error            string           `json:"error,omitempty"`
	IsError          bool             `json:"isError,omitempty"`
	GuardianReviews  []GuardianReview `json:"guardian_reviews,omitempty"`
}

// CodeModeCell is an engine-owned capability ceiling. Tools must fail closed
// without it; a source module or model cannot manufacture a dispatcher grant.
type CodeModeCell interface {
	CodeModeModules
	Names() []string
	Call(context.Context, string, json.RawMessage) (json.RawMessage, error)
	Describe(context.Context, []string) (json.RawMessage, error)
	Image(json.RawMessage) (ToolContentPart, error)
	Outcomes() []CodeModeCallOutcome
	Close()
}

type codeModeCellKey struct{}

func ContextWithCodeModeCell(ctx context.Context, cell CodeModeCell) context.Context {
	return context.WithValue(ctx, codeModeCellKey{}, cell)
}
func CodeModeCellFromContext(ctx context.Context) CodeModeCell {
	cell, _ := ctx.Value(codeModeCellKey{}).(CodeModeCell)
	return cell
}
