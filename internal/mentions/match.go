package mentions

import (
	"container/heap"
	"context"
	"sort"
	"strings"
)

// Search performs deterministic whole-relative-path fuzzy matching.
func (s *Snapshot) Search(ctx context.Context, query string, limit int) []Match {
	if s == nil || limit <= 0 {
		return nil
	}
	query = lowerASCII(query)
	qbytes := []byte(query)
	var qmask [4]uint64
	for _, b := range qbytes {
		qmask[b>>6] |= 1 << (b & 63)
	}
	h := &matchHeap{matches: make([]Match, 0, limit), candidates: s.Candidates}
	positionScratch := make([]int, 0, len(qbytes))
	for i := range s.Candidates {
		if i&255 == 0 {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
		}
		c := &s.Candidates[i]
		if !containsMask(c.ASCII, qmask) {
			continue
		}
		score, positions, ok := scoreCandidate(*c, qbytes, positionScratch[:0])
		if !ok {
			continue
		}
		m := Match{Candidate: i, Score: score}
		if h.Len() < limit {
			m.Positions = append([]int(nil), positions...)
			heap.Push(h, m)
		} else if betterMatch(m, h.matches[0], s.Candidates) {
			m.Positions = append([]int(nil), positions...)
			h.matches[0] = m
			heap.Fix(h, 0)
		}
	}
	out := append([]Match(nil), h.matches...)
	sort.Slice(out, func(i, j int) bool { return betterMatch(out[i], out[j], s.Candidates) })
	return out
}

func containsMask(have, want [4]uint64) bool {
	for i := range have {
		if have[i]&want[i] != want[i] {
			return false
		}
	}
	return true
}

func scoreCandidate(c Candidate, query []byte, positions []int) (int, []int, bool) {
	if len(query) == 0 {
		return 1000 - strings.Count(c.Path, "/")*10 - len(c.Path), positions, true
	}
	path := []byte(c.LowerPath)
	positions = positions[:0]
	pi := 0
	score := 0
	for qi, q := range query {
		found := -1
		for pi < len(path) {
			if path[pi] == q {
				found = pi
				pi++
				break
			}
			pi++
		}
		if found < 0 {
			return 0, nil, false
		}
		positions = append(positions, found)
		score += 20
		if found == int(c.BaseOffset) || found > 0 && path[found-1] == '/' {
			score += 35
		}
		if qi > 0 {
			gap := found - positions[qi-1] - 1
			if gap == 0 {
				score += 28
			} else {
				score -= gap
			}
		}
	}
	base := path[c.BaseOffset:]
	q := string(query)
	if string(base) == q {
		score += 500
	} else if strings.HasPrefix(string(base), q) {
		score += 250
	} else if strings.Contains(string(base), q) {
		score += 100
	}
	score -= strings.Count(c.Path, "/") * 4
	score -= len(c.Path) / 8
	if c.Kind == KindDirectory {
		score -= 1
	}
	return score, positions, true
}

func betterMatch(a, b Match, candidates []Candidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	ap, bp := candidates[a.Candidate].Path, candidates[b.Candidate].Path
	if len(ap) != len(bp) {
		return len(ap) < len(bp)
	}
	return ap < bp
}

// matchHeap keeps the worst retained match at index zero.
type matchHeap struct {
	matches    []Match
	candidates []Candidate
}

func (h matchHeap) Len() int      { return len(h.matches) }
func (h matchHeap) Swap(i, j int) { h.matches[i], h.matches[j] = h.matches[j], h.matches[i] }
func (h matchHeap) Less(i, j int) bool {
	// container/heap puts the Less element at the root, so "less" means
	// worse according to the exact inverse of the final ordering.
	return betterMatch(h.matches[j], h.matches[i], h.candidates)
}
func (h *matchHeap) Push(x any) { h.matches = append(h.matches, x.(Match)) }
func (h *matchHeap) Pop() any {
	old := h.matches
	x := old[len(old)-1]
	h.matches = old[:len(old)-1]
	return x
}
