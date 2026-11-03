package authoridentity

import "sync"

// Selection is invocation-scoped, shared across replacement chat models. It is
// not session metadata: continuing another conversation never adopts its author.
type Selection struct {
	mu   sync.RWMutex
	name string
}

// Name returns the current user, or empty for anonymous/nil selections.
func (s *Selection) Name() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// Set changes only future turns. Empty explicitly clears the selection.
func (s *Selection) Set(name string) error {
	if name != "" {
		var err error
		name, err = ParseName(name)
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
	return nil
}
