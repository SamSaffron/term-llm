package ui

import (
	"fmt"
	"os"
	"sync"

	"github.com/samsaffron/term-llm/internal/image"
)

// maxImageCacheSize is the maximum number of entries in the image cache
const maxImageCacheSize = 100

// imageCacheEntry stores a rendered image with its modification time for cache invalidation
type imageCacheEntry struct {
	modTime int64  // file modification time in nanoseconds
	content string // rendered escape sequence
}

// renderedImages caches rendered image output to avoid re-encoding
var (
	renderedImages      = make(map[string]imageCacheEntry)
	renderedImagesOrder []string // tracks insertion order for FIFO eviction
	renderedImagesMu    sync.Mutex
)

// RenderInlineImage renders an image as terminal escape sequences for inline display.
// The rendered output is cached, so subsequent calls return the cached result.
// Returns empty string on error or if terminal doesn't support images.
// The cache is limited to maxImageCacheSize entries; oldest entries are evicted when full.
// The cache is invalidated when the file's modification time changes.
func RenderInlineImage(path string) string {
	if path == "" {
		return ""
	}

	// Get file modification time for cache validation
	modTime, err := getFileModTime(path)
	if err != nil {
		// File doesn't exist or can't be read - return empty without caching
		return ""
	}

	// Check cache first - validate that the file hasn't been modified
	renderedImagesMu.Lock()
	if cached, ok := renderedImages[path]; ok && cached.modTime == modTime {
		renderedImagesMu.Unlock()
		return cached.content
	}
	renderedImagesMu.Unlock()

	// Render the image
	result, err := image.RenderImageToString(path)
	if err != nil || result.Full == "" {
		// Don't cache failures - they may be transient
		return ""
	}

	// Cache the rendered output with modification time
	renderedImagesMu.Lock()
	addToImageCache(path, imageCacheEntry{modTime: modTime, content: result.Full})
	renderedImagesMu.Unlock()

	return result.Full
}

// getFileModTime returns the modification time of a file in nanoseconds.
func getFileModTime(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("failed to stat file: %w", err)
	}
	return info.ModTime().UnixNano(), nil
}

// addToImageCache adds an entry to the cache, evicting oldest if at capacity.
// Must be called with renderedImagesMu held.
func addToImageCache(path string, entry imageCacheEntry) {
	// If already in cache, just update the entry (don't change order)
	if _, exists := renderedImages[path]; exists {
		renderedImages[path] = entry
		return
	}

	// FIFO eviction: remove oldest entries if at capacity
	for len(renderedImages) >= maxImageCacheSize && len(renderedImagesOrder) > 0 {
		oldest := renderedImagesOrder[0]
		renderedImagesOrder = renderedImagesOrder[1:]
		delete(renderedImages, oldest)
	}

	// Add new entry
	renderedImages[path] = entry
	renderedImagesOrder = append(renderedImagesOrder, path)
}

// ClearRenderedImages clears the cache of rendered images.
// Call this when starting a new session.
func ClearRenderedImages() {
	renderedImagesMu.Lock()
	renderedImages = make(map[string]imageCacheEntry)
	renderedImagesOrder = nil
	renderedImagesMu.Unlock()
}
