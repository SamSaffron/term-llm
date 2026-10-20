//go:build js && wasm

package tools

import "github.com/samsaffron/term-llm/internal/runtimeoutput"

// HuhApprovalPrompt fails closed on WebAssembly: there is no controlling
// terminal, so the prompt is denied rather than auto-approved.
func HuhApprovalPrompt(req *ApprovalRequest) (ConfirmOutcome, string) {
	runtimeoutput.Printf("%s\n", "Approval denied: interactive approval UI unsupported on WebAssembly")
	return Cancel, ""
}
