package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/worktree"
)

// rootCheckoutLeaseRegistry coordinates agent runs with operations that mutate
// a repository's main checkout. Runs in linked worktrees use their own checkout
// and intentionally do not participate.
type rootCheckoutLeaseRegistry struct {
	mu     sync.Mutex
	leases map[string]*rootCheckoutLease
}

type rootCheckoutLease struct {
	mu      sync.Mutex
	readers int
	writer  bool
	changed chan struct{}
}

var processRootCheckoutLeases rootCheckoutLeaseRegistry

func (r *rootCheckoutLeaseRegistry) acquireRun(ctx context.Context, dir string) (func(), error) {
	dir, err := canonicalRootLeasePath(dir)
	if err != nil {
		return func() {}, nil
	}

	mainRoot, gitErr := worktree.MainRepoRoot(dir)
	checkoutRoot, checkoutErr := worktree.CheckoutRoot(dir)
	if gitErr == nil && checkoutErr == nil {
		mainRoot, err = canonicalRootLeasePath(mainRoot)
		if err == nil {
			checkoutRoot, err = canonicalRootLeasePath(checkoutRoot)
		}
		if err == nil {
			if checkoutRoot != mainRoot {
				// The directory belongs to a linked worktree, not the main checkout.
				return func() {}, nil
			}
			return r.lease(mainRoot).acquireRead(ctx)
		}
	}

	// Mutation admission registers its root before changing HEAD or the index.
	// If Git inspection fails while that mutation is in progress, fall back to a
	// registered containing root so the run cannot bypass the admitted writer.
	if lease := r.knownRootLease(dir); lease != nil {
		return lease.acquireRead(ctx)
	}

	// Non-Git working directories have no checkout to coordinate. Canonical path
	// failures are likewise non-actionable here because there is no root key.
	return func() {}, nil
}

func (r *rootCheckoutLeaseRegistry) tryAcquireMutation(root string) (func(), bool, error) {
	mainRoot, err := worktree.MainRepoRoot(root)
	if err != nil {
		return nil, false, err
	}
	mainRoot, err = canonicalRootLeasePath(mainRoot)
	if err != nil {
		return nil, false, err
	}
	release, ok := r.lease(mainRoot).tryAcquireWrite()
	return release, ok, nil
}

func (r *rootCheckoutLeaseRegistry) knownRootLease(dir string) *rootCheckoutLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	var match string
	for root := range r.leases {
		if pathWithinRoot(dir, root) && len(root) > len(match) {
			match = root
		}
	}
	return r.leases[match]
}

func (r *rootCheckoutLeaseRegistry) lease(root string) *rootCheckoutLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.leases == nil {
		r.leases = make(map[string]*rootCheckoutLease)
	}
	lease := r.leases[root]
	if lease == nil {
		lease = &rootCheckoutLease{changed: make(chan struct{})}
		r.leases[root] = lease
	}
	return lease
}

func (l *rootCheckoutLease) acquireRead(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		l.mu.Lock()
		if !l.writer {
			l.readers++
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					l.readers--
					l.mu.Unlock()
				})
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (l *rootCheckoutLease) tryAcquireWrite() (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writer || l.readers > 0 {
		return nil, false
	}
	l.writer = true
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.writer = false
			close(l.changed)
			l.changed = make(chan struct{})
			l.mu.Unlock()
		})
	}, true
}

func canonicalRootLeasePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func pathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
