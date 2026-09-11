package memory

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/embedding"
)

func hasEmbeddingVectorBinaryMagic(payload []byte) bool {
	if len(payload) < len(embeddingVectorBinaryMagic) {
		return false
	}
	for i := 0; i < len(embeddingVectorBinaryMagic); i++ {
		if payload[i] != embeddingVectorBinaryMagic[i] {
			return false
		}
	}
	return true
}

func encodeEmbeddingVector(vec []float64) []byte {
	headerLen := len(embeddingVectorBinaryMagic) + 4
	payload := make([]byte, headerLen+len(vec)*8)
	copy(payload, embeddingVectorBinaryMagic)
	binary.LittleEndian.PutUint32(payload[len(embeddingVectorBinaryMagic):headerLen], uint32(len(vec)))

	offset := headerLen
	for _, v := range vec {
		binary.LittleEndian.PutUint64(payload[offset:offset+8], math.Float64bits(v))
		offset += 8
	}
	return payload
}

func decodeEmbeddingVector(payload []byte) ([]float64, error) {
	if hasEmbeddingVectorBinaryMagic(payload) {
		return decodeBinaryEmbeddingVector(payload)
	}

	var vec []float64
	if err := json.Unmarshal(payload, &vec); err != nil {
		return nil, err
	}
	return vec, nil
}

func decodeBinaryEmbeddingVector(payload []byte) ([]float64, error) {
	dims, offset, err := parseBinaryEmbeddingVectorHeader(payload)
	if err != nil {
		return nil, err
	}

	vec := make([]float64, dims)
	for i := range vec {
		vec[i] = math.Float64frombits(binary.LittleEndian.Uint64(payload[offset : offset+8]))
		offset += 8
	}
	return vec, nil
}

func parseBinaryEmbeddingVectorHeader(payload []byte) (dims, offset int, err error) {
	headerLen := len(embeddingVectorBinaryMagic) + 4
	if len(payload) < headerLen {
		return 0, 0, fmt.Errorf("truncated binary embedding vector header")
	}
	encodedDims := binary.LittleEndian.Uint32(payload[len(embeddingVectorBinaryMagic):headerLen])
	if encodedDims > uint32((len(payload)-headerLen)/8) {
		return 0, 0, fmt.Errorf("binary embedding vector declares %d dimensions but payload has %d value bytes", encodedDims, len(payload)-headerLen)
	}
	dims = int(encodedDims)
	wantLen := headerLen + dims*8
	if len(payload) != wantLen {
		return 0, 0, fmt.Errorf("binary embedding vector length = %d, want %d for %d dimensions", len(payload), wantLen, dims)
	}
	return dims, headerLen, nil
}

const (
	vectorSearchPrefixDims     = 8
	vectorSearchExactBatchSize = 128
	vectorSearchUpperBoundEps  = 1e-9
)

type embeddingSearchSummary struct {
	prefix     [vectorSearchPrefixDims]float64
	tailNorm   float64
	vectorNorm float64
}

func summarizeEmbeddingSearchVector(vec []float64) embeddingSearchSummary {
	var s embeddingSearchSummary
	var tailNormSq float64
	for i, v := range vec {
		s.vectorNorm += v * v
		if i < vectorSearchPrefixDims {
			s.prefix[i] = v
		} else {
			tailNormSq += v * v
		}
	}
	if s.vectorNorm > 0 {
		s.vectorNorm = math.Sqrt(s.vectorNorm)
	}
	if tailNormSq > 0 {
		s.tailNorm = math.Sqrt(tailNormSq)
	}
	return s
}

func cosineSimilarityPayload(queryVec []float64, payload []byte) (float64, error) {
	if hasEmbeddingVectorBinaryMagic(payload) {
		return cosineSimilarityBinaryPayload(queryVec, payload)
	}

	vec, err := decodeEmbeddingVector(payload)
	if err != nil {
		return 0, err
	}
	return embedding.CosineSimilarity(queryVec, vec), nil
}

