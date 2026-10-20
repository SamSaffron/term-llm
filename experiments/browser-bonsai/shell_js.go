//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"sync"
	"syscall/js"
)

// browserShellExecutor calls the page's v86 Linux guest. The frozen contract is
// one JSON request string in and one JSON result string out; the page enforces
// command timeouts and Go cancels the VM if the bridge stalls.
type browserShellExecutor struct{}

func newShellExecutor() ShellExecutor { return &browserShellExecutor{} }

func (e *browserShellExecutor) RunShell(ctx context.Context, requestJSON string) (string, error) {
	run := js.Global().Get("termLLMRunShell")
	if run.Type() != js.TypeFunction {
		return "", fmt.Errorf("termLLMRunShell is not installed in the page")
	}

	var once sync.Once
	done := make(chan struct{})
	var result js.Value
	var failure string

	var then, catch js.Func
	then = js.FuncOf(func(_ js.Value, args []js.Value) any {
		once.Do(func() {
			if len(args) > 0 {
				result = args[0]
			}
			close(done)
		})
		then.Release()
		catch.Release()
		return nil
	})
	catch = js.FuncOf(func(_ js.Value, args []js.Value) any {
		once.Do(func() {
			if len(args) > 0 {
				if message := args[0].Get("message"); message.Type() == js.TypeString {
					failure = message.String()
				} else {
					failure = args[0].String()
				}
			}
			if failure == "" {
				failure = "browser Linux shell failed"
			}
			close(done)
		})
		then.Release()
		catch.Release()
		return nil
	})

	run.Invoke(requestJSON).Call("then", then, catch)

	select {
	case <-done:
		if failure != "" {
			return "", fmt.Errorf("%s", failure)
		}
		if result.Type() != js.TypeString {
			return "", fmt.Errorf("termLLMRunShell returned %s, want JSON string", result.Type().String())
		}
		return result.String(), nil
	case <-ctx.Done():
		// Stop the guest command so a timed-out or cancelled tool call cannot
		// keep the VM busy after the engine has given up on it.
		if cancel := js.Global().Get("termLLMCancelShell"); cancel.Type() == js.TypeFunction {
			cancel.Invoke()
		}
		return "", ctx.Err()
	}
}
