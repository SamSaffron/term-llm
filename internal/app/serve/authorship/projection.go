package authorship

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

func (d *Directory) MessageAuthors(store session.Store, messages []session.Message) map[string]*session.Author {
	ids := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.Role == core.RoleUser {
			ids = append(ids, m.AuthorID)
		}
	}
	out := map[string]*session.Author{}
	for id, a := range d.Lookup(context.Background(), store, ids) {
		author := session.Author(a)
		out[id] = &author
	}
	return out
}

func StoredAuthor(store session.Store, id string) *session.Author {
	if id == "" {
		return nil
	}
	var directory Directory
	a := session.Author(directory.Lookup(context.Background(), store, []string{id})[id])
	return &a
}

func AddSteeringAuthor(store session.Store, payload map[string]any, message *core.Message) {
	if message != nil && message.Role == core.RoleUser {
		if a := StoredAuthor(store, message.AuthorID); a != nil {
			payload["author"] = a
		}
	}
}

func SteeringReceipt(lookup func(string) *session.Author, entry engine.QueuedSteering, text string) map[string]any {
	payload := map[string]any{"client_message_id": entry.ID, "text": text}
	if lookup != nil {
		if a := lookup(entry.Message.AuthorID); a != nil {
			payload["author"] = a
		}
	}
	return payload
}

// Project groups keep their existing summary wire contract; only attribution is added.
type SidebarSummary struct {
	session.SessionSummary
	CreatedBy        *session.Author  `json:"created_by,omitempty"`
	Participants     []session.Author `json:"participants,omitempty"`
	ParticipantCount *int             `json:"participant_count,omitempty"`
}
type SidebarGroup struct {
	session.SidebarGroup
	Sessions []SidebarSummary `json:"sessions"`
}

func (d *Directory) Sidebar(ctx context.Context, store session.Store, groups []session.SidebarGroup, starters map[string]string) ([]SidebarGroup, map[string]SessionFields) {
	for _, g := range groups {
		for _, row := range g.Sessions {
			starters[row.ID] = row.CreatedBy
		}
	}
	fields := d.Sessions(ctx, store, starters)
	out := make([]SidebarGroup, 0, len(groups))
	for _, g := range groups {
		group := SidebarGroup{SidebarGroup: g, Sessions: []SidebarSummary{}}
		for _, row := range g.Sessions {
			a := fields[row.ID]
			group.Sessions = append(group.Sessions, SidebarSummary{SessionSummary: row, CreatedBy: a.CreatedBy, Participants: a.Participants, ParticipantCount: a.ParticipantCount})
		}
		out = append(out, group)
	}
	return out, fields
}
