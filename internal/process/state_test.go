//go:build !windows

package process

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateHandoffPrivateSingleUseAndRollback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	p := Start("test", "")
	defer p.Stop()
	undo, err := SaveState("chat", map[string]string{"draft": "private draft"})
	if err != nil {
		t.Fatal(err)
	}
	defer undo(context.Background())
	hint := HandoffEnviron()
	if len(hint) != 1 {
		t.Fatal(hint)
	}
	path := strings.TrimPrefix(hint[0], stateEnv+"=")
	for name, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private state %s: %v %v", name, info, err)
		}
	}
	if _, err := SaveState("chat", nil); err == nil {
		t.Fatal("overwrote prepared handoff")
	}
	incomingState = path
	defer func() { incomingState = "" }()
	var restored map[string]string
	if _, err := RestoreState("chat", &restored); err == nil {
		t.Fatal("same exec consumed its outgoing state")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope stateEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Instance = "previous-exec"
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := RestoreState("telegram", &restored); err != nil || ok {
		t.Fatal("another component's valid handoff must remain unconsumed", ok, err)
	}
	ok, err := RestoreState("chat", &restored)
	if err != nil || !ok || restored["draft"] != "private draft" {
		t.Fatal(ok, err, restored)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("state not consumed", err)
	}
	if ok, err := RestoreState("chat", &restored); ok || err != nil {
		t.Fatal("state consumed twice", ok, err)
	}
	undo(context.Background())
	undo(context.Background())
	if len(HandoffEnviron()) != 0 {
		t.Fatal("rollback hint retained")
	}
}

func TestStateHandoffSupportsCombinedModeSections(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	publisher := Start("bundle-test", "")
	defer publisher.Stop()
	first, err := SaveState("web-runs", map[string]int{"response": 1})
	if err != nil {
		t.Fatal(err)
	}
	defer first(context.Background())
	second, err := SaveState("telegram:42", map[string]int{"offset": 2})
	if err != nil {
		t.Fatal(err)
	}
	defer second(context.Background())
	hint := strings.TrimPrefix(HandoffEnviron()[0], stateEnv+"=")
	var paths map[string]string
	if err := json.Unmarshal([]byte(hint), &paths); err != nil || len(paths) != 2 {
		t.Fatal(err, hint)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var envelope stateEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.Instance = "previous-image"
		raw, err = json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	incomingState = hint
	defer func() { incomingState = "" }()
	var value map[string]int
	if ok, err := RestoreState("absent", &value); ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := RestoreState("web-runs", &value); !ok || err != nil || value["response"] != 1 {
		t.Fatal(ok, err, value)
	}
	value = nil
	if ok, err := RestoreState("telegram:42", &value); !ok || err != nil || value["offset"] != 2 {
		t.Fatal(ok, err, value)
	}
	if incomingState != "" {
		t.Fatal("bundle not completely consumed")
	}
}