func cosineSimilarityBinaryPayload(queryVec []float64, payload []byte) (float64, error) {
	dims, offset, err := parseBinaryEmbeddingVectorHeader(payload)
	if err != nil {
		return 0, err
	}
	if dims != len(queryVec) || dims == 0 {
		return 0, nil
	}

	var dotProduct, normA, normB float64
	for _, a := range queryVec {
		b := math.Float64frombits(binary.LittleEndian.Uint64(payload[offset : offset+8]))
		offset += 8
		dotProduct += a * b
		normA += a * a
		normB += b * b
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0, nil
	}
	return dotProduct / denom, nil
}

// UpsertEmbedding inserts or updates an embedding vector for a fragment.
func (s *Store) UpsertEmbedding(ctx context.Context, fragmentID, provider, model string, dims int, vec []float64) error {
	fragmentID = strings.TrimSpace(fragmentID)
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if fragmentID == "" {
		return fmt.Errorf("fragment_id is required")
	}
	if provider == "" {
		return fmt.Errorf("provider is required")
	}
	if model == "" {
		return fmt.Errorf("model is required")
	}
	if len(vec) == 0 {
		return fmt.Errorf("vector cannot be empty")
	}
	if dims <= 0 {
		dims = len(vec)
	}
	if len(vec) != dims {
		return fmt.Errorf("vector dimensions mismatch: got %d values, dims=%d", len(vec), dims)
	}

	payload := encodeEmbeddingVector(vec)
	summary := summarizeEmbeddingSearchVector(vec)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_embeddings(
			fragment_id,
			provider,
			model,
			dimensions,
			vector,
			search_prefix_0,
			search_prefix_1,
			search_prefix_2,
			search_prefix_3,
			search_prefix_4,
			search_prefix_5,
			search_prefix_6,
			search_prefix_7,
			search_tail_norm,
			search_vector_norm,
			embedded_at
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fragment_id, provider, model) DO UPDATE SET
			dimensions = excluded.dimensions,
			vector = excluded.vector,
			search_prefix_0 = excluded.search_prefix_0,
			search_prefix_1 = excluded.search_prefix_1,
			search_prefix_2 = excluded.search_prefix_2,
			search_prefix_3 = excluded.search_prefix_3,
			search_prefix_4 = excluded.search_prefix_4,
			search_prefix_5 = excluded.search_prefix_5,
			search_prefix_6 = excluded.search_prefix_6,
			search_prefix_7 = excluded.search_prefix_7,
			search_tail_norm = excluded.search_tail_norm,
			search_vector_norm = excluded.search_vector_norm,
			embedded_at = excluded.embedded_at`,
		fragmentID,
		provider,
		model,
		dims,
		payload,
		summary.prefix[0],
		summary.prefix[1],
		summary.prefix[2],
		summary.prefix[3],
		summary.prefix[4],
		summary.prefix[5],
		summary.prefix[6],
		summary.prefix[7],
		summary.tailNorm,
		summary.vectorNorm,
		time.Now())
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}
	return nil
}

// GetEmbedding fetches a stored embedding vector for a fragment+provider+model.
func (s *Store) GetEmbedding(ctx context.Context, fragmentID, provider, model string) ([]float64, error) {
	fragmentID = strings.TrimSpace(fragmentID)
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if fragmentID == "" || provider == "" || model == "" {
		return nil, fmt.Errorf("fragment_id, provider, and model are required")
	}

	var payload []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT vector
		FROM memory_embeddings
		WHERE fragment_id = ? AND provider = ? AND model = ?`,
		fragmentID, provider, model).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}

	vec, err := decodeEmbeddingVector(payload)
	if err != nil {
		return nil, fmt.Errorf("decode embedding vector: %w", err)
	}
	return vec, nil
}

