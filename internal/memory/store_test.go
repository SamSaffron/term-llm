package memory

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreFragmentCRUDAndSearch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	frag := &Fragment{
		Agent:   "reviewer",
		Path:    "preferences/editor",
		Content: "User prefers concise answers and durable summaries.",
		Source:  DefaultSourceMine,
	}
	if err := store.CreateFragment(ctx, frag); err != nil {
		t.Fatalf("CreateFragment() error = %v", err)
	}

	got, err := store.GetFragment(ctx, "reviewer", "preferences/editor")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetFragment() returned nil fragment")
	}
	if got.Content != frag.Content {
		t.Fatalf("content = %q, want %q", got.Content, frag.Content)
	}

	results, err := store.SearchFragments(ctx, "durable", 10, "reviewer")
	if err != nil {
		t.Fatalf("SearchFragments() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchFragments() returned no rows")
	}

	updated, err := store.UpdateFragment(ctx, "reviewer", "preferences/editor", "User prefers terse answers.")
	if err != nil {
		t.Fatalf("UpdateFragment() error = %v", err)
	}
	if !updated {
		t.Fatal("UpdateFragment() returned updated=false")
	}

	results, err = store.SearchFragments(ctx, "terse", 10, "reviewer")
	if err != nil {
		t.Fatalf("SearchFragments() after update error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SearchFragments() after update returned no rows")
	}
}

