package authorship

import (
	"context"
	"log"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
)

// Initialize checks the directory once at startup. Tests/custom hosts may defer
// this until the first projection. Failed probes are retried, not cached empty.
func (d *Directory) Initialize(ctx context.Context, store session.Store) { d.hasAuthors(ctx, store) }

func (d *Directory) hasAuthors(ctx context.Context, store session.Store) bool {
	d.mu.Lock()
	known, checked := d.authorsKnown, d.presenceChecked
	d.mu.Unlock()
	if checked {
		return known
	}
	directory, ok := session.AsAuthorStore(store)
	if !ok {
		return false
	}
	exists, err := directory.HasAuthors(ctx)
	if err != nil {
		log.Printf("[serve] inspect authors: %v", err)
		return false
	}
	d.mu.Lock()
	d.authorsKnown = d.authorsKnown || exists
	d.presenceChecked = true
	known = d.authorsKnown
	d.mu.Unlock()
	return known
}

// recordExistsLocked keeps the existence cache separate from header names.
func (d *Directory) recordExistsLocked(id string) {
	d.authorsKnown = true
	d.presenceChecked = true
	if d.devices == nil {
		d.devices = map[string]bool{}
	}
	if len(d.devices) >= 512 {
		clear(d.devices)
	}
	d.devices[id] = true
}

func (d *Directory) Stamp(ctx context.Context, store session.Store, msg *core.Message) {
	if msg.Role != core.RoleUser {
		msg.AuthorID = ""
		return
	}
	identity := FromContext(ctx)
	if identity == nil {
		return
	}
	a := identity.Author
	if directory, ok := session.AsAuthorStore(store); ok {
		d.stampAuthor(ctx, directory, a, identity.LinkedDevice)
	}
	msg.AuthorID = a.ID
}

func (d *Directory) stampAuthor(ctx context.Context, directory session.AuthorStore, a session.Author, device bool) {
	d.mu.Lock()
	if device && d.devices[a.ID] {
		d.mu.Unlock()
		return
	}
	if !device {
		if previous, ok := d.stamped[a.ID]; ok && previous == a {
			d.mu.Unlock()
			return
		}
	}
	// Overlapping header writes may commit in either order. Do not retain an
	// unchanged-values shortcut for that ID until a later exclusive write.
	var flight *headerWrite
	if !device {
		if d.writes == nil {
			d.writes = map[string]*headerWrite{}
		}
		flight = d.writes[a.ID]
		if flight == nil {
			flight = &headerWrite{}
			d.writes[a.ID] = flight
		}
		flight.active++
		if flight.active > 1 {
			flight.overlapping = true
			delete(d.stamped, a.ID)
		}
	}
	d.mu.Unlock()
	err := directory.UpsertAuthor(ctx, a, device)
	d.mu.Lock()
	if err == nil {
		d.recordExistsLocked(a.ID)
		if !device && !flight.overlapping {
			if d.stamped == nil {
				d.stamped = map[string]session.Author{}
			}
			if len(d.stamped) >= 512 {
				clear(d.stamped)
			}
			d.stamped[a.ID] = a
		}
	}
	if flight != nil {
		flight.active--
		if flight.active == 0 {
			delete(d.writes, a.ID)
		}
	}
	d.mu.Unlock()
	if err != nil {
		log.Printf("[serve] persist author: %v", err)
	}
}
