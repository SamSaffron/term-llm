package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventV1GoldenJSON(t *testing.T) {
	timestamp := time.Date(2026, time.August, 29, 4, 5, 6, 123456789, time.FixedZone("AEST", 10*60*60))
	event := NewEvent(KindState, 1700000000000000001, timestamp, Metadata{
		Producer:   " term-llm ",
		PID:        4242,
		CWD:        "/work/project",
		ResumeArgv: []string{"/usr/local/bin/term-llm", "chat", "--resume=session-123"},
	}, Snapshot{State: Blocked, SessionID: " session-123 ", Message: "Waiting\n  for approval"})

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"producer":"term-llm","kind":"state","sequence":1700000000000000001,"timestamp":"2026-08-28T18:05:06.123456789Z","state":"blocked","message":"Waiting for approval","session_id":"session-123","pid":4242,"cwd":"/work/project","resume_argv":["/usr/local/bin/term-llm","chat","--resume=session-123"]}`
	if string(encoded) != want {
		t.Fatalf("event JSON = %s\nwant       = %s", encoded, want)
	}
}

func TestReleaseEventV1GoldenJSON(t *testing.T) {
	event := NewEvent(KindRelease, 8, time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC), Metadata{
		Producer: "term-llm",
		PID:      99,
		CWD:      "/tmp",
	}, Snapshot{State: Working, SessionID: "session-a", Message: "Generating"})
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"producer":"term-llm","kind":"release","sequence":8,"timestamp":"2026-08-29T01:02:03Z","state":"","message":"","session_id":"session-a","pid":99,"cwd":"/tmp"}`
	if string(encoded) != want {
		t.Fatalf("release JSON = %s\nwant         = %s", encoded, want)
	}
}

func TestNormalizeSnapshotSanitizesAndBounds(t *testing.T) {
	snapshot := NormalizeSnapshot(Snapshot{
		State:     Working,
		SessionID: "  session\x1b[31m\nvalue  ",
		Message:   strings.Repeat("界", maxMessageRunes+20) + "\x00",
	})
	if snapshot.SessionID != "session[31m value" {
		t.Fatalf("session = %q", snapshot.SessionID)
	}
	if got := len([]rune(snapshot.Message)); got != maxMessageRunes {
		t.Fatalf("message runes = %d, want %d", got, maxMessageRunes)
	}

	idle := NormalizeSnapshot(Snapshot{State: Idle, Message: "must disappear"})
	if idle.Message != "" {
		t.Fatalf("idle message = %q", idle.Message)
	}
	invalid := NormalizeSnapshot(Snapshot{State: "unknown"})
	if invalid.State != "" {
		t.Fatalf("invalid state = %q", invalid.State)
	}
}

func TestNormalizeMetadataCopiesAndBoundsResumeArgv(t *testing.T) {
	args := make([]string, maxResumeArgs+1)
	for i := range args {
		args[i] = "arg\x00"
	}
	metadata := NormalizeMetadata(Metadata{Producer: " producer\nname ", PID: -2, CWD: " /tmp/a\x00b ", ResumeArgv: args})
	if metadata.Producer != "producer name" || metadata.PID != 0 || metadata.CWD != "/tmp/ab" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if len(metadata.ResumeArgv) != maxResumeArgs || metadata.ResumeArgv[0] != "arg" {
		t.Fatalf("resume argv = %#v", metadata.ResumeArgv)
	}
	args[0] = "changed"
	if metadata.ResumeArgv[0] != "arg" {
		t.Fatal("normalized argv aliases caller storage")
	}
}
