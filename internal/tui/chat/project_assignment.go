package chat

import (
	"context"
	"sync"
	"time"

	projectpkg "github.com/samsaffron/term-llm/internal/project"
	"github.com/samsaffron/term-llm/internal/session"
)

var reconciledTUIProjects sync.Map

// persistNewTUISession keeps project grouping implicit: a session born inside a
// registered project is assigned before its first durable write. Project lookup
// is best-effort and never prevents a chat from starting.
func persistNewTUISession(ctx context.Context, store session.Store, sess *session.Session) {
	if store == nil || sess == nil {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	p, _ := projectpkg.AssignSessionForDir(lookupCtx, store, sess, sess.CWD)
	cancel()
	if err := store.Create(ctx, sess); err != nil {
		return
	}
	_ = store.SetCurrent(ctx, sess.ID)
	if p != nil {
		reconcileTUIProjectInBackground(store, *p)
	}
}

func reconcileTUIProjectInBackground(store session.Store, p session.Project) {
	if _, loaded := reconciledTUIProjects.LoadOrStore(p.ID, struct{}{}); loaded {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := projectpkg.ClaimMatchingSessions(ctx, store, p); err != nil {
			reconciledTUIProjects.Delete(p.ID)
		}
	}()
}
