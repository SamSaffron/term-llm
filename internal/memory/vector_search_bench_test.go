package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestStoreVectorSearchLargeLimitFetchesFragmentsInBatches(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	count := fragmentByIDBatchSize + 5
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	fragStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_fragments (id, agent, path, content, source, created_at, updated_at, decay_score, pinned)
		VALUES (?, 'agent', ?, 'content', 'test', ?, ?, 1.0, 0)`)
	if err != nil {
		t.Fatalf("prepare fragment insert: %v", err)
	}
	defer fragStmt.Close()

	embStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_embeddings (fragment_id, provider, model, dimensions, vector, embedded_at)
		VALUES (?, 'test', 'model', 2, '[1,0]', ?)`)
	if err != nil {
		t.Fatalf("prepare embedding insert: %v", err)
	}
	defer embStmt.Close()

	base := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("large-limit-%03d", i)
		updatedAt := base.Add(time.Duration(i) * time.Second)
		if _, err := fragStmt.ExecContext(ctx, id, fmt.Sprintf("path/%03d", i), base, updatedAt); err != nil {
			t.Fatalf("insert fragment %d: %v", i, err)
		}
		if _, err := embStmt.ExecContext(ctx, id, updatedAt); err != nil {
			t.Fatalf("insert embedding %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	results, err := store.VectorSearch(ctx, "agent", "test", "model", []float64{1, 0}, count)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(results) != count {
		t.Fatalf("got %d results, want %d", len(results), count)
	}
	if want := fmt.Sprintf("large-limit-%03d", count-1); results[0].ID != want {
		t.Fatalf("top result = %s, want newest tied result %s", results[0].ID, want)
	}
}

func BenchmarkStoreVectorSearchLargeCorpus(b *testing.B) {
	ctx := context.Background()
	store, err := NewStore(Config{Path: ":memory:"})
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	const (
		fragmentCount = 3000
		dimensions    = 128
		limit         = 24
	)
	seedLargeVectorSearchBenchmark(b, ctx, store, fragmentCount, dimensions)
	queryVec := largeBenchmarkVector(99_999, dimensions)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, err := store.VectorSearch(ctx, "agent", "test", "model", queryVec, limit)
		if err != nil {
			b.Fatalf("VectorSearch: %v", err)
		}
		if len(results) != limit {
			b.Fatalf("got %d results, want %d", len(results), limit)
		}
	}
}

func seedLargeVectorSearchBenchmark(b *testing.B, ctx context.Context, store *Store, fragmentCount, dimensions int) {
	b.Helper()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	fragStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_fragments (id, agent, path, content, source, created_at, updated_at, decay_score, pinned)
		VALUES (?, 'agent', ?, ?, 'bench', ?, ?, 1.0, 0)`)
	if err != nil {
		b.Fatalf("prepare fragment insert: %v", err)
	}
	defer fragStmt.Close()

	embStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_embeddings (fragment_id, provider, model, dimensions, vector, embedded_at)
		VALUES (?, 'test', 'model', ?, ?, ?)`)
	if err != nil {
		b.Fatalf("prepare embedding insert: %v", err)
	}
	defer embStmt.Close()

	base := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	content := strings.Repeat("This is representative memory content with enough bytes to make unnecessary row reads visible. ", 32)
	for i := 0; i < fragmentCount; i++ {
		id := fmt.Sprintf("frag-%05d", i)
		updatedAt := base.Add(time.Duration(i) * time.Second)
		if _, err := fragStmt.ExecContext(ctx, id, fmt.Sprintf("path/%05d", i), content, base, updatedAt); err != nil {
			b.Fatalf("insert fragment %d: %v", i, err)
		}

		payload, err := json.Marshal(largeBenchmarkVector(i, dimensions))
		if err != nil {
			b.Fatalf("marshal vector %d: %v", i, err)
		}
		if _, err := embStmt.ExecContext(ctx, id, dimensions, payload, updatedAt); err != nil {
			b.Fatalf("insert embedding %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("Commit: %v", err)
	}
}

func largeBenchmarkVector(seed, dimensions int) []float64 {
	v := make([]float64, dimensions)
	for i := range v {
		x := float64((seed+1)*(i+17)) * 0.01337
		v[i] = math.Sin(x) + 0.5*math.Cos(x*0.7)
	}
	return v
}
