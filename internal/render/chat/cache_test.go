package chat

import (
	"testing"
)

func testBlockCacheKey(id int64) blockCacheKey {
	return blockCacheKey{
		messageID:      id,
		width:          80,
		partsSignature: uint64(id),
	}
}

func TestBlockCache_PutAndGet(t *testing.T) {
	cache := NewBlockCache(3)

	// Add some blocks
	block1 := &MessageBlock{MessageID: 1, Rendered: "block1", Height: 5}
	block2 := &MessageBlock{MessageID: 2, Rendered: "block2", Height: 3}

	key1 := testBlockCacheKey(1)
	key2 := testBlockCacheKey(2)
	key3 := testBlockCacheKey(3)
	cache.Put(key1, block1)
	cache.Put(key2, block2)

	// Verify retrieval
	got1 := cache.Get(key1)
	if got1 == nil || got1.MessageID != 1 {
		t.Errorf("Get(key1) = %v, want block1", got1)
	}

	got2 := cache.Get(key2)
	if got2 == nil || got2.MessageID != 2 {
		t.Errorf("Get(key2) = %v, want block2", got2)
	}

	// Non-existent key
	got3 := cache.Get(key3)
	if got3 != nil {
		t.Errorf("Get(key3) = %v, want nil", got3)
	}
}

func TestBlockCache_LRUEviction(t *testing.T) {
	cache := NewBlockCache(3)

	keyA := testBlockCacheKey(1)
	keyB := testBlockCacheKey(2)
	keyC := testBlockCacheKey(3)
	keyD := testBlockCacheKey(4)

	// Fill the cache
	cache.Put(keyA, &MessageBlock{MessageID: 1})
	cache.Put(keyB, &MessageBlock{MessageID: 2})
	cache.Put(keyC, &MessageBlock{MessageID: 3})

	// Access "a" to make it recently used
	cache.Get(keyA)

	// Add a new item, should evict "b" (least recently used)
	cache.Put(keyD, &MessageBlock{MessageID: 4})

	// "a" and "c" should still be there, "b" should be evicted
	if cache.Get(keyA) == nil {
		t.Error("'a' should not have been evicted")
	}
	if cache.Get(keyC) == nil {
		t.Error("'c' should not have been evicted")
	}
	if cache.Get(keyD) == nil {
		t.Error("'d' should be in cache")
	}
	if cache.Get(keyB) != nil {
		t.Error("'b' should have been evicted")
	}
}

func TestBlockCache_Update(t *testing.T) {
	cache := NewBlockCache(3)

	key := testBlockCacheKey(1)
	block1 := &MessageBlock{MessageID: 1, Rendered: "original"}
	block2 := &MessageBlock{MessageID: 1, Rendered: "updated"}

	cache.Put(key, block1)
	cache.Put(key, block2)

	got := cache.Get(key)
	if got == nil || got.Rendered != "updated" {
		t.Errorf("Get(key).Rendered = %v, want 'updated'", got)
	}

	// Size should still be 1
	if cache.Size() != 1 {
		t.Errorf("Size() = %d, want 1", cache.Size())
	}
}

func TestBlockCache_Remove(t *testing.T) {
	cache := NewBlockCache(3)

	key := testBlockCacheKey(1)
	cache.Put(key, &MessageBlock{MessageID: 1})
	cache.Remove(key)

	if cache.Get(key) != nil {
		t.Error("Get(key) after Remove should return nil")
	}
	if cache.Size() != 0 {
		t.Errorf("Size() after Remove = %d, want 0", cache.Size())
	}
}

func TestBlockCache_EnsureCapacityGrowsOnly(t *testing.T) {
	cache := NewBlockCache(3)
	cache.EnsureCapacity(5)
	if cache.MaxSize() != 5 {
		t.Fatalf("MaxSize() after grow = %d, want 5", cache.MaxSize())
	}

	cache.EnsureCapacity(2)
	if cache.MaxSize() != 5 {
		t.Fatalf("MaxSize() after smaller ensure = %d, want 5", cache.MaxSize())
	}

	cache.EnsureCapacity(maxBlockCacheSize + 100)
	if cache.MaxSize() != maxBlockCacheSize {
		t.Fatalf("MaxSize() after capped grow = %d, want %d", cache.MaxSize(), maxBlockCacheSize)
	}
}

func TestBlockCache_InvalidateAll(t *testing.T) {
	cache := NewBlockCache(10)

	for i := 0; i < 5; i++ {
		cache.Put(testBlockCacheKey(int64(i)), &MessageBlock{MessageID: int64(i)})
	}

	if cache.Size() != 5 {
		t.Errorf("Size() before invalidate = %d, want 5", cache.Size())
	}

	cache.InvalidateAll()

	if cache.Size() != 0 {
		t.Errorf("Size() after InvalidateAll = %d, want 0", cache.Size())
	}

	// Verify all keys are gone
	for i := 0; i < 5; i++ {
		if cache.Get(testBlockCacheKey(int64(i))) != nil {
			t.Errorf("Key %d should be nil after InvalidateAll", i)
		}
	}
}

func TestBlockCache_ConcurrentAccess(t *testing.T) {
	cache := NewBlockCache(100)
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 1000; i++ {
			cache.Put(testBlockCacheKey(int64(i%100)), &MessageBlock{MessageID: int64(i)})
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 1000; i++ {
			cache.Get(testBlockCacheKey(int64(i % 100)))
		}
		done <- true
	}()

	// Wait for both
	<-done
	<-done

	// If we got here without deadlock/panic, the test passes
}
