package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/agents/gist"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/config"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
)

type shareRequest struct {
	forceNew bool
	public   bool
}

type gistSharer interface {
	Create(string, bool, map[string]string) (*gist.Gist, error)
	Update(string, map[string]string) error
}

var newGistClient = func() (gistSharer, error) { return gist.NewClient() }

type shareDoneMsg struct {
	store           session.Store
	sessionID       string
	priorSharedAt   time.Time
	gist            *gist.Gist
	preview         string
	updated         bool
	public          bool
	requestedPublic bool
	err             error
}

func (m *Model) cmdShare(args []string) (tea.Model, tea.Cmd) {
	req := shareRequest{}
	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "new":
			req.forceNew = true
		case "public":
			req.public = true
		default:
			return m.showFooterError("Usage: /share [new] [public]")
		}
	}
	if m.sess == nil || m.store == nil {
		return m.showFooterError("No saved session to share.")
	}
	if m.streaming {
		return m.showFooterError("Cannot share while streaming.")
	}
	if m.shareInFlight {
		return m.showFooterError("A share is already in progress.")
	}
	if m.sess.Share != nil && m.sess.Share.GistID != "" && !req.forceNew {
		m.pendingShare = &req
		m.dialog.ShowShareChoice()
		return m, nil
	}
	return m.startShare(req, false)
}

func (m *Model) startShare(req shareRequest, update bool) (tea.Model, tea.Cmd) {
	if m.shareInFlight {
		return m.showFooterError("A share is already in progress.")
	}
	if m.sess == nil || m.store == nil {
		return m.showFooterError("No saved session to share.")
	}

	sess := m.sess
	sessSnapshot := *sess
	store := m.store
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := config.DefaultReasoningConfig()
	if m.config != nil {
		cfg = m.config.ResolveReasoning("chat")
	}
	opts := session.ExportOptions{IncludeReasoningSummaries: internalreasoning.ExportSummaries(cfg)}
	if cfg.Raw && internalreasoning.SourceAllowsRaw(cfg) && strings.EqualFold(strings.TrimSpace(cfg.Export), config.ReasoningExportRaw) {
		opts.IncludeRawReasoning = true
		opts.IncludeReasoningSummaries = true
	}

	sessionID := sess.ID
	name := sess.Name
	if name == "" {
		name = fmt.Sprintf("#%d", sess.Number)
	}
	var updateID, updateURL string
	var updatePublic bool
	var priorSharedAt time.Time
	if update {
		if sess.Share == nil || sess.Share.GistID == "" {
			return m.showFooterError("No existing gist to update.")
		}
		updateID = sess.Share.GistID
		updateURL = sess.Share.GistURL
		updatePublic = sess.Share.Public
		priorSharedAt = sess.Share.SharedAt
		if session.GistPreviewURL(updateID) == "" {
			return m.showFooterError("The stored gist ID is invalid; create a new gist instead.")
		}
	}

	m.pendingShare = nil
	m.shareInFlight = true
	label := "Creating Gist…"
	if update {
		label = "Updating Gist…"
	}
	updatedModel, footerCmd := m.showFooterMuted(label)
	cmd := func() tea.Msg {
		result := shareDoneMsg{store: store, sessionID: sessionID, priorSharedAt: priorSharedAt, updated: update, public: updatePublic, requestedPublic: req.public}
		messages, _, err := session.LoadScrollbackWithBoundary(ctx, store, &sessSnapshot)
		if err != nil {
			result.err = fmt.Errorf("load session messages: %w", err)
			return result
		}
		files, err := session.GistFiles(&sessSnapshot, session.VisibleExportMessages(messages), opts)
		if err != nil {
			result.err = err
			return result
		}
		client, err := newGistClient()
		if err != nil {
			result.err = err
			return result
		}
		if update {
			if err := client.Update(updateID, files); err != nil {
				result.err = fmt.Errorf("update gist: %w", err)
				return result
			}
			result.gist = &gist.Gist{ID: updateID, URL: updateURL, Public: updatePublic}
			result.preview = session.GistPreviewURL(updateID)
			return result
		}
		g, err := client.Create("term-llm session: "+name, req.public, files)
		if err != nil {
			result.err = fmt.Errorf("create gist: %w", err)
			return result
		}
		preview := session.GistPreviewURL(g.ID)
		if preview == "" {
			result.err = fmt.Errorf("gist returned an invalid ID %q", g.ID)
			return result
		}
		result.gist = g
		result.preview = preview
		result.public = req.public
		return result
	}
	return updatedModel, tea.Batch(footerCmd, cmd)
}

func (m *Model) handleShareDone(msg shareDoneMsg) (tea.Model, tea.Cmd) {
	m.shareInFlight = false
	if msg.err != nil {
		return m.showFooterError("Share failed: " + msg.err.Error())
	}
	if msg.gist == nil || msg.sessionID == "" || msg.store == nil {
		return m.showFooterError("Share failed: empty gist result.")
	}
	now := time.Now()
	sharedAt := now
	if msg.updated && !msg.priorSharedAt.IsZero() {
		sharedAt = msg.priorSharedAt
	}
	state := &session.ShareState{GistID: msg.gist.ID, GistURL: msg.gist.URL, PreviewURL: msg.preview, Public: msg.public, SharedAt: sharedAt, UpdatedAt: now}
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := session.UpdateShare(ctx, msg.store, msg.sessionID, state); err != nil {
		return m.showFooterError("Gist shared, but saving share state failed: " + err.Error())
	}
	if m.sess != nil && m.sess.ID == msg.sessionID {
		m.sess.Share = state
	}
	sysErr := clipboard.CopyText(msg.preview)
	oscErr := clipboard.CopyTextOSC52(msg.preview)
	copied := sysErr == nil || oscErr == nil
	action := "Created new gist"
	if msg.updated {
		action = "Updated existing gist"
	}
	visibility := "Secret (unlisted, not private)"
	if msg.public {
		visibility = "Public"
	}
	content := fmt.Sprintf("%s\n\nPreview:\n%s\n\nGist/source:\n%s\n\nVisibility: %s", action, msg.preview, msg.gist.URL, visibility)
	if msg.updated && msg.requestedPublic && !msg.public {
		content += "\n\nVisibility unchanged: updating an existing gist cannot make it public. Create a new public gist instead."
	}
	if copied {
		content += "\n\nURL copied to clipboard."
	} else {
		content += "\n\nClipboard copy failed; copy the preview URL above."
	}
	m.clearFooterMessage()
	m.dialog.ShowContent("Session shared", content)
	return m, nil
}
