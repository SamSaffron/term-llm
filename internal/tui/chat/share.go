package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/config"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
	sharepkg "github.com/samsaffron/term-llm/internal/share"
)

type shareRequest struct {
	forceNew      bool
	includeRaw    bool
	visibility    sharepkg.Visibility
	visibilitySet bool
	publisher     sharepkg.Publisher
	capabilities  sharepkg.Capabilities
}

var newSharePublisher = func(cfg config.ShareConfig) (sharepkg.Publisher, error) {
	return sharepkg.NewPublisher(cfg)
}

type shareCapabilitiesMsg struct {
	request      shareRequest
	publisher    sharepkg.Publisher
	capabilities sharepkg.Capabilities
	err          error
}

type shareDoneMsg struct {
	store               session.Store
	sessionID           string
	priorSharedAt       time.Time
	result              sharepkg.Result
	updated             bool
	requestedVisibility sharepkg.Visibility
	providerName        string
	providerHelp        string
	providerNotes       []string
	includedRaw         bool
	copyAttempted       bool
	copyMethod          clipboard.CopyMethod
	copyErr             error
	err                 error
}

func (m *Model) cmdShare(args []string) (tea.Model, tea.Cmd) {
	const usage = "Usage: /share [new] [raw] [public|unlisted|private]"
	req := shareRequest{}
	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "new":
			if req.forceNew {
				return m.showFooterError(usage)
			}
			req.forceNew = true
		case "raw":
			if req.includeRaw {
				return m.showFooterError(usage)
			}
			req.includeRaw = true
		case "public", "unlisted", "private":
			if req.visibilitySet {
				return m.showFooterError(usage)
			}
			req.visibility = sharepkg.Visibility(strings.ToLower(arg))
			req.visibilitySet = true
		default:
			return m.showFooterError(usage)
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

	cfg := config.ShareConfig{}
	if m.config != nil {
		cfg = m.config.Share
	}
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	m.shareInFlight = true
	updatedModel, _ := m.showFooterPersistent("Loading sharing provider…", "muted")
	capabilityCmd := func() tea.Msg {
		publisher, err := newSharePublisher(cfg)
		if err != nil {
			return shareCapabilitiesMsg{request: req, err: err}
		}
		capabilities, err := publisher.Capabilities(ctx)
		return shareCapabilitiesMsg{request: req, publisher: publisher, capabilities: capabilities, err: err}
	}
	return updatedModel, tea.Batch(m.spinner.Tick, capabilityCmd)
}

func (m *Model) handleShareCapabilities(msg shareCapabilitiesMsg) (tea.Model, tea.Cmd) {
	m.shareInFlight = false
	if msg.err != nil {
		return m.showFooterError("Share unavailable: " + msg.err.Error())
	}
	if err := sharepkg.ValidateCapabilities(msg.capabilities); err != nil {
		return m.showFooterError("Share unavailable: provider returned invalid capabilities.")
	}
	req := msg.request
	req.publisher = msg.publisher
	req.capabilities = msg.capabilities
	if !req.visibilitySet {
		req.visibility = msg.capabilities.DefaultVisibility
	}
	if !msg.capabilities.SupportsVisibility(req.visibility) {
		return m.showFooterError(fmt.Sprintf("%s visibility is not supported by %s.", req.visibility, msg.capabilities.Provider.Name))
	}

	compatibleUpdate := false
	orphanWarning := false
	if m.sess != nil && m.sess.Share != nil && m.sess.Share.Exists() {
		m.sess.Share.Normalize()
		_, canUpdate := msg.publisher.(sharepkg.Updater)
		compatibleUpdate = !req.forceNew && canUpdate && m.sess.Share.Provider == string(msg.capabilities.Provider.ID) &&
			m.sess.Share.Scope == session.ShareScopeSession && msg.capabilities.Supports(sharepkg.OperationUpdate)
		orphanWarning = !compatibleUpdate
	}
	m.pendingShare = &req
	m.dialog.ShowShareChoice(compatibleUpdate, shareProviderGuidance(req, orphanWarning))
	m.clearFooterMessage()
	return m, nil
}

