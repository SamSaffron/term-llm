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

	f1 := &Fragment{Agent: "jarvis", Path: "top", Content: "Top hit"}
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

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := NewStore(Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}