func (s *Store) GetEmbeddingsByIDs(ctx context.Context, fragmentIDs []string, provider, model string) (map[string][]float64, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return nil, fmt.Errorf("provider and model are required")
	}
	if len(fragmentIDs) == 0 {
		return map[string][]float64{}, nil
	}

	seen := map[string]struct{}{}
	ids := make([]string, 0, len(fragmentIDs))
	for _, id := range fragmentIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return map[string][]float64{}, nil
	}

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = strings.TrimSuffix(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT fragment_id, vector
		FROM memory_embeddings
		WHERE provider = ? AND model = ? AND fragment_id IN (%s)`, placeholders)

	args := make([]any, 0, len(ids)+2)
	args = append(args, provider, model)
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get embeddings by ids: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]float64, len(ids))
	for rows.Next() {
		var id string
		var payload sql.RawBytes
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("scan embedding row: %w", err)
		}
		vec, err := decodeEmbeddingVector(payload)
		if err != nil {
			return nil, fmt.Errorf("decode embedding vector: %w", err)
		}
		out[id] = vec
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type embeddingSearchBackfillRow struct {
	rowID   int64
	summary embeddingSearchSummary
}

func (s *Store) backfillVectorSearchColumns(ctx context.Context, provider, model string, dimensions int) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, vector
		FROM memory_embeddings
		WHERE provider = ? AND model = ? AND dimensions = ? AND search_vector_norm < 0`,
		provider, model, dimensions)
	if err != nil {
		return fmt.Errorf("query pending vector search metadata: %w", err)
	}
	defer rows.Close()

	var pending []embeddingSearchBackfillRow
	for rows.Next() {
		var row embeddingSearchBackfillRow
		var payload sql.RawBytes
		if err := rows.Scan(&row.rowID, &payload); err != nil {
			return fmt.Errorf("scan pending vector search metadata: %w", err)
		}
		vec, err := decodeEmbeddingVector(payload)
		if err != nil {
			return fmt.Errorf("decode pending vector search vector row %d: %w", row.rowID, err)
		}
		row.summary = summarizeEmbeddingSearchVector(vec)
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin vector search metadata backfill: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE memory_embeddings
		SET search_prefix_0 = ?,
		    search_prefix_1 = ?,
		    search_prefix_2 = ?,
		    search_prefix_3 = ?,
		    search_prefix_4 = ?,
		    search_prefix_5 = ?,
		    search_prefix_6 = ?,
		    search_prefix_7 = ?,
		    search_tail_norm = ?,
		    search_vector_norm = ?
		WHERE rowid = ?`)
	if err != nil {
		return fmt.Errorf("prepare vector search metadata backfill: %w", err)
	}
	defer stmt.Close()

	for _, row := range pending {
		if _, err := stmt.ExecContext(ctx,
			row.summary.prefix[0],
			row.summary.prefix[1],
			row.summary.prefix[2],
			row.summary.prefix[3],
			row.summary.prefix[4],
			row.summary.prefix[5],
			row.summary.prefix[6],
			row.summary.prefix[7],
			row.summary.tailNorm,
			row.summary.vectorNorm,
			row.rowID,
		); err != nil {
			return fmt.Errorf("update vector search metadata row %d: %w", row.rowID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit vector search metadata backfill: %w", err)
	}
	return nil
}

// GetFragmentsNeedingEmbedding returns fragments missing an embedding row for provider+model.
func (s *Store) GetFragmentsNeedingEmbedding(ctx context.Context, agent, provider, model string) ([]Fragment, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return nil, fmt.Errorf("provider and model are required")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.agent, f.path, f.content, f.source, f.created_at, f.updated_at,
		       f.accessed_at, f.access_count, f.decay_score, f.pinned
		FROM memory_fragments f
		LEFT JOIN memory_embeddings e
		  ON e.fragment_id = f.id AND e.provider = ? AND e.model = ?
		WHERE e.fragment_id IS NULL
		  AND (? = '' OR f.agent = ?)
		ORDER BY f.updated_at DESC`, provider, model, strings.TrimSpace(agent), strings.TrimSpace(agent))
	if err != nil {
		return nil, fmt.Errorf("query fragments needing embedding: %w", err)
	}
	defer rows.Close()

	var out []Fragment
	for rows.Next() {
		frag, scanErr := scanFragment(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan fragment: %w", scanErr)
		}
		out = append(out, *frag)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

const vectorSearchSQL = `
		SELECT e.fragment_id,
		       ((? * e.search_prefix_0) +
		        (? * e.search_prefix_1) +
		        (? * e.search_prefix_2) +
		        (? * e.search_prefix_3) +
		        (? * e.search_prefix_4) +
		        (? * e.search_prefix_5) +
		        (? * e.search_prefix_6) +
		        (? * e.search_prefix_7) +
		        (? * e.search_tail_norm)) /
		       CASE
		         WHEN e.search_vector_norm > 0 THEN e.search_vector_norm
		         ELSE 1
		       END AS upper_bound
		FROM memory_embeddings e
		JOIN memory_fragments f ON f.id = e.fragment_id
		WHERE e.provider = ? AND e.model = ? AND e.dimensions = ?
		  AND e.search_vector_norm >= 0
		  AND (? = '' OR f.agent = ?)
		ORDER BY upper_bound DESC, f.updated_at DESC`

type vectorSearchCandidate struct {
	id        string
	updatedAt time.Time
	score     float64
	vector    []float64
}

type vectorSearchCandidateHeap []vectorSearchCandidate

func (h vectorSearchCandidateHeap) Len() int { return len(h) }
func (h vectorSearchCandidateHeap) Less(i, j int) bool {
	return vectorSearchCandidateWorse(h[i], h[j])
}
func (h vectorSearchCandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *vectorSearchCandidateHeap) Push(x any) {
	*h = append(*h, x.(vectorSearchCandidate))
}
func (h *vectorSearchCandidateHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func buildVectorSearchBatchSQL(ids []string) string {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	return fmt.Sprintf(`
		SELECT e.fragment_id,
		       f.updated_at,
		       e.vector
		FROM memory_embeddings e
		JOIN memory_fragments f ON f.id = e.fragment_id
		WHERE e.provider = ? AND e.model = ? AND e.dimensions = ?
		  AND e.fragment_id IN (%s)`, placeholders)
}

func (s *Store) scoreVectorSearchBatch(ctx context.Context, provider, model string, dimensions int, queryVec []float64, ids []string, limit int, top *vectorSearchCandidateHeap) error {
	if len(ids) == 0 {
		return nil
	}

	query := buildVectorSearchBatchSQL(ids)
	args := make([]any, 0, len(ids)+3)
	args = append(args, provider, model, dimensions)
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query exact vector search batch: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c vectorSearchCandidate
		var payload sql.RawBytes
		if err := rows.Scan(&c.id, &c.updatedAt, &payload); err != nil {
			return fmt.Errorf("scan exact vector search row: %w", err)
		}

		score, err := cosineSimilarityPayload(queryVec, payload)
		if err != nil {
			return fmt.Errorf("score stored vector for fragment %s: %w", c.id, err)
		}
		c.score = score

		if top.Len() < limit {
			c.vector, err = decodeEmbeddingVector(payload)
			if err != nil {
				return fmt.Errorf("decode stored vector for fragment %s: %w", c.id, err)
			}
			heap.Push(top, c)
			continue
		}

		if vectorSearchCandidateBetter(c, (*top)[0]) {
			c.vector, err = decodeEmbeddingVector(payload)
			if err != nil {
				return fmt.Errorf("decode stored vector for fragment %s: %w", c.id, err)
			}
			(*top)[0] = c
			heap.Fix(top, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// VectorSearch performs an exact cosine similarity search while using SQL-side
// prefix/tail upper bounds to avoid loading every embedding blob into Go.
func (s *Store) VectorSearch(ctx context.Context, agent, provider, model string, queryVec []float64, limit int) ([]ScoredFragment, error) {
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("query vector cannot be empty")
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	agent = strings.TrimSpace(agent)
	if provider == "" || model == "" {
		return nil, fmt.Errorf("provider and model are required")
	}
	if limit <= 0 {
		limit = 24
	}

	if err := s.backfillVectorSearchColumns(ctx, provider, model, len(queryVec)); err != nil {
		return nil, err
	}

	querySummary := summarizeEmbeddingSearchVector(queryVec)
	if querySummary.vectorNorm > 0 {
		for i := range querySummary.prefix {
			querySummary.prefix[i] /= querySummary.vectorNorm
		}
		querySummary.tailNorm /= querySummary.vectorNorm
	}
	rows, err := s.db.QueryContext(ctx, vectorSearchSQL,
		querySummary.prefix[0],
		querySummary.prefix[1],
		querySummary.prefix[2],
		querySummary.prefix[3],
		querySummary.prefix[4],
		querySummary.prefix[5],
		querySummary.prefix[6],
		querySummary.prefix[7],
		querySummary.tailNorm,
		provider,
		model,
		len(queryVec),
		agent,
		agent,
	)
	if err != nil {
		return nil, fmt.Errorf("vector search query: %w", err)
	}
	defer rows.Close()

	top := make(vectorSearchCandidateHeap, 0, limit)
	pending := make([]string, 0, vectorSearchExactBatchSize)
	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := s.scoreVectorSearchBatch(ctx, provider, model, len(queryVec), queryVec, pending, limit, &top); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	for rows.Next() {
		var id string
		var upperBound float64
		if err := rows.Scan(&id, &upperBound); err != nil {
			return nil, fmt.Errorf("scan vector search candidate: %w", err)
		}
		if top.Len() >= limit && upperBound+vectorSearchUpperBoundEps < top[0].score {
			break
		}

		pending = append(pending, id)
		if len(pending) >= vectorSearchExactBatchSize {
			if err := flushPending(); err != nil {
				return nil, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := flushPending(); err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return []ScoredFragment{}, nil
	}

	sort.Slice(top, func(i, j int) bool {
		return vectorSearchCandidateBetter(top[i], top[j])
	})

	ids := make([]string, 0, len(top))
	for _, c := range top {
		ids = append(ids, c.id)
	}
	fragments, err := s.getFragmentsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]ScoredFragment, 0, len(top))
	for _, c := range top {
		r, ok := fragments[c.id]
		if !ok {
			continue
		}
		r.Score = c.score
		r.Vector = c.vector
		out = append(out, r)
	}
	return out, nil
}

func vectorSearchCandidateBetter(a, b vectorSearchCandidate) bool {
	if a.score == b.score {
		return a.updatedAt.After(b.updatedAt)
	}
	return a.score > b.score
}

func vectorSearchCandidateWorse(a, b vectorSearchCandidate) bool {
	if a.score == b.score {
		return a.updatedAt.Before(b.updatedAt)
	}
	return a.score < b.score
}

const fragmentByIDBatchSize = 500

func (s *Store) getFragmentsByIDs(ctx context.Context, ids []string) (map[string]ScoredFragment, error) {
	out := make(map[string]ScoredFragment, len(ids))
	for start := 0; start < len(ids); start += fragmentByIDBatchSize {
		end := start + fragmentByIDBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.getFragmentsByIDBatch(ctx, ids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) getFragmentsByIDBatch(ctx context.Context, ids []string, out map[string]ScoredFragment) error {
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = strings.TrimSuffix(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE id IN (%s)`, placeholders)

	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("get fragments by ids: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r ScoredFragment
		var accessedAt sql.NullTime
		if err := rows.Scan(
			&r.ID,
			&r.Agent,
			&r.Path,
			&r.Content,
			&r.Source,
			&r.CreatedAt,
			&r.UpdatedAt,
			&accessedAt,
			&r.AccessCount,
			&r.DecayScore,
			&r.Pinned,
		); err != nil {
			return fmt.Errorf("scan fragment by id: %w", err)
		}
		if accessedAt.Valid {
			at := accessedAt.Time
			r.AccessedAt = &at
		}
		out[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// BumpAccess marks a fragment as recently accessed and increments access_count.
// It intentionally does not modify decay_score; recency/freshness is applied as
// an explicit, non-persistent search-time option.