func shareProviderGuidance(req shareRequest, orphanWarning bool) string {
	parts := []string{fmt.Sprintf("Provider: %s. Visibility: %s.", req.capabilities.Provider.Name, req.visibility)}
	if req.visibility == sharepkg.VisibilityUnlisted {
		parts = append(parts, "Unlisted links are not private; anyone with the link may be able to view the transcript.")
	}
	parts = append(parts, req.capabilities.Notes...)
	if help := strings.TrimSpace(req.capabilities.Provider.Help); help != "" {
		parts = append(parts, "Help: "+help)
	}
	if req.includeRaw {
		parts = append(parts, "WARNING: raw model reasoning was explicitly requested and may contain private or sensitive information.")
	}
	if orphanWarning {
		parts = append(parts, "WARNING: creating this share replaces the saved share state; the previous provider link may remain active and must be managed separately.")
	}
	return strings.Join(parts, " ")
}

func (m *Model) startShare(req shareRequest, update bool) (tea.Model, tea.Cmd) {
	if m.shareInFlight {
		return m.showFooterError("A share is already in progress.")
	}
	if m.sess == nil || m.store == nil {
		return m.showFooterError("No saved session to share.")
	}
	if req.publisher == nil {
		return m.showFooterError("Sharing provider is unavailable.")
	}

	sess := m.sess
	sessSnapshot := *sess
	store := m.store
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	reasoningCfg := config.DefaultReasoningConfig()
	if m.config != nil {
		reasoningCfg = m.config.ResolveReasoning("chat")
	}
	opts := session.ExportOptions{IncludeReasoningSummaries: internalreasoning.ExportSummaries(reasoningCfg)}
	if req.includeRaw {
		switch {
		case !reasoningCfg.Raw:
			return m.showFooterError("Raw reasoning sharing is disabled; enable reasoning.raw before using /share raw.")
		case !internalreasoning.SourceAllowsRaw(reasoningCfg):
			return m.showFooterError("Raw reasoning sharing is blocked by reasoning.source; set it to all before using /share raw.")
		default:
			opts.IncludeRawReasoning = true
			opts.IncludeReasoningSummaries = true
		}
	}

	sessionID := sess.ID
	name := strings.TrimSpace(sess.PreferredShortTitle())
	if name == "" {
		name = fmt.Sprintf("#%d", sess.Number)
	}
	requestedVisibility := sharepkg.Visibility("")
	if req.visibilitySet {
		requestedVisibility = req.visibility
	}
	var updateID string
	var updater sharepkg.Updater
	var priorSharedAt time.Time
	if update {
		var ok bool
		updater, ok = req.publisher.(sharepkg.Updater)
		if !ok {
			return m.showFooterError("The sharing provider does not support updates; create a new share instead.")
		}
		if sess.Share == nil || !sess.Share.Exists() {
			return m.showFooterError("No existing share to update.")
		}
		sess.Share.Normalize()
		if sess.Share.Provider != string(req.capabilities.Provider.ID) || sess.Share.Scope != session.ShareScopeSession || !req.capabilities.Supports(sharepkg.OperationUpdate) {
			return m.showFooterError("The existing share is not compatible with this provider; create a new share instead.")
		}
		updateID = sess.Share.ID
		priorSharedAt = sess.Share.SharedAt
	}

	m.pendingShare = nil
	m.shareInFlight = true
	label := "Creating share…"
	if update {
		label = "Updating share…"
	}
	updatedModel, _ := m.showFooterPersistent(label, "muted")
	cmd := func() tea.Msg {
		result := shareDoneMsg{
			store: store, sessionID: sessionID, priorSharedAt: priorSharedAt, updated: update,
			requestedVisibility: requestedVisibility, providerName: req.capabilities.Provider.Name,
			providerHelp: req.capabilities.Provider.Help, providerNotes: append([]string(nil), req.capabilities.Notes...),
			includedRaw: opts.IncludeRawReasoning,
		}
		messages, _, err := session.LoadScrollbackWithBoundary(ctx, store, &sessSnapshot)
		if err != nil {
			result.err = fmt.Errorf("load session messages: %w", err)
			return result
		}
		files, err := session.ShareFiles(&sessSnapshot, session.VisibleExportMessages(messages), opts)
		if err != nil {
			result.err = err
			return result
		}
		request := sharepkg.Request{
			RequestID: sharepkg.NewRequestID(), Title: name,
			Description: "term-llm session: " + name, Visibility: req.visibility,
			Entrypoint: "index.html", Files: sharepkg.TranscriptFiles(files),
		}
		if update {
			result.result, result.err = updater.Update(ctx, updateID, request)
		} else {
			result.result, result.err = req.publisher.Create(ctx, request)
		}
		if result.err != nil {
			return result
		}
		if result.result.Provider == "" {
			result.result.Provider = req.capabilities.Provider.ID
		}
		result.copyAttempted = true
		result.copyMethod, result.copyErr = copyTextBestEffort(result.result.URL)
		return result
	}
	return updatedModel, tea.Batch(m.spinner.Tick, cmd)
}

