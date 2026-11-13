package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestClientToolMailboxCancellationTombstone(t *testing.T) {
	published := make(chan struct{})
	box := NewClientToolMailbox(map[string]bool{"phone": true}, 2, time.Hour, func(ctx context.Context, _ ToolCall) error {
		if err := BeginClientToolDispatch(ctx); err != nil {
			return err
		}
		close(published)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	call := ToolCall{ID: "device-effect", Name: "phone"}
	go func() { _, err := box.RunClientTool(ctx, call); done <- err }()
	<-published
	if len(box.RecoveryPending()) != 1 {
		t.Fatal("direct passthrough recovery was suppressed")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if len(box.Pending()) != 0 {
		t.Fatal("pending waiter leaked")
	}
	if box.Answer(call.ID, "late") != ClientToolAnswerUnknown {
		t.Fatal("late answer delivered after cancellation")
	}
	if _, err := box.RunClientTool(context.Background(), call); err == nil {
		t.Fatal("cancelled effect was replayed")
	}
}

func TestClientFunctionToolFullOffer(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		raw := `{"name":"phone","description":"device","parameters":{"type":"object"},"strict":true,"allowed_callers":["programmatic"],"output_schema":{"type":"string"}}`
		if wrapped {
			raw = `{"function":` + raw + `}`
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			t.Fatal(err)
		}
		spec, ok := ParseClientFunctionTool(object)
		if !ok || spec.Name != "phone" || spec.Description != "device" || !spec.Strict || len(spec.AllowedCallers) != 1 || spec.Schema["type"] != "object" || spec.OutputSchema["type"] != "string" {
			t.Fatalf("full offer lost: %+v", spec)
		}
	}
}

func TestClientLeaseRegistryBoundedRevocation(t *testing.T) {
	var registry ClientToolLeaseRegistry
	spec := []ToolSpec{{Name: "phone", Schema: map[string]any{"type": "object"}}}
	binding := ClientToolBinding{ResponseID: "r", SessionID: "s", ClientID: "c", Generation: "g"}
	first := registry.Admit(binding, spec, spec, 1, 1)
	if first == nil {
		t.Fatal("first lease denied")
	}
	if registry.Admit(binding, spec, spec, 1, 1) != nil {
		t.Fatal("registry grew past bound")
	}
	registry.Revoke("other observer", 2)
	if first.Check() != nil {
		t.Fatal("unrelated owner revoked lease")
	}
	registry.Revoke("owner gone", 1)
	if first.Check() == nil {
		t.Fatal("owner lease survived")
	}
	second := registry.Admit(binding, spec, spec, 2, 1)
	if second == nil {
		t.Fatal("terminal lease leaked registry place")
	}
	registry.End(second)
	if second.Check() == nil {
		t.Fatal("response end did not revoke")
	}
}

func TestClientMailboxPendingSnapshotFencesExpiredLease(t *testing.T) {
	lease, _ := newTestClientLease(t)
	published := make(chan struct{})
	box := NewClientToolMailbox(map[string]bool{"phone": true}, 2, time.Hour, func(ctx context.Context, _ ToolCall) error {
		if err := BeginClientToolDispatch(ctx); err != nil {
			return err
		}
		close(published)
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := lease.Dispatch(context.Background(), box, ToolCall{ID: "foreground", Name: "phone"})
		done <- err
	}()
	<-published
	if len(box.Pending()) != 1 {
		t.Fatal("active foreground missing from pending snapshot")
	}
	if len(box.RecoveryPending()) != 0 {
		t.Fatal("generic recovery exposed an active foreground call")
	}
	lease.Revoke("foreground ended")
	if len(box.Pending()) != 0 {
		t.Fatal("snapshot could redispatch expired foreground request")
	}
	<-done
}

func TestClientMailboxExpiredPendingAndReceipt(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending_unwinding", true: "accepted_receipt"}[accepted], func(t *testing.T) {
			lease, _ := newTestClientLease(t)
			published, release := make(chan struct{}), make(chan struct{})
			box := NewClientToolMailbox(map[string]bool{"phone": true}, 2, time.Hour, func(ctx context.Context, _ ToolCall) error {
				if err := BeginClientToolDispatch(ctx); err != nil {
					return err
				}
				close(published)
				if !accepted {
					<-release
				}
				return nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := lease.Dispatch(context.Background(), box, ToolCall{ID: "original", Name: "phone"})
				done <- err
			}()
			<-published
			if accepted {
				if got := box.Answer("original", "result"); got != ClientToolAnswerAccepted {
					t.Fatalf("accept=%v", got)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if got := box.Answer("original", "result"); got != ClientToolAnswerDuplicate {
					t.Fatalf("active duplicate=%v", got)
				}
			}
			lease.Revoke("owner ended")
			if got := box.Answer("original", "result"); got == ClientToolAnswerAccepted || got == ClientToolAnswerDuplicate {
				t.Fatalf("expired result acknowledged: %v", got)
			}
			if !accepted {
				close(release)
				<-done
			}
		})
	}
}

func TestClientMailboxBeforePublicationAndPublishFailure(t *testing.T) {
	lease, _ := newTestClientLease(t)
	entered, release := make(chan struct{}), make(chan struct{})
	box := NewClientToolMailbox(map[string]bool{"phone": true}, 2, time.Hour, func(context.Context, ToolCall) error {
		close(entered)
		<-release
		return errors.New("transport ended before Begin")
	})
	done := make(chan error, 1)
	go func() {
		_, err := lease.Dispatch(context.Background(), box, ToolCall{ID: "unpublished", Name: "phone"})
		done <- err
	}()
	<-entered
	if len(box.Pending()) != 0 {
		t.Fatal("recovery advertised a request before Begin/publication")
	}
	if box.Answer("unpublished", "premature") != ClientToolAnswerConflict {
		t.Fatal("unpublished request accepted an answer")
	}
	close(release)
	assertClientFailure(t, <-done, "client_tool_error", "not_dispatched")
	if len(box.Pending()) != 0 {
		t.Fatal("failed publication leaked pending state")
	}
}

func TestClientMailboxImmediateAnswerDuringPublish(t *testing.T) {
	lease, _ := newTestClientLease(t)
	var box *ClientToolMailbox
	box = NewClientToolMailbox(map[string]bool{"phone": true}, 2, time.Hour, func(ctx context.Context, call ToolCall) error {
		if err := BeginClientToolDispatch(ctx); err != nil {
			return err
		}
		if box.Answer(call.ID, "immediate") != ClientToolAnswerAccepted {
			return errors.New("waiter was not registered before publication")
		}
		return nil
	})
	out, err := lease.Dispatch(context.Background(), box, ToolCall{ID: "immediate", Name: "phone"})
	if err != nil || out.Content != "immediate" {
		t.Fatalf("immediate result=%+v %v", out, err)
	}
}
