package authorship

import (
	"context"
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

type pendingAuthorStore struct {
	session.Store
	session.PendingSteeringStore
	entries []session.PendingSteering
	err     error
}

func (s pendingAuthorStore) ListPendingSteering(context.Context, string) ([]session.PendingSteering, error) {
	return s.entries, s.err
}

func TestRestorePendingAuthors(t *testing.T) {
	for _, tc := range []struct{ name, original, owner, want string }{
		{name: "different viewer preserves original", original: "bob", want: "bob"},
		{name: "anonymous original stays anonymous", want: ""},
		{name: "rush owns its own commit", original: "bob", owner: "rush", want: "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := pendingAuthorStore{entries: []session.PendingSteering{{ID: "pending", OwnerKind: tc.owner, Message: core.Message{Role: core.RoleUser, AuthorID: tc.original}}}}
			messages := []core.Message{{Role: core.RoleUser, ClientMessageID: "pending", AuthorID: "alice"}, {Role: core.RoleUser, ClientMessageID: "new", AuthorID: "alice"}}
			if err := RestorePendingAuthors(context.Background(), store, "session", messages); err != nil {
				t.Fatal(err)
			}
			if messages[0].AuthorID != tc.want || messages[1].AuthorID != "alice" {
				t.Fatalf("authors=%+v", messages)
			}
		})
	}
	sentinel := errors.New("read failed")
	if err := RestorePendingAuthors(context.Background(), pendingAuthorStore{err: sentinel}, "session", nil); !errors.Is(err, sentinel) {
		t.Fatalf("lost read failure: %v", err)
	}
	if err := RestorePendingAuthors(context.Background(), nil, "session", nil); err != nil {
		t.Fatal(err)
	}
}
