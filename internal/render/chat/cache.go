package chat

import (
	"container/list"
	"sync"
)

const maxBlockCacheSize = 2000

// blockCacheKey identifies a cached render without allocating per-frame key
// strings. It is intentionally fixed-width/comparable so map lookups in the
// hot View() path don't need strconv or string concatenation for every message.
type blockCacheKey struct {
	messageID      int64
	width          int
	toolsExpanded  bool
	partsSignature uint64
}

// BlockCache is an LRU cache for rendered MessageBlocks.
// It keeps memory bounded while avoiding re-rendering unchanged messages.
type BlockCache struct {
	mu      sync.RWMutex
	maxSize int
	cache   map[blockCacheKey]*list.Element
	lruList *list.List
}

// cacheEntry holds a cache key-value pair for the LRU list.
type cacheEntry struct {
	key   blockCacheKey
	block *MessageBlock
}

// NewBlockCache creates a new block cache with the given maximum size.
func NewBlockCache(maxSize int) *BlockCache {
	if maxSize <= 0 {
		maxSize = 100
	}
	if maxSize > maxBlockCacheSize {
		maxSize = maxBlockCacheSize
	}
	return &BlockCache{
		maxSize: maxSize,
		cache:   make(map[blockCacheKey]*list.Element),
		lruList: list.New(),
	}
}

// Get retrieves a block from the cache, returning nil if not found.
// Accessing a block moves it to the front of the LRU list.
func (c *BlockCache) Get(key blockCacheKey) *MessageBlock {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.cache[key]; ok {
		// Move to front (most recently used)
		c.lruList.MoveToFront(elem)
		return elem.Value.(*cacheEntry).block
	}
	return nil
}

// Put adds a block to the cache, evicting the least recently used
// block if the cache is at capacity.
func (c *BlockCache) Put(key blockCacheKey, block *MessageBlock) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if key already exists
	if elem, ok := c.cache[key]; ok {
		// Update existing entry and move to front
		c.lruList.MoveToFront(elem)
		elem.Value.(*cacheEntry).block = block
		return
	}

	// Evict oldest if at capacity
	if c.lruList.Len() >= c.maxSize {
		c.evictOldest()
	}

	// Add new entry at front
	entry := &cacheEntry{key: key, block: block}
	elem := c.lruList.PushFront(entry)
	c.cache[key] = elem
}

// evictOldest removes the least recently used entry.
// Must be called with lock held.
func (c *BlockCache) evictOldest() {
	oldest := c.lruList.Back()
	if oldest != nil {
		entry := oldest.Value.(*cacheEntry)
		delete(c.cache, entry.key)
		c.lruList.Remove(oldest)
	}
}

// Remove removes a specific key from the cache.
func (c *BlockCache) Remove(key blockCacheKey) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.cache[key]; ok {
		delete(c.cache, key)
		c.lruList.Remove(elem)
	}
}

// EnsureCapacity grows the cache capacity, capped by maxBlockCacheSize.
// It never shrinks the cache: renders can move between small and large histories,
// and shrinking would evict warm blocks only to re-grow on the next full-history view.
func (c *BlockCache) EnsureCapacity(size int) {
	if size <= 0 {
		return
	}
	if size > maxBlockCacheSize {
		size = maxBlockCacheSize
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.maxSize {
		c.maxSize = size
	}
}

// MaxSize returns the configured maximum number of cached blocks.
func (c *BlockCache) MaxSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.maxSize
}

// InvalidateAll clears the entire cache.
// Call this on terminal resize when all cached renders are invalid.
func (c *BlockCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache = make(map[blockCacheKey]*list.Element)
	c.lruList.Init()
}

// Size returns the current number of cached blocks.
func (c *BlockCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// Clear removes all entries from the cache.
// Alias for InvalidateAll for semantic clarity.
func (c *BlockCache) Clear() {
	c.InvalidateAll()
}
