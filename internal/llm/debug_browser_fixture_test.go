//go:build browserfixture

package llm

import (
	"context"
	"testing"
	"testing/synctest"
)

func TestDebugBrowserGate(t *testing.T) {
	for _, action := range []string{"release", "cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := NewDebugBrowserGate()
				defer gate.Release()
				other := NewDebugBrowserGate()
				defer other.Release()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				stream, err := NewDebugProvider("fast").Stream(ctx, Request{Messages: []Message{UserText(gate.Prompt)}})
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				done := make(chan error, 1)
				go func() {
					_, err := stream.Recv()
					done <- err
				}()
				if err := gate.WaitStarted(ctx); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				// Releasing another test's gate must not unblock this response.
				other.Release()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("response advanced before release: %v", err)
				default:
				}
				switch action {
				case "release":
					gate.Release()
					gate.Release() // Teardown is safe after an explicit release.
					if _, ok := LookupDebugBrowserGate(gate.Prompt); ok {
						t.Fatal("released gate remains registered")
					}
				case "cancel":
					cancel()
				case "close":
					if err := stream.Close(); err != nil {
						t.Fatal(err)
					}
				}
				err = <-done
				if action == "release" && err != nil {
					t.Fatalf("released response failed: %v", err)
				}
				if action != "release" && err == nil {
					t.Fatal("canceled response emitted content")
				}
			})
		})
	}
}
