package authorship

import (
	"context"
	"log"
	"net/http"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

// Directory caches successful identity writes; reads refresh names in one batch.
// It belongs to one server/store and never modifies message content.
type Directory struct {
	mu              sync.Mutex
	stamped         map[string]session.Author // successful header upserts, never pairing snapshots
	devices         map[string]bool           // rows known to exist; device snapshots cannot shadow names
	writes          map[string]*headerWrite
	presenceChecked bool
	authorsKnown    bool
}

type headerWrite struct {
	active      int
	overlapping bool
}

// Lookup returns stored metadata with an ID fallback for a missing directory row.
func (d *Directory) Lookup(ctx context.Context, store session.Store, ids []string) map[string]session.Author {
	out := map[string]session.Author{}
	d.mu.Lock()
	for _, id := range ids {
		if id != "" {
			if cached, ok := d.stamped[id]; ok {
				out[id] = cached
			} else {
				out[id] = session.Author{ID: id, Name: id}
			}
		}
	}
	d.mu.Unlock()
	if directory, ok := session.AsAuthorStore(store); ok {
		if stored, err := directory.GetAuthors(ctx, ids); err == nil {
			d.mu.Lock()
			for id, a := range stored {
				out[id] = a
				d.recordExistsLocked(id)
			}
			d.mu.Unlock()
		} else {
			log.Printf("[serve] read authors: %v", err)
		}
	}
	return out
}

// SessionFields is the shared wire shape for list, search and project rows.
type SessionFields struct {
	CreatedBy        *session.Author  `json:"created_by,omitempty"`
	Participants     []session.Author `json:"participants,omitempty"`
	ParticipantCount *int             `json:"participant_count,omitempty"`
}

// Sessions resolves one batched participants query and one directory read per page.
func (d *Directory) Sessions(ctx context.Context, store session.Store, starters map[string]string) map[string]SessionFields {
	out := map[string]SessionFields{}
	ids := make([]string, 0, len(starters))
	authorIDs := []string{}
	for id, starter := range starters {
		ids = append(ids, id)
		authorIDs = append(authorIDs, starter)
	}
	participants := map[string]session.Participants{}
	participantsKnown := false
	anyAuthors := d.hasAuthors(ctx, store)
	if directory, ok := session.AsAuthorStore(store); ok {
		var err error
		participants, err = directory.SessionParticipants(ctx, ids)
		participantsKnown = err == nil
		if err != nil {
			log.Printf("[serve] read participants: %v", err)
		}
	}
	for _, p := range participants {
		authorIDs = append(authorIDs, p.IDs...)
	}
	authors := d.Lookup(ctx, store, authorIDs)
	for id, starter := range starters {
		fields := SessionFields{}
		if a, ok := authors[starter]; ok {
			fields.CreatedBy = &a
		}
		p := participants[id]
		if participantsKnown && (anyAuthors || p.Count > 0) {
			count := p.Count
			fields.ParticipantCount = &count
		}
		for _, aid := range p.IDs {
			fields.Participants = append(fields.Participants, authors[aid])
		}
		out[id] = fields
	}
	return out
}

// ViewerID is empty for anonymous contexts.
func ViewerID(ctx context.Context) string {
	if i := FromContext(ctx); i != nil {
		return i.Author.ID
	}
	return ""
}

func (d *Directory) StampInputs(r *http.Request, store session.Store, messages []core.Message) {
	for i := range messages {
		if messages[i].Role == core.RoleUser && messages[i].ClientMessageID != "" {
			d.Stamp(r.Context(), store, &messages[i])
		}
	}
}
