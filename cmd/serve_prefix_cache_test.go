package cmd

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestComputeMessageHashes(t *testing.T) {
	m1 := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}}
	m2 := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "world"}}}
	m3 := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "foo"}}}

	t.Run("stable", func(t *testing.T) {
		h1 := computeMessageHashes([]llm.Message{m1, m2, m3}, nil)
		h2 := computeMessageHashes([]llm.Message{m1, m2, m3}, nil)
		for i := range h1 {
			if h1[i] != h2[i] {
				t.Fatalf("hash mismatch at index %d: %d != %d", i, h1[i], h2[i])
			}
		}
	})

	t.Run("incremental_prefix", func(t *testing.T) {
		short := computeMessageHashes([]llm.Message{m1, m2}, nil)
		long := computeMessageHashes([]llm.Message{m1, m2, m3}, nil)
		for i := 0; i < len(short); i++ {
			if short[i] != long[i] {
				t.Fatalf("prefix hash mismatch at index %d: %d != %d", i, short[i], long[i])
			}
		}
	})

	t.Run("tool_call_parts", func(t *testing.T) {
		tc := llm.Message{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{{
				Type:     llm.PartToolCall,
				ToolCall: &llm.ToolCall{ID: "call_1", Name: "bash"},
			}},
		}
		tr := llm.Message{
			Role: llm.RoleTool,
			Parts: []llm.Part{{
				Type:       llm.PartToolResult,
				ToolResult: &llm.ToolResult{ID: "call_1"},
			}},
		}
		hashes := computeMessageHashes([]llm.Message{m1, tc, tr}, nil)
		if len(hashes) != 3 {
			t.Fatalf("expected 3 hashes, got %d", len(hashes))
		}
		// All hashes should be non-zero
		for i, h := range hashes {
			if h == 0 {
				t.Errorf("hash at index %d is zero", i)
			}
		}
	})
}

func TestPrefixCacheMatchAndStore(t *testing.T) {
	t.Run("store_and_match", func(t *testing.T) {
		cache := newPrefixHashCache()
		cache.Store(42, "sess-1", time.Minute)

		sid, count, ok := cache.Match([]uint64{10, 20, 42})
		if !ok {
			t.Fatal("expected match")
		}
		if sid != "sess-1" {
			t.Errorf("expected sess-1, got %s", sid)
		}
		if count != 3 {
			t.Errorf("expected matchedCount 3, got %d", count)
		}
	})

	t.Run("match_prefers_later_hashes", func(t *testing.T) {
		cache := newPrefixHashCache()
		cache.Store(10, "sess-early", time.Minute)
		cache.Store(42, "sess-late", time.Minute)

		sid, count, ok := cache.Match([]uint64{10, 20, 42})
		if !ok {
			t.Fatal("expected match")
		}
		if sid != "sess-late" {
			t.Errorf("expected sess-late, got %s", sid)
		}
		if count != 3 {
			t.Errorf("expected matchedCount 3, got %d", count)
		}
	})

	t.Run("expired_not_returned", func(t *testing.T) {
		cache := newPrefixHashCache()
		// Store with a TTL in the past (via direct entry manipulation)
		cache.mu.Lock()
		cache.entries[42] = prefixHashEntry{
			sessionID: "sess-expired",
			expires:   time.Now().Add(-time.Second),
		}
		cache.mu.Unlock()

		_, _, ok := cache.Match([]uint64{42})
		if ok {
			t.Fatal("expected no match for expired entry")
		}

		// Verify the expired entry was lazily deleted
		cache.mu.Lock()
		_, exists := cache.entries[42]
		cache.mu.Unlock()
		if exists {
			t.Error("expected expired entry to be deleted")
		}
	})

	t.Run("no_match", func(t *testing.T) {
		cache := newPrefixHashCache()
		_, _, ok := cache.Match([]uint64{99, 100, 101})
		if ok {
			t.Fatal("expected no match on empty cache")
		}
	})
}

func TestPrefixCacheEvictSession(t *testing.T) {
	cache := newPrefixHashCache()
	cache.Store(10, "sess-a", time.Minute)
	cache.Store(20, "sess-a", time.Minute)
	cache.Store(30, "sess-b", time.Minute)
	cache.Store(40, "sess-b", time.Minute)

	cache.EvictSession("sess-a")

	// sess-a entries should be gone
	_, _, ok := cache.Match([]uint64{10})
	if ok {
		t.Error("expected sess-a hash 10 to be evicted")
	}
	_, _, ok = cache.Match([]uint64{20})
	if ok {
		t.Error("expected sess-a hash 20 to be evicted")
	}

	// sess-b entries should remain
	sid, _, ok := cache.Match([]uint64{30})
	if !ok || sid != "sess-b" {
		t.Errorf("expected sess-b for hash 30, got ok=%v sid=%s", ok, sid)
	}
	sid, _, ok = cache.Match([]uint64{40})
	if !ok || sid != "sess-b" {
		t.Errorf("expected sess-b for hash 40, got ok=%v sid=%s", ok, sid)
	}
}

func TestPrefixCacheNoCollision(t *testing.T) {
	cases := []struct {
		name     string
		messages []llm.Message
	}{
		{
			name: "user_hello",
			messages: []llm.Message{
				{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}},
			},
		},
		{
			name: "user_goodbye",
			messages: []llm.Message{
				{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "goodbye"}}},
			},
		},
		{
			name: "assistant_hello",
			messages: []llm.Message{
				{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}},
			},
		},
		{
			name: "user_empty",
			messages: []llm.Message{
				{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: ""}}},
			},
		},
		{
			name: "tool_call",
			messages: []llm.Message{
				{Role: llm.RoleAssistant, Parts: []llm.Part{{
					Type:     llm.PartToolCall,
					ToolCall: &llm.ToolCall{ID: "tc1", Name: "bash"},
				}}},
			},
		},
		{
			name: "tool_call_different_id",
			messages: []llm.Message{
				{Role: llm.RoleAssistant, Parts: []llm.Part{{
					Type:     llm.PartToolCall,
					ToolCall: &llm.ToolCall{ID: "tc2", Name: "bash"},
				}}},
			},
		},
	}

	hashes := make(map[string]uint64)
	for _, tc := range cases {
		h := computeMessageHashes(tc.messages, nil)
		hashes[tc.name] = h[len(h)-1]
	}

	// Every pair of different test cases should produce a different hash
	names := make([]string, 0, len(hashes))
	for name := range hashes {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if hashes[names[i]] == hashes[names[j]] {
				t.Errorf("hash collision between %q and %q: %d",
					names[i], names[j], hashes[names[i]])
			}
		}
	}
}

func TestPrefixCacheCleanExpired(t *testing.T) {
	cache := newPrefixHashCache()
	cache.Store(10, "sess-alive", time.Minute)

	cache.mu.Lock()
	cache.entries[20] = prefixHashEntry{
		sessionID: "sess-dead",
		expires:   time.Now().Add(-time.Second),
	}
	cache.mu.Unlock()

	cache.CleanExpired()

	// Alive entry should remain
	sid, _, ok := cache.Match([]uint64{10})
	if !ok || sid != "sess-alive" {
		t.Error("expected sess-alive to survive cleanup")
	}

	// Dead entry should be gone
	cache.mu.Lock()
	_, exists := cache.entries[20]
	cache.mu.Unlock()
	if exists {
		t.Error("expected expired entry to be cleaned")
	}
}