func TestFragmentSourcesCRUD(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	agent := "jarvis"
	fragPath := "fragments/homelab/setup.md"
	if err := store.CreateFragment(ctx, &Fragment{
		Agent:   agent,
		Path:    fragPath,
		Content: "homelab setup notes",
		Source:  DefaultSourceMine,
	}); err != nil {
		t.Fatalf("CreateFragment() error = %v", err)
	}

	if err := store.AddFragmentSource(ctx, agent, fragPath, "sess-1", 0, 14); err != nil {
		t.Fatalf("AddFragmentSource(sess-1) error = %v", err)
	}
	if err := store.AddFragmentSource(ctx, agent, fragPath, "sess-2", 14, 28); err != nil {
		t.Fatalf("AddFragmentSource(sess-2) error = %v", err)
	}

	sources, err := store.GetFragmentSources(ctx, agent, fragPath)
	if err != nil {
		t.Fatalf("GetFragmentSources() error = %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("GetFragmentSources() len = %d, want 2", len(sources))
	}
	if sources[0].SessionID != "sess-1" || sources[0].TurnStart != 0 || sources[0].TurnEnd != 14 {
		t.Fatalf("first source = %+v, want sess-1 [0,14)", sources[0])
	}
	if sources[1].SessionID != "sess-2" || sources[1].TurnStart != 14 || sources[1].TurnEnd != 28 {
		t.Fatalf("second source = %+v, want sess-2 [14,28)", sources[1])
	}

	if err := store.AddFragmentSource(ctx, agent, fragPath, "sess-1", 0, 14); err != nil {
		t.Fatalf("AddFragmentSource(duplicate) error = %v", err)
	}
	sources, err = store.GetFragmentSources(ctx, agent, fragPath)
	if err != nil {
		t.Fatalf("GetFragmentSources(after duplicate) error = %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("GetFragmentSources(after duplicate) len = %d, want 2", len(sources))
	}

	for _, tc := range []struct {
		sessionID string
		wantPath  string
		wantStart int
		wantEnd   int
	}{
		{sessionID: "sess-1", wantPath: fragPath, wantStart: 0, wantEnd: 14},
		{sessionID: "sess-2", wantPath: fragPath, wantStart: 14, wantEnd: 28},
	} {
		rows, err := store.GetSourcesForSession(ctx, tc.sessionID)
		if err != nil {
			t.Fatalf("GetSourcesForSession(%s) error = %v", tc.sessionID, err)
		}
		if len(rows) != 1 {
			t.Fatalf("GetSourcesForSession(%s) len = %d, want 1", tc.sessionID, len(rows))
		}
		if rows[0].Path != tc.wantPath || rows[0].TurnStart != tc.wantStart || rows[0].TurnEnd != tc.wantEnd {
			t.Fatalf("GetSourcesForSession(%s) row = %+v, want path=%s turns=[%d,%d)", tc.sessionID, rows[0], tc.wantPath, tc.wantStart, tc.wantEnd)
		}
	}

	deleted, err := store.DeleteFragment(ctx, agent, fragPath)
	if err != nil {
		t.Fatalf("DeleteFragment() error = %v", err)
	}
	if !deleted {
		t.Fatal("DeleteFragment() returned deleted=false")
	}

	sources, err = store.GetFragmentSources(ctx, agent, fragPath)
	if err != nil {
		t.Fatalf("GetFragmentSources(after delete) error = %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("GetFragmentSources(after delete) len = %d, want 0", len(sources))
	}

	secondPath := "fragments/homelab/network.md"
	if err := store.CreateFragment(ctx, &Fragment{
		Agent:   agent,
		Path:    secondPath,
		Content: "network notes",
		Source:  DefaultSourceMine,
	}); err != nil {
		t.Fatalf("CreateFragment(second) error = %v", err)
	}
	if err := store.AddFragmentSource(ctx, agent, secondPath, "sess-3", 3, 9); err != nil {
		t.Fatalf("AddFragmentSource(second) error = %v", err)
	}
	frags, err := store.ListFragments(ctx, ListOptions{Agent: agent})
	if err != nil {
		t.Fatalf("ListFragments(second) error = %v", err)
	}
	var secondRowID int64
	for _, f := range frags {
		if f.Path == secondPath {
			secondRowID = f.RowID
			break
		}
	}
	if secondRowID == 0 {
		t.Fatalf("failed to locate rowid for %s", secondPath)
	}

	deleted, err = store.DeleteFragmentByRowID(ctx, secondRowID)
	if err != nil {
		t.Fatalf("DeleteFragmentByRowID() error = %v", err)
	}
	if !deleted {
		t.Fatal("DeleteFragmentByRowID() returned deleted=false")
	}
	sources, err = store.GetFragmentSources(ctx, agent, secondPath)
	if err != nil {
		t.Fatalf("GetFragmentSources(after rowid delete) error = %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("GetFragmentSources(after rowid delete) len = %d, want 0", len(sources))
	}
}

func TestStoreEmbeddingMethods(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	frag := &Fragment{Agent: "jarvis", Path: "projects/term-llm/search", Content: "Hybrid search details"}
	if err := store.CreateFragment(ctx, frag); err != nil {
		t.Fatalf("CreateFragment() error = %v", err)
	}

	vec := []float64{0.1, 0.2, 0.3}
	if err := store.UpsertEmbedding(ctx, frag.ID, "gemini", "gemini-embedding-001", len(vec), vec); err != nil {
		t.Fatalf("UpsertEmbedding() error = %v", err)
	}

	got, err := store.GetEmbedding(ctx, frag.ID, "gemini", "gemini-embedding-001")
	if err != nil {
		t.Fatalf("GetEmbedding() error = %v", err)
	}
	if len(got) != len(vec) {
		t.Fatalf("embedding length = %d, want %d", len(got), len(vec))
	}
	for i := range vec {
		if math.Abs(got[i]-vec[i]) > 1e-9 {
			t.Fatalf("embedding[%d] = %f, want %f", i, got[i], vec[i])
		}
	}

	vec2 := []float64{0.3, 0.2, 0.1}
	if err := store.UpsertEmbedding(ctx, frag.ID, "gemini", "gemini-embedding-001", len(vec2), vec2); err != nil {
		t.Fatalf("UpsertEmbedding(update) error = %v", err)
	}
	got, err = store.GetEmbedding(ctx, frag.ID, "gemini", "gemini-embedding-001")
	if err != nil {
		t.Fatalf("GetEmbedding(update) error = %v", err)
	}
	for i := range vec2 {
		if math.Abs(got[i]-vec2[i]) > 1e-9 {
			t.Fatalf("updated embedding[%d] = %f, want %f", i, got[i], vec2[i])
		}
	}

	if _, err := store.UpdateFragment(ctx, frag.Agent, frag.Path, "Updated content"); err != nil {
		t.Fatalf("UpdateFragment() error = %v", err)
	}
	got, err = store.GetEmbedding(ctx, frag.ID, "gemini", "gemini-embedding-001")
	if err != nil {
		t.Fatalf("GetEmbedding(after update) error = %v", err)
	}
	if got != nil {
		t.Fatalf("expected stale embedding to be cleared on update, got %v", got)
	}
}

func TestUpdateFragmentNoOpWhenContentUnchanged(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	frag := &Fragment{Agent: "jarvis", Path: "notes/unchanged", Content: "original content"}
	if err := store.CreateFragment(ctx, frag); err != nil {
		t.Fatalf("CreateFragment() error = %v", err)
	}

	before, err := store.GetFragment(ctx, frag.Agent, frag.Path)
	if err != nil || before == nil {
		t.Fatalf("GetFragment() before: err=%v frag=%v", err, before)
	}

	// Small sleep so any timestamp change would be visible.
	time.Sleep(5 * time.Millisecond)

	// Update with identical content — should be a no-op.
	updated, err := store.UpdateFragment(ctx, frag.Agent, frag.Path, frag.Content)
	if err != nil {
		t.Fatalf("UpdateFragment(no-op) error = %v", err)
	}
	if updated {
		t.Fatal("UpdateFragment(no-op) returned updated=true, want false")
	}

	after, err := store.GetFragment(ctx, frag.Agent, frag.Path)
	if err != nil || after == nil {
		t.Fatalf("GetFragment() after: err=%v frag=%v", err, after)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at changed on no-op update: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestUpdateFragmentBumpsUpdatedAtOnRealChange(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	frag := &Fragment{Agent: "jarvis", Path: "notes/changing", Content: "version one"}
	if err := store.CreateFragment(ctx, frag); err != nil {
		t.Fatalf("CreateFragment() error = %v", err)
	}

	before, err := store.GetFragment(ctx, frag.Agent, frag.Path)
	if err != nil || before == nil {
		t.Fatalf("GetFragment() before: err=%v frag=%v", err, before)
	}

	time.Sleep(5 * time.Millisecond)

	updated, err := store.UpdateFragment(ctx, frag.Agent, frag.Path, "version two")
	if err != nil {
		t.Fatalf("UpdateFragment() error = %v", err)
	}
	if !updated {
		t.Fatal("UpdateFragment() returned updated=false, want true")
	}

	after, err := store.GetFragment(ctx, frag.Agent, frag.Path)
	if err != nil || after == nil {
		t.Fatalf("GetFragment() after: err=%v frag=%v", err, after)
	}
	if after.Content != "version two" {
		t.Fatalf("content = %q, want %q", after.Content, "version two")
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("updated_at not bumped: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestStoreGetEmbeddingsByIDs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	f1 := &Fragment{Agent: "jarvis", Path: "alpha", Content: "Alpha"}
	f2 := &Fragment{Agent: "jarvis", Path: "beta", Content: "Beta"}
	f3 := &Fragment{Agent: "jarvis", Path: "gamma", Content: "Gamma"}
	for _, f := range []*Fragment{f1, f2, f3} {
		if err := store.CreateFragment(ctx, f); err != nil {
			t.Fatalf("CreateFragment(%s) error = %v", f.Path, err)
		}
	}

	vec1 := []float64{0.4, 0.5}
	vec2 := []float64{0.6, 0.7}
	if err := store.UpsertEmbedding(ctx, f1.ID, "gemini", "gemini-embedding-001", len(vec1), vec1); err != nil {
		t.Fatalf("UpsertEmbedding(f1) error = %v", err)
	}
	if err := store.UpsertEmbedding(ctx, f2.ID, "gemini", "gemini-embedding-001", len(vec2), vec2); err != nil {
		t.Fatalf("UpsertEmbedding(f2) error = %v", err)
	}

	got, err := store.GetEmbeddingsByIDs(ctx, []string{f1.ID, f2.ID, f3.ID, f2.ID}, "gemini", "gemini-embedding-001")
	if err != nil {
		t.Fatalf("GetEmbeddingsByIDs() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetEmbeddingsByIDs() len = %d, want 2", len(got))
	}
	if vec, ok := got[f1.ID]; !ok || len(vec) != len(vec1) {
		t.Fatalf("GetEmbeddingsByIDs() missing f1 or wrong length: %#v", got)
	}
	if vec, ok := got[f2.ID]; !ok || len(vec) != len(vec2) {
		t.Fatalf("GetEmbeddingsByIDs() missing f2 or wrong length: %#v", got)
	}
	for i := range vec1 {
		if math.Abs(got[f1.ID][i]-vec1[i]) > 1e-9 {
			t.Fatalf("f1 embedding[%d] = %f, want %f", i, got[f1.ID][i], vec1[i])
		}
	}
	for i := range vec2 {
		if math.Abs(got[f2.ID][i]-vec2[i]) > 1e-9 {
			t.Fatalf("f2 embedding[%d] = %f, want %f", i, got[f2.ID][i], vec2[i])
		}
	}
}

func TestStoreGetFragmentsNeedingEmbedding(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	f1 := &Fragment{Agent: "jarvis", Path: "a", Content: "A"}
	f2 := &Fragment{Agent: "jarvis", Path: "b", Content: "B"}
	f3 := &Fragment{Agent: "reviewer", Path: "c", Content: "C"}
	for _, f := range []*Fragment{f1, f2, f3} {
		if err := store.CreateFragment(ctx, f); err != nil {
			t.Fatalf("CreateFragment(%s) error = %v", f.Path, err)
		}
	}

	if err := store.UpsertEmbedding(ctx, f1.ID, "gemini", "gemini-embedding-001", 2, []float64{1, 0}); err != nil {
		t.Fatalf("UpsertEmbedding() error = %v", err)
	}

	needJarvis, err := store.GetFragmentsNeedingEmbedding(ctx, "jarvis", "gemini", "gemini-embedding-001")
	if err != nil {
		t.Fatalf("GetFragmentsNeedingEmbedding(jarvis) error = %v", err)
	}
	if len(needJarvis) != 1 || needJarvis[0].ID != f2.ID {
		t.Fatalf("jarvis needing embedding = %#v, want only %s", needJarvis, f2.ID)
	}

	needAll, err := store.GetFragmentsNeedingEmbedding(ctx, "", "gemini", "gemini-embedding-001")
	if err != nil {
		t.Fatalf("GetFragmentsNeedingEmbedding(all) error = %v", err)
	}
	ids := map[string]bool{}
	for _, f := range needAll {
		ids[f.ID] = true
	}
	if !ids[f2.ID] || !ids[f3.ID] || len(ids) != 2 {
		t.Fatalf("all needing embedding IDs = %#v, want [%s %s]", ids, f2.ID, f3.ID)
	}
}

func TestStoreVectorSearchAndBumpAccess(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	f1 := &Fragment{Agent: "jarvis", Path: "top", Content: "Top hit", DecayScore: 0.6}
	f2 := &Fragment{Agent: "jarvis", Path: "mid", Content: "Mid hit"}
	f3 := &Fragment{Agent: "jarvis", Path: "low", Content: "Low hit"}
	f4 := &Fragment{Agent: "jarvis", Path: "other-model", Content: "Other model"}
	for _, f := range []*Fragment{f1, f2, f3, f4} {
		if err := store.CreateFragment(ctx, f); err != nil {
			t.Fatalf("CreateFragment(%s) error = %v", f.Path, err)
		}
	}

	mustUpsertEmbedding := func(id string, provider, model string, vec []float64) {
		t.Helper()
		if err := store.UpsertEmbedding(ctx, id, provider, model, len(vec), vec); err != nil {
			t.Fatalf("UpsertEmbedding(%s) error = %v", id, err)
		}
	}
	mustUpsertEmbedding(f1.ID, "gemini", "gemini-embedding-001", []float64{1, 0})
	mustUpsertEmbedding(f2.ID, "gemini", "gemini-embedding-001", []float64{0.8, 0.2})
	mustUpsertEmbedding(f3.ID, "gemini", "gemini-embedding-001", []float64{-1, 0})
	mustUpsertEmbedding(f4.ID, "openai", "text-embedding-3-large", []float64{1, 0})

	results, err := store.VectorSearch(ctx, "jarvis", "gemini", "gemini-embedding-001", []float64{1, 0}, 3)
	if err != nil {
		t.Fatalf("VectorSearch() error = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("VectorSearch() len = %d, want 3", len(results))
	}
	if results[0].ID != f1.ID {
		t.Fatalf("VectorSearch() top ID = %s, want %s", results[0].ID, f1.ID)
	}
	if results[0].Score < results[1].Score {
		t.Fatalf("scores out of order: %f < %f", results[0].Score, results[1].Score)
	}
	ids := map[string]bool{}
	for _, result := range results {
		ids[result.ID] = true
	}
	if !ids[f1.ID] || !ids[f2.ID] || !ids[f3.ID] {
		t.Fatalf("VectorSearch() IDs = %#v, want %s %s %s", ids, f1.ID, f2.ID, f3.ID)
	}
	if ids[f4.ID] {
		t.Fatalf("VectorSearch() unexpectedly included other provider/model id %s", f4.ID)
	}

	if err := store.BumpAccess(ctx, f1.ID); err != nil {
		t.Fatalf("BumpAccess(first) error = %v", err)
	}
	if err := store.BumpAccess(ctx, f1.ID); err != nil {
		t.Fatalf("BumpAccess(second) error = %v", err)
	}

	got, err := store.GetFragment(ctx, f1.Agent, f1.Path)
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetFragment() returned nil")
	}
	if got.AccessCount != 2 {
		t.Fatalf("access_count = %d, want 2", got.AccessCount)
	}
	if got.AccessedAt == nil {
		t.Fatal("accessed_at was not updated")
	}
	if math.Abs(got.DecayScore-0.8) > 1e-9 {
		t.Fatalf("decay_score after bumps = %f, want %f", got.DecayScore, 0.8)
	}

	if err := store.BumpAccess(ctx, "missing-id"); err == nil {
		t.Fatal("BumpAccess(missing-id) expected error")
	}
}

func TestStoreMiningState(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	st := &MiningState{
		SessionID:       "sess-1",
		Agent:           "reviewer",
		LastMinedOffset: 42,
		MinedAt:         time.Now().UTC(),
	}
	if err := store.UpsertState(ctx, st); err != nil {
		t.Fatalf("UpsertState() error = %v", err)
	}

	got, err := store.GetState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetState() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetState() returned nil")
	}
	if got.LastMinedOffset != 42 {
		t.Fatalf("offset = %d, want 42", got.LastMinedOffset)
	}

	st.LastMinedOffset = 100
	if err := store.UpsertState(ctx, st); err != nil {
		t.Fatalf("UpsertState(update) error = %v", err)
	}

	got, err = store.GetState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetState(update) error = %v", err)
	}
	if got.LastMinedOffset != 100 {
		t.Fatalf("offset after update = %d, want 100", got.LastMinedOffset)
	}
}

func TestStoreMetaGetSet(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	value, err := store.GetMeta(ctx, "last_promoted_at_jarvis")
	if err != nil {
		t.Fatalf("GetMeta(missing) error = %v", err)
	}
	if value != "" {
		t.Fatalf("GetMeta(missing) = %q, want empty", value)
	}

	if err := store.SetMeta(ctx, "last_promoted_at_jarvis", "2026-02-22T11:00:00Z"); err != nil {
		t.Fatalf("SetMeta(insert) error = %v", err)
	}
	value, err = store.GetMeta(ctx, "last_promoted_at_jarvis")
	if err != nil {
		t.Fatalf("GetMeta(after insert) error = %v", err)
	}
	if value != "2026-02-22T11:00:00Z" {
		t.Fatalf("GetMeta(after insert) = %q, want %q", value, "2026-02-22T11:00:00Z")
	}

	if err := store.SetMeta(ctx, "last_promoted_at_jarvis", "2026-02-22T12:00:00Z"); err != nil {
		t.Fatalf("SetMeta(update) error = %v", err)
	}
	value, err = store.GetMeta(ctx, "last_promoted_at_jarvis")
	if err != nil {
		t.Fatalf("GetMeta(after update) error = %v", err)
	}
	if value != "2026-02-22T12:00:00Z" {
		t.Fatalf("GetMeta(after update) = %q, want %q", value, "2026-02-22T12:00:00Z")
	}
}

func TestRecordImage(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	rec := &ImageRecord{
		Agent:      "jarvis",
		SessionID:  "sess-1",
		Prompt:     "Astronaut on the moon",
		OutputPath: "/tmp/astronaut.png",
		Provider:   "openai",
		Width:      512,
		Height:     512,
		FileSize:   2048,
	}
	if err := store.RecordImage(ctx, rec); err != nil {
		t.Fatalf("RecordImage() error = %v", err)
	}

	images, err := store.ListImages(ctx, ImageListOptions{Agent: "jarvis", Limit: 5})
	if err != nil {
		t.Fatalf("ListImages() error = %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("ListImages() len = %d, want 1", len(images))
	}
	got := images[0]
	if got.Agent != rec.Agent {
		t.Fatalf("agent = %q, want %q", got.Agent, rec.Agent)
	}
	if got.SessionID != rec.SessionID {
		t.Fatalf("session_id = %q, want %q", got.SessionID, rec.SessionID)
	}
	if got.Prompt != rec.Prompt {
		t.Fatalf("prompt = %q, want %q", got.Prompt, rec.Prompt)
	}
	if got.OutputPath != rec.OutputPath {
		t.Fatalf("output_path = %q, want %q", got.OutputPath, rec.OutputPath)
	}
	if got.Provider != rec.Provider {
		t.Fatalf("provider = %q, want %q", got.Provider, rec.Provider)
	}
	if got.MimeType != "image/png" {
		t.Fatalf("mime_type = %q, want image/png", got.MimeType)
	}
	if got.Width != rec.Width || got.Height != rec.Height {
		t.Fatalf("dimensions = %dx%d, want %dx%d", got.Width, got.Height, rec.Width, rec.Height)
	}
	if got.FileSize != rec.FileSize {
		t.Fatalf("file_size = %d, want %d", got.FileSize, rec.FileSize)
	}
	if got.ID == "" {
		t.Fatal("expected image ID to be set")
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("expected created_at to be set")
	}
}

func TestSearchImages(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	records := []*ImageRecord{
		{
			Agent:      "jarvis",
			Prompt:     "Astronaut floating in space",
			OutputPath: "/tmp/space.png",
			Provider:   "openai",
		},
		{
			Agent:      "reviewer",
			Prompt:     "Sunset over the mountains",
			OutputPath: "/tmp/sunset.png",
			Provider:   "openai",
		},
	}
	for _, rec := range records {
		if err := store.RecordImage(ctx, rec); err != nil {
			t.Fatalf("RecordImage() error = %v", err)
		}
	}

	results, err := store.SearchImages(ctx, "astronaut", "jarvis", 10)
	if err != nil {
		t.Fatalf("SearchImages() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("SearchImages() len = %d, want 1", len(results))
	}
	if results[0].Agent != "jarvis" {
		t.Fatalf("SearchImages() agent = %q, want jarvis", results[0].Agent)
	}
	if results[0].Prompt != records[0].Prompt {
		t.Fatalf("SearchImages() prompt = %q, want %q", results[0].Prompt, records[0].Prompt)
	}
}

func TestStoreRecalcDecayScores(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	now := time.Now().UTC()
	veryOld := &Fragment{
		Agent:     "jarvis",
		Path:      "decay/very-old",
		Content:   "very old",
		CreatedAt: now.Add(-400 * 24 * time.Hour),
		UpdatedAt: now.Add(-400 * 24 * time.Hour),
	}

	recentAccessedAt := now.Add(-1 * 24 * time.Hour)
	recent := &Fragment{
		Agent:      "jarvis",
		Path:       "decay/recent",
		Content:    "recent",
		CreatedAt:  now.Add(-20 * 24 * time.Hour),
		UpdatedAt:  now.Add(-20 * 24 * time.Hour),
		AccessedAt: &recentAccessedAt,
	}
	pinned := &Fragment{
		Agent:      "jarvis",
		Path:       "decay/pinned",
		Content:    "pinned",
		CreatedAt:  now.Add(-700 * 24 * time.Hour),
		UpdatedAt:  now.Add(-700 * 24 * time.Hour),
		Pinned:     true,
		DecayScore: 1.0,
	}
	otherAgent := &Fragment{
		Agent:     "reviewer",
		Path:      "decay/other",
		Content:   "other",
		CreatedAt: now.Add(-300 * 24 * time.Hour),
		UpdatedAt: now.Add(-300 * 24 * time.Hour),
	}

	for _, frag := range []*Fragment{veryOld, recent, pinned, otherAgent} {
		if err := store.CreateFragment(ctx, frag); err != nil {
			t.Fatalf("CreateFragment(%s) error = %v", frag.Path, err)
		}
	}

	updated, err := store.RecalcDecayScores(ctx, "jarvis", 0)
	if err != nil {
		t.Fatalf("RecalcDecayScores(jarvis) error = %v", err)
	}
	if updated != 2 {
		t.Fatalf("RecalcDecayScores(jarvis) updated = %d, want 2", updated)
	}

	veryOldGot, err := store.GetFragment(ctx, "jarvis", "decay/very-old")
	if err != nil {
		t.Fatalf("GetFragment(very-old) error = %v", err)
	}
	if veryOldGot == nil {
		t.Fatal("GetFragment(very-old) returned nil")
	}
	if math.Abs(veryOldGot.DecayScore-0.04) > 1e-9 {
		t.Fatalf("very-old decay_score = %f, want 0.04 floor", veryOldGot.DecayScore)
	}

	recentGot, err := store.GetFragment(ctx, "jarvis", "decay/recent")
	if err != nil {
		t.Fatalf("GetFragment(recent) error = %v", err)
	}
	if recentGot == nil {
		t.Fatal("GetFragment(recent) returned nil")
	}
	if recentGot.AccessedAt == nil {
		t.Fatal("GetFragment(recent) missing accessed_at")
	}
	lastActive := recentGot.UpdatedAt
	if recentGot.AccessedAt != nil && recentGot.AccessedAt.After(lastActive) {
		lastActive = *recentGot.AccessedAt
	}
	expectedRecent := math.Pow(0.5, time.Since(lastActive).Hours()/24.0/30.0)
	expectedRecent = math.Max(expectedRecent, 0.04)
	if math.Abs(recentGot.DecayScore-expectedRecent) > 1e-3 {
		t.Fatalf("recent decay_score = %f, want approximately %f", recentGot.DecayScore, expectedRecent)
	}

	pinnedGot, err := store.GetFragment(ctx, "jarvis", "decay/pinned")
	if err != nil {
		t.Fatalf("GetFragment(pinned) error = %v", err)
	}
	if pinnedGot == nil {
		t.Fatal("GetFragment(pinned) returned nil")
	}
	if pinnedGot.DecayScore != 1.0 {
		t.Fatalf("pinned decay_score = %f, want 1.0", pinnedGot.DecayScore)
	}

	otherGot, err := store.GetFragment(ctx, "reviewer", "decay/other")
	if err != nil {
		t.Fatalf("GetFragment(other) error = %v", err)
	}
	if otherGot == nil {
		t.Fatal("GetFragment(other) returned nil")
	}
	if otherGot.DecayScore != 1.0 {
		t.Fatalf("other-agent decay_score before global recalc = %f, want unchanged 1.0", otherGot.DecayScore)
	}

	updatedAll, err := store.RecalcDecayScores(ctx, "", 30.0)
	if err != nil {
		t.Fatalf("RecalcDecayScores(all) error = %v", err)
	}
	if updatedAll != 3 {
		t.Fatalf("RecalcDecayScores(all) updated = %d, want 3 non-pinned fragments", updatedAll)
	}

	otherGot, err = store.GetFragment(ctx, "reviewer", "decay/other")
	if err != nil {
		t.Fatalf("GetFragment(other after global recalc) error = %v", err)
	}
	if otherGot == nil {
		t.Fatal("GetFragment(other after global recalc) returned nil")
	}
	if otherGot.DecayScore < 0.04 || otherGot.DecayScore > 1.0 {
		t.Fatalf("other-agent decay_score after global recalc = %f, want within [0.04,1.0]", otherGot.DecayScore)
	}
}

func TestStoreCountGCCandidatesAndGCFragments(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	now := time.Now().UTC()
	stale := &Fragment{
		Agent:     "jarvis",
		Path:      "gc/stale",
		Content:   "gcstalealpha",
		CreatedAt: now.Add(-400 * 24 * time.Hour),
		UpdatedAt: now.Add(-400 * 24 * time.Hour),
	}
	pinned := &Fragment{
		Agent:     "jarvis",
		Path:      "gc/pinned",
		Content:   "gcpinned",
		CreatedAt: now.Add(-400 * 24 * time.Hour),
		UpdatedAt: now.Add(-400 * 24 * time.Hour),
		Pinned:    true,
	}
	keep := &Fragment{
		Agent:     "jarvis",
		Path:      "gc/keep",
		Content:   "gckeep",
		CreatedAt: now.Add(-5 * 24 * time.Hour),
		UpdatedAt: now.Add(-5 * 24 * time.Hour),
	}
	other := &Fragment{
		Agent:     "reviewer",
		Path:      "gc/other",
		Content:   "gcstalebeta",
		CreatedAt: now.Add(-500 * 24 * time.Hour),
		UpdatedAt: now.Add(-500 * 24 * time.Hour),
	}

	for _, frag := range []*Fragment{stale, pinned, keep, other} {
		if err := store.CreateFragment(ctx, frag); err != nil {
			t.Fatalf("CreateFragment(%s) error = %v", frag.Path, err)
		}
	}

	if err := store.UpsertEmbedding(ctx, stale.ID, "gemini", "gemini-embedding-001", 2, []float64{0.1, 0.2}); err != nil {
		t.Fatalf("UpsertEmbedding(stale) error = %v", err)
	}

	if _, err := store.RecalcDecayScores(ctx, "", 30.0); err != nil {
		t.Fatalf("RecalcDecayScores(all) error = %v", err)
	}

	countJarvis, err := store.CountGCCandidates(ctx, "jarvis")
	if err != nil {
		t.Fatalf("CountGCCandidates(jarvis) error = %v", err)
	}
	if countJarvis != 1 {
		t.Fatalf("CountGCCandidates(jarvis) = %d, want 1", countJarvis)
	}

	countAll, err := store.CountGCCandidates(ctx, "")
	if err != nil {
		t.Fatalf("CountGCCandidates(all) error = %v", err)
	}
	if countAll != 2 {
		t.Fatalf("CountGCCandidates(all) = %d, want 2", countAll)
	}

	removed, err := store.GCFragments(ctx, "jarvis")
	if err != nil {
		t.Fatalf("GCFragments(jarvis) error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("GCFragments(jarvis) removed = %d, want 1", removed)
	}

	staleGot, err := store.GetFragment(ctx, "jarvis", "gc/stale")
	if err != nil {
		t.Fatalf("GetFragment(gc/stale) error = %v", err)
	}
	if staleGot != nil {
		t.Fatalf("stale fragment still exists after gc: %#v", staleGot)
	}

	emb, err := store.GetEmbedding(ctx, stale.ID, "gemini", "gemini-embedding-001")
	if err != nil {
		t.Fatalf("GetEmbedding(stale after gc) error = %v", err)
	}
	if emb != nil {
		t.Fatalf("embedding for stale fragment should be deleted via cascade, got %v", emb)
	}

	searchResults, err := store.SearchFragments(ctx, "gcstalealpha", 10, "jarvis")
	if err != nil {
		t.Fatalf("SearchFragments(after gc) error = %v", err)
	}
	if len(searchResults) != 0 {
		t.Fatalf("SearchFragments(after gc) returned %d rows, want 0", len(searchResults))
	}

	countJarvis, err = store.CountGCCandidates(ctx, "jarvis")
	if err != nil {
		t.Fatalf("CountGCCandidates(jarvis after gc) error = %v", err)
	}
	if countJarvis != 0 {
		t.Fatalf("CountGCCandidates(jarvis after gc) = %d, want 0", countJarvis)
	}

	removedAll, err := store.GCFragments(ctx, "")
	if err != nil {
		t.Fatalf("GCFragments(all) error = %v", err)
	}
	if removedAll != 1 {
		t.Fatalf("GCFragments(all) removed = %d, want 1", removedAll)
	}

	countAll, err = store.CountGCCandidates(ctx, "")
	if err != nil {
		t.Fatalf("CountGCCandidates(all after gc) error = %v", err)
	}
	if countAll != 0 {
		t.Fatalf("CountGCCandidates(all after gc) = %d, want 0", countAll)
	}
}

func TestLastMinedByAgent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	// Nothing mined yet — should return empty map.
	m, err := store.LastMinedByAgent(ctx)
	if err != nil {
		t.Fatalf("LastMinedByAgent (empty) error = %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("LastMinedByAgent (empty) = %v, want empty map", m)
	}

	// Insert a row via UpsertState (stores RFC3339 via modernc driver).
	ref := time.Date(2026, 2, 22, 5, 0, 0, 0, time.UTC)
	if err := store.UpsertState(ctx, &MiningState{
		SessionID:       "sess-rfc3339",
		Agent:           "jarvis",
		LastMinedOffset: 10,
		MinedAt:         ref,
	}); err != nil {
		t.Fatalf("UpsertState error = %v", err)
	}

	m, err = store.LastMinedByAgent(ctx)
	if err != nil {
		t.Fatalf("LastMinedByAgent (after upsert) error = %v", err)
	}
	got, ok := m["jarvis"]
	if !ok {
		t.Fatal("LastMinedByAgent: missing 'jarvis' key")
	}
	if !got.Equal(ref) {
		t.Fatalf("LastMinedByAgent = %v, want %v", got, ref)
	}

	// Inject a row with the legacy Go time.String() format directly via SQL to
	// simulate rows created by older versions of the binary.
	legacyStr := "2026-02-22 16:33:08.122217311 +1100 AEDT m=+183.551756999"
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO memory_mining_state(session_id, agent, last_mined_offset, mined_at)
		VALUES('sess-legacy', 'legacy-agent', 5, ?)`, legacyStr)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	m, err = store.LastMinedByAgent(ctx)
	if err != nil {
		t.Fatalf("LastMinedByAgent (with legacy row) error = %v", err)
	}
	if _, ok := m["legacy-agent"]; !ok {
		t.Fatal("LastMinedByAgent: missing 'legacy-agent' key")
	}
}

func TestParseFlexibleTime(t *testing.T) {
	cases := []struct {
		input string
		wantY int
		wantM time.Month
		wantD int
	}{
		{"2026-02-22T05:00:00Z", 2026, time.February, 22},
		{"2026-02-22T16:33:08.122217311+11:00", 2026, time.February, 22},
		{"2026-02-22 16:33:08.122217311 +1100 AEDT m=+183.551756999", 2026, time.February, 22},
		{"2026-02-22 16:33:08 +1100 AEDT", 2026, time.February, 22},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseFlexibleTime(tc.input)
			if err != nil {
				t.Fatalf("parseFlexibleTime(%q) error = %v", tc.input, err)
			}
			gotUTC := got.UTC()
			// Compare date only (UTC); we just care it parsed without error and
			// landed on the right calendar day.
			if gotUTC.Year() != tc.wantY || gotUTC.Month() != tc.wantM {
				t.Fatalf("parseFlexibleTime(%q) = %v, want %d-%02d-%02d",
					tc.input, gotUTC, tc.wantY, tc.wantM, tc.wantD)
			}
		})
	}

	_, err := parseFlexibleTime("not-a-time")
	if err == nil {
		t.Fatal("parseFlexibleTime(invalid) expected error, got nil")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := NewStore(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}

// ── Insight tests ─────────────────────────────────────────────────────────────

func TestInsightCRUD(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	ins := &Insight{
		Agent:          "jarvis",
		Content:        "Do not expose internal tooling names in public posts.",
		CompactContent: "Never name internal tools in public writing.",
		Category:       "anti-pattern",
		TriggerDesc:    "writing a blog post or public article",
		Confidence:     0.9,
	}
	if err := store.CreateInsight(ctx, ins); err != nil {
		t.Fatalf("CreateInsight: %v", err)
	}
	if ins.ID == 0 {
		t.Fatal("expected non-zero ID after CreateInsight")
	}

	got, err := store.GetInsightByID(ctx, ins.ID)
	if err != nil {
		t.Fatalf("GetInsightByID: %v", err)
	}
	if got.Content != ins.Content {
		t.Errorf("content = %q, want %q", got.Content, ins.Content)
	}
	if got.CompactContent != ins.CompactContent {
		t.Errorf("compact_content = %q, want %q", got.CompactContent, ins.CompactContent)
	}
	if got.Category != "anti-pattern" {
		t.Errorf("category = %q, want anti-pattern", got.Category)
	}

	// Update content + compact together.
	if err := store.UpdateInsight(ctx, ins.ID, "Never mention NZBGeek or SABnzbd in public posts.", "No internal tool names in public posts."); err != nil {
		t.Fatalf("UpdateInsight: %v", err)
	}
	got2, _ := store.GetInsightByID(ctx, ins.ID)
	if got2.Content != "Never mention NZBGeek or SABnzbd in public posts." {
		t.Errorf("after update, content = %q", got2.Content)
	}
	if got2.CompactContent != "No internal tool names in public posts." {
		t.Errorf("after update, compact_content = %q", got2.CompactContent)
	}

	// Update content only — compact should be preserved.
	if err := store.UpdateInsight(ctx, ins.ID, "Keep internal tool names out of published writing.", ""); err != nil {
		t.Fatalf("UpdateInsight preserve compact: %v", err)
	}
	got3u, _ := store.GetInsightByID(ctx, ins.ID)
	if got3u.CompactContent != "No internal tool names in public posts." {
		t.Errorf("compact should be preserved when empty passed: %q", got3u.CompactContent)
	}

	// Delete
	deleted, err := store.DeleteInsight(ctx, ins.ID)
	if err != nil {
		t.Fatalf("DeleteInsight: %v", err)
	}
	if !deleted {
		t.Error("DeleteInsight returned false")
	}
	got3, err := store.GetInsightByID(ctx, ins.ID)
	if err != nil {
		t.Fatalf("GetInsightByID after delete: %v", err)
	}
	if got3 != nil {
		t.Error("expected nil after delete")
	}
}

func TestInsightListAndSearch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	for i, content := range []string{
		"Never expose private tool names in public writing.",
		"User prefers direct answers without hedging.",
		"Always search memory before answering questions about setup.",
	} {
		cat := []string{"anti-pattern", "communication-style", "workflow"}[i]
		if err := store.CreateInsight(ctx, &Insight{
			Agent:      "jarvis",
			Content:    content,
			Category:   cat,
			Confidence: 0.7 + float64(i)*0.1,
		}); err != nil {
			t.Fatalf("CreateInsight[%d]: %v", i, err)
		}
	}

	list, err := store.ListInsights(ctx, "jarvis", 10)
	if err != nil {
		t.Fatalf("ListInsights: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("len(list) = %d, want 3", len(list))
	}

	results, err := store.SearchInsights(ctx, "jarvis", "private tool names blog", 5)
	if err != nil {
		t.Fatalf("SearchInsights: %v", err)
	}
	if len(results) == 0 {
		t.Error("SearchInsights returned nothing for relevant query")
	}
	// Top result should be the anti-pattern about private tools.
	if results[0].Category != "anti-pattern" {
		t.Errorf("top result category = %q, want anti-pattern", results[0].Category)
	}
}

func TestInsightReinforce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	ins := &Insight{
		Agent:      "jarvis",
		Content:    "User always wants a recommendation, not a list of options.",
		Category:   "communication-style",
		Confidence: 0.5,
	}
	if err := store.CreateInsight(ctx, ins); err != nil {
		t.Fatalf("CreateInsight: %v", err)
	}

	if err := store.ReinforceInsight(ctx, ins.ID); err != nil {
		t.Fatalf("ReinforceInsight: %v", err)
	}

	got, _ := store.GetInsightByID(ctx, ins.ID)
	if got.Confidence <= 0.5 {
		t.Errorf("confidence after reinforce = %.3f, want > 0.5", got.Confidence)
	}
	// CreateInsight seeds reinforcement_count=1, so after one reinforce it's 2.
	if got.ReinforcementCount != 2 {
		t.Errorf("reinforcement_count = %d, want 2", got.ReinforcementCount)
	}
}

func TestInsightDecayAndGC(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	// Insert a high-confidence insight and backdate last_reinforced by 60 days.
	ins := &Insight{
		Agent:      "jarvis",
		Content:    "Old insight that should decay.",
		Category:   "workflow",
		Confidence: 0.8,
	}
	if err := store.CreateInsight(ctx, ins); err != nil {
		t.Fatalf("CreateInsight: %v", err)
	}
	// Manually backdate to simulate staleness.
	old := time.Now().Add(-60 * 24 * time.Hour)
	if _, err := store.db.ExecContext(ctx,
		`UPDATE memory_insights SET last_reinforced = ? WHERE id = ?`,
		old.Format(time.RFC3339), ins.ID,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Decay with 30-day half-life: after 60 days the confidence should halve twice.
	n, err := store.DecayInsights(ctx, "jarvis", 30.0)
	if err != nil {
		t.Fatalf("DecayInsights: %v", err)
	}
	if n == 0 {
		t.Fatal("DecayInsights updated 0 rows — expected at least 1")
	}
	got, _ := store.GetInsightByID(ctx, ins.ID)
	expected := 0.8 * math.Pow(2, -60.0/30.0) // ≈ 0.2
	if math.Abs(got.Confidence-expected) > 0.02 {
		t.Errorf("confidence after decay = %.4f, want ≈%.4f", got.Confidence, expected)
	}

	// Insert a fresh insight that should NOT be GC'd.
	fresh := &Insight{
		Agent:      "jarvis",
		Content:    "Fresh insight.",
		Category:   "workflow",
		Confidence: 0.85,
	}
	if err := store.CreateInsight(ctx, fresh); err != nil {
		t.Fatalf("CreateInsight fresh: %v", err)
	}

	// GC with threshold 0.3 — should remove the decayed insight (~0.2) but keep the fresh one.
	deleted, err := store.GCInsights(ctx, "jarvis", 0.3)
	if err != nil {
		t.Fatalf("GCInsights: %v", err)
	}
	if deleted != 1 {
		t.Errorf("GCInsights deleted %d rows, want 1", deleted)
	}

	remaining, _ := store.ListInsights(ctx, "jarvis", 10)
	if len(remaining) != 1 || remaining[0].ID != fresh.ID {
		t.Errorf("expected only fresh insight to remain, got %d entries", len(remaining))
	}
}

func TestInsightExpand(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	// Insight with both full and compact forms.
	if err := store.CreateInsight(ctx, &Insight{
		Agent:          "jarvis",
		Content:        "When writing public content, use generic stand-ins for private tools.",
		CompactContent: "Use generic names in public writing.",
		Category:       "anti-pattern",
		Confidence:     0.9,
	}); err != nil {
		t.Fatalf("CreateInsight with compact: %v", err)
	}

	// Insight without compact — should fall back to full content.
	if err := store.CreateInsight(ctx, &Insight{
		Agent:      "jarvis",
		Content:    "Always confirm before overwriting existing files.",
		Category:   "workflow",
		Confidence: 0.8,
	}); err != nil {
		t.Fatalf("CreateInsight no compact: %v", err)
	}

	out, err := store.ExpandInsights(ctx, "jarvis", "any", 500)
	if err != nil {
		t.Fatalf("ExpandInsights: %v", err)
	}
	if out == "" {
		t.Fatal("ExpandInsights returned empty string")
	}
	if !strings.Contains(out, "<insights>") {
		t.Errorf("output missing <insights> tag: %q", out)
	}
	// Compact form should appear, not the verbose form.
	if !strings.Contains(out, "Use generic names in public writing.") {
		t.Errorf("compact form not used in expansion: %q", out)
	}
	if strings.Contains(out, "use generic stand-ins for private tools") {
		t.Errorf("verbose form should not appear when compact is set: %q", out)
	}
	// Insight without compact should show full content.
	if !strings.Contains(out, "Always confirm before overwriting existing files.") {
		t.Errorf("full content fallback not working: %q", out)
	}

	// Empty bank for a different agent should return empty.
	out2, err := store.ExpandInsights(ctx, "other-agent", "any query", 500)
	if err != nil {
		t.Fatalf("ExpandInsights other-agent: %v", err)
	}
	if out2 != "" {
		t.Errorf("expected empty for unknown agent, got: %q", out2)
	}
}

func TestInsightMiningState(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	const sid = "sess-abc-123"
	const agent = "jarvis"

	// Initially unmined.
	ts, err := store.InsightMinedAt(ctx, sid)
	if err != nil {
		t.Fatalf("InsightMinedAt: %v", err)
	}
	if !ts.IsZero() {
		t.Errorf("expected zero time for unmined session, got %v", ts)
	}

	// Mark mined.
	if err := store.MarkInsightMined(ctx, sid, agent); err != nil {
		t.Fatalf("MarkInsightMined: %v", err)
	}

	ts2, err := store.InsightMinedAt(ctx, sid)
	if err != nil {
		t.Fatalf("InsightMinedAt after mark: %v", err)
	}
	if ts2.IsZero() {
		t.Error("expected non-zero time after MarkInsightMined")
	}

	// Idempotent: marking again updates mined_at but doesn't error.
	if err := store.MarkInsightMined(ctx, sid, agent); err != nil {
		t.Fatalf("MarkInsightMined second call: %v", err)
	}
	ts3, _ := store.InsightMinedAt(ctx, sid)
	if ts3.IsZero() {
		t.Error("expected non-zero time after second MarkInsightMined")
	}
}
