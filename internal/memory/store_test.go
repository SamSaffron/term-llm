package memory

import (
	"context"
	"math"
	"path/filepath"
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
