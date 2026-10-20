//go:build js && wasm

package tools

import "fmt"

// RunAskUser fails closed on WebAssembly: the browser host has no controlling
// terminal and must not synthesize user answers.
func RunAskUser(questions []AskUserQuestion) ([]AskUserAnswer, error) {
	return nil, fmt.Errorf("interactive ask_user prompt unsupported on WebAssembly")
}
