package debuglog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchTextQueryFiltersByDaysAndReturnsMatches(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	writeDebugSearchFixture(t, dir, "recent", now.Add(-1*time.Hour), []string{
		debugSearchEventLine(now.Add(-59*time.Minute), "recent", "text_delta", `{"text":"The NeedLe is here"}`),
	})
	writeDebugSearchFixture(t, dir, "old", now.Add(-48*time.Hour), []string{
		debugSearchEventLine(now.Add(-48*time.Hour+time.Minute), "old", "text_delta", `{"text":"needle but too old"}`),
	})

	results, err := Search(dir, SearchOptions{Query: "needle", Days: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %#v", len(results), results)
	}
	got := results[0]
	if got.SessionID != "recent" {
		t.Fatalf("SessionID = %q, want recent", got.SessionID)
	}
	if got.EntryType != "event" || got.EventType != "text_delta" {
		t.Fatalf("entry = %s/%s, want event/text_delta", got.EntryType, got.EventType)
	}
	if !strings.Contains(strings.ToLower(got.Match), "needle") {
		t.Fatalf("Match = %q, want snippet containing needle", got.Match)
	}
}

func BenchmarkSearchTextQuery(b *testing.B) {
	dir := b.TempDir()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	const sessionCount = 12
	const eventsPerSession = 500
	for i := 0; i < sessionCount; i++ {
		sessionID := fmt.Sprintf("bench-%02d", i)
		start := base.Add(time.Duration(i) * time.Minute)
		extra := make([]string, 0, eventsPerSession)
		for j := 0; j < eventsPerSession; j++ {
			text := fmt.Sprintf("ordinary debug event session=%d line=%d", i, j)
			if j == eventsPerSession-1 {
				text = fmt.Sprintf("ordinary debug event with needle session=%d", i)
			}
			extra = append(extra, debugSearchEventLine(start.Add(time.Duration(j+1)*time.Second), sessionID, "text_delta", fmt.Sprintf(`{"text":%q}`, text)))
		}
		writeDebugSearchFixture(b, dir, sessionID, start, extra)
	}

	opts := SearchOptions{Query: "needle", Days: 7}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, err := Search(dir, opts)
		if err != nil {
			b.Fatalf("Search: %v", err)
		}
		if len(results) != sessionCount {
			b.Fatalf("got %d results, want %d", len(results), sessionCount)
		}
	}
}

func writeDebugSearchFixture(tb testing.TB, dir, sessionID string, start time.Time, extraLines []string) {
	tb.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("create fixture: %v", err)
	}
	defer f.Close()

	fmt.Fprintf(f, `{"timestamp":%q,"session_id":%q,"type":"session_start","command":"term-llm","args":[],"cwd":"/tmp"}`+"\n", start.Format(time.RFC3339Nano), sessionID)
	fmt.Fprintf(f, `{"timestamp":%q,"session_id":%q,"type":"request","provider":"mock","model":"mock-model","request":{"messages":[]}}`+"\n", start.Add(time.Millisecond).Format(time.RFC3339Nano), sessionID)
	for _, line := range extraLines {
		fmt.Fprintln(f, line)
	}
}

func debugSearchEventLine(ts time.Time, sessionID, eventType, data string) string {
	return fmt.Sprintf(`{"timestamp":%q,"session_id":%q,"type":"event","event_type":%q,"data":%s}`,
		ts.Format(time.RFC3339Nano), sessionID, eventType, data)
}
