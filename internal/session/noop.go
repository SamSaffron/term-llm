package session

import "context"

// NoopStore is a no-op implementation of Store used when sessions are disabled.
// It silently discards all writes and returns empty results for reads.
type NoopStore struct{}

func (s *NoopStore) Create(ctx context.Context, sess *Session) error {
	if sess.ID == "" {
		sess.ID = NewID()
	}
	return nil
}

func (s *NoopStore) Get(ctx context.Context, id string) (*Session, error) {
	return nil, nil
}

func (s *NoopStore) Update(ctx context.Context, sess *Session) error {
	return nil
}

func (s *NoopStore) Delete(ctx context.Context, id string) error {
	return nil
}

func (s *NoopStore) List(ctx context.Context, opts ListOptions) ([]SessionSummary, error) {
	return nil, nil
}

func (s *NoopStore) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return nil, nil
}

func (s *NoopStore) AddMessage(ctx context.Context, sessionID string, msg *Message) error {
	return nil
}

func (s *NoopStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	return nil, nil
}

func (s *NoopStore) SetCurrent(ctx context.Context, sessionID string) error {
	return nil
}

func (s *NoopStore) GetCurrent(ctx context.Context) (*Session, error) {
	return nil, nil
}

func (s *NoopStore) ClearCurrent(ctx context.Context) error {
	return nil
}

func (s *NoopStore) Close() error {
	return nil
}