func (m *Model) handleShareDone(msg shareDoneMsg) (tea.Model, tea.Cmd) {
	m.shareInFlight = false
	if msg.err != nil {
		return m.showFooterError("Share failed: " + msg.err.Error())
	}
	if msg.result.ID == "" || msg.result.URL == "" || msg.sessionID == "" || msg.store == nil {
		return m.showFooterError("Share failed: empty provider result.")
	}
	now := time.Now()
	sharedAt := now
	if msg.updated && !msg.priorSharedAt.IsZero() {
		sharedAt = msg.priorSharedAt
	}
	state := &session.ShareState{
		Provider: string(msg.result.Provider), ID: msg.result.ID, URL: msg.result.URL,
		SourceURL: msg.result.SourceURL, Visibility: string(msg.result.Visibility), Scope: session.ShareScopeSession,
		SharedAt: sharedAt, UpdatedAt: now,
	}
	state.Normalize()
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := session.UpdateShare(ctx, msg.store, msg.sessionID, state); err != nil {
		return m.showFooterError("Share created, but saving share state failed: " + err.Error())
	}
	if m.sess != nil && m.sess.ID == msg.sessionID {
		m.sess.Share = state
	}
	copied := msg.copyAttempted && msg.copyErr == nil
	action := "Created new share"
	if msg.updated {
		action = "Updated existing share"
	}
	content := fmt.Sprintf("%s\n\nLink:\n%s\n\nVisibility: %s", action, msg.result.URL, msg.result.Visibility)
	if msg.providerName != "" {
		content += "\n\nProvider: " + msg.providerName
	}
	for _, note := range msg.providerNotes {
		content += "\n\nNote: " + note
	}
	if msg.providerHelp != "" {
		content += "\n\nHelp: " + msg.providerHelp
	}
	if msg.includedRaw {
		content += "\n\nWARNING: This share includes raw model reasoning, which may contain private or sensitive information."
	}
	if msg.result.SourceURL != "" {
		content += "\n\nSource:\n" + msg.result.SourceURL
	}
	if !msg.result.Ready {
		content += "\n\nThe share was created, but anonymous readiness could not be confirmed yet."
	}
	if msg.updated && msg.requestedVisibility != "" && msg.requestedVisibility != msg.result.Visibility {
		content += fmt.Sprintf("\n\nVisibility unchanged: the provider kept this share %s instead of %s.", msg.result.Visibility, msg.requestedVisibility)
	}
	if copied {
		if msg.copyMethod == clipboard.CopyMethodOSC52 {
			content += "\n\nURL copied to clipboard via OSC 52."
		} else {
			content += "\n\nURL copied to clipboard."
		}
	} else {
		content += "\n\nClipboard copy failed; copy the link above."
	}
	m.clearFooterMessage()
	m.dialog.ShowContent("Session shared", content)
	return m, nil
}
