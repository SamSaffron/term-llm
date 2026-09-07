package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// validateRequestSessionID prevents API clients from creating identities that
// cannot be addressed as a single URL path segment. Existing sessions retain
// their identity and can be addressed by their durable session number instead.
func (s *serveServer) validateRequestSessionID(ctx context.Context, id string) error {
	if id != "." && id != ".." && !strings.ContainsAny(id, `/\`) && strings.IndexFunc(id, unicode.IsControl) < 0 {
		return nil
	}
	if s.store != nil {
		existing, err := s.store.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("look up session ID: %w", err)
		}
		if existing != nil {
			return nil
		}
	}
	return fmt.Errorf("new session ID must not contain path separators or control characters, or be . or ..")
}

// resolveSessionPathID is shared by both session API namespaces so numeric
// routes address the same persisted identity, including legacy IDs with slashes.
func (s *serveServer) resolveSessionPathID(ctx context.Context, id string) (string, error) {
	if number, err := strconv.ParseInt(id, 10, 64); err == nil && number > 0 && s.store != nil {
		sess, err := s.store.GetByNumber(ctx, number)
		if err != nil {
			return "", fmt.Errorf("resolve session number: %w", err)
		}
		if sess == nil {
			return "", fmt.Errorf("session number %d not found", number)
		}
		return sess.ID, nil
	}
	return id, nil
}
