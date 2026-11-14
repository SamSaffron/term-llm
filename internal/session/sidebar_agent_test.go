package session

import (
	"context"
	"testing"
	"time"
)

// The web client merges sidebar rows over the selected session, so a sidebar
// row without the agent would blank the conversation's agent on every refresh.
func TestSidebarSummariesCarryAgent(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	at := time.Now()
	sess := &Session{ID: "dev", Provider: "mock", ProviderKey: "mock", Model: "mock-model", Mode: ModeChat, Origin: OriginWeb, Agent: "developer", CreatedAt: at, UpdatedAt: at, Status: StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	groups, err := store.Sidebar(ctx, SidebarOptions{PerProject: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		for _, summary := range group.Sessions {
			if summary.ID == "dev" {
				if summary.Agent != "developer" {
					t.Fatalf("sidebar agent = %q, want developer", summary.Agent)
				}
				return
			}
		}
	}
	t.Fatalf("session dev missing from sidebar groups %#v", groups)
}
