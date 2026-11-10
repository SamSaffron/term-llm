//go:build js

package codemode

import (
	"context"
	"errors"
)

// Available is false on the browser host; other tools remain usable.
func Available() bool { return false }
func Run(context.Context, Input, []string, Host, Modules) Result {
	return Result{Status: "failed", Error: "code_mode unavailable on browser host"}
}
func StartCell(context.Context, CellSpec) (*Cell, error) {
	return nil, errors.New("code_mode unavailable on browser host")
}
