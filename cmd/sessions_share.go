package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
	sharepkg "github.com/samsaffron/term-llm/internal/share"
	"github.com/spf13/cobra"
)

var newSessionSharePublisher = sharepkg.NewPublisher

type sessionsShareOutput struct {
	Provider   sharepkg.ProviderID `json:"provider"`
	ID         string              `json:"id"`
	URL        string              `json:"url"`
	SourceURL  string              `json:"source_url,omitempty"`
	Visibility sharepkg.Visibility `json:"visibility"`
	Ready      bool                `json:"ready"`
	Scope      session.ShareScope  `json:"scope"`
	Updated    bool                `json:"updated"`
}

func runSessionsShare(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	storeCfg := sessionStoreConfig(cfg)
	if !storeCfg.Enabled {
		return fmt.Errorf("session storage is disabled (check sessions.enabled and --no-session)")
	}
	store, err := session.NewStore(storeCfg)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session %q not found", args[0])
	}

	requestedVisibility := sharepkg.Visibility(strings.ToLower(strings.TrimSpace(sessionsShareVisibility)))
	if requestedVisibility != "" && !sharepkg.ValidVisibility(requestedVisibility) {
		return fmt.Errorf("invalid visibility %q: expected public, unlisted, or private", sessionsShareVisibility)
	}
	publisher, err := newSessionSharePublisher(cfg.Share)
	if err != nil {
		return err
	}
	capabilities, err := publisher.Capabilities(ctx)
	if err != nil {
		return err
	}
	if err := sharepkg.ValidateCapabilities(capabilities); err != nil {
		return sharepkg.NewError(sharepkg.ErrorProtocol, "sharing provider returned invalid capabilities")
	}
	visibility := requestedVisibility
	if visibility == "" {
		visibility = capabilities.DefaultVisibility
		if sess.Share != nil {
			sess.Share.Normalize()
			if sess.Share.Provider == string(capabilities.Provider.ID) && sess.Share.Scope == session.ShareScopeSession && sharepkg.ValidVisibility(sharepkg.Visibility(sess.Share.Visibility)) {
				visibility = sharepkg.Visibility(sess.Share.Visibility)
			}
		}
	}
	if !capabilities.SupportsVisibility(visibility) {
		return sharepkg.NewError(sharepkg.ErrorUnsupportedVisibility, fmt.Sprintf("%s visibility is not supported by %s", visibility, capabilities.Provider.Name))
	}
	if !sessionsShareJSON {
		for _, note := range capabilities.Notes {
			fmt.Fprintf(cmd.OutOrStdout(), "Note: %s\n", note)
		}
		if capabilities.Provider.Help != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "Help: %s\n", capabilities.Provider.Help)
		}
	}

	messages, _, err := session.LoadScrollbackWithBoundary(ctx, store, sess)
	if err != nil {
		return fmt.Errorf("failed to get messages: %w", err)
	}
	exportOptions, err := sessionShareExportOptions(cfg, sess, sessionsShareIncludeRawReasoning)
	if err != nil {
		return err
	}
	if sessionsShareIncludeRawReasoning {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: raw model reasoning was explicitly requested and may contain private or sensitive information.")
	}
	files, err := session.ShareFiles(sess, session.VisibleExportMessages(messages), exportOptions)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(sess.PreferredShortTitle())
	if name == "" {
		name = fmt.Sprintf("#%d", sess.Number)
	}
	request := sharepkg.Request{
		RequestID: sharepkg.NewRequestID(), Title: name,
		Description: "term-llm session: " + name, Visibility: visibility,
		Entrypoint: "index.html", Files: sharepkg.TranscriptFiles(files),
	}

	updated := false
	var result sharepkg.Result
	hadExistingShare := sess.Share != nil && sess.Share.Exists()
	if hadExistingShare {
		sess.Share.Normalize()
	}
	if !sessionsShareNew && hadExistingShare {
		updater, canUpdate := publisher.(sharepkg.Updater)
		compatible := canUpdate && sess.Share.Provider == string(capabilities.Provider.ID) &&
			sess.Share.Scope == session.ShareScopeSession && capabilities.Supports(sharepkg.OperationUpdate)
		if compatible {
			result, err = updater.Update(ctx, sess.Share.ID, request)
			updated = err == nil
		}
	}
	if !updated && err == nil {
		if hadExistingShare {
			fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: creating a new %s share will replace the saved share state; the previous %s link may remain active and must be managed with that provider.\n", capabilities.Provider.Name, sess.Share.Provider)
		}
		result, err = publisher.Create(ctx, request)
	}
	if err != nil {
		return err
	}
	if result.Provider == "" {
		result.Provider = capabilities.Provider.ID
	}

	now := time.Now()
	sharedAt := now
	if updated && sess.Share != nil && !sess.Share.SharedAt.IsZero() {
		sharedAt = sess.Share.SharedAt
	}
	state := &session.ShareState{
		Provider: string(result.Provider), ID: result.ID, URL: result.URL, SourceURL: result.SourceURL,
		Visibility: string(result.Visibility), Scope: session.ShareScopeSession,
		SharedAt: sharedAt, UpdatedAt: now,
	}
	state.Normalize()
	if err := session.UpdateShare(ctx, store, sess.ID, state); err != nil {
		return fmt.Errorf("share created, but saving share state failed: %w", err)
	}

	output := sessionsShareOutput{
		Provider: result.Provider, ID: result.ID, URL: result.URL, SourceURL: result.SourceURL,
		Visibility: result.Visibility, Ready: result.Ready, Scope: session.ShareScopeSession, Updated: updated,
	}
	if sessionsShareJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(output)
	}
	action := "Created share"
	if updated {
		action = "Updated share"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s with %s\n%s\n", action, capabilities.Provider.Name, result.URL)
	if result.SourceURL != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Source: %s\n", result.SourceURL)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Visibility: %s\n", result.Visibility)
	if updated && requestedVisibility != "" && requestedVisibility != result.Visibility {
		fmt.Fprintf(cmd.OutOrStdout(), "Visibility unchanged: the provider kept this share %s instead of %s.\n", result.Visibility, requestedVisibility)
	}
	if !result.Ready {
		fmt.Fprintln(cmd.OutOrStdout(), "The share was created, but anonymous readiness could not be confirmed yet.")
	}
	return nil
}

func sessionShareExportOptions(cfg *config.Config, sess *session.Session, includeRaw bool) (session.ExportOptions, error) {
	reasoningCfg := config.DefaultReasoningConfig()
	if cfg != nil {
		reasoningCfg = cfg.ResolveReasoning(sessionExportReasoningSurface(sess))
	}
	opts := session.ExportOptions{IncludeReasoningSummaries: internalreasoning.ExportSummaries(reasoningCfg)}
	if !includeRaw {
		return opts, nil
	}
	opts.IncludeReasoningSummaries = true
	switch {
	case !reasoningCfg.Raw:
		return session.ExportOptions{}, fmt.Errorf("raw reasoning sharing is disabled; set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1")
	case !internalreasoning.SourceAllowsRaw(reasoningCfg):
		return session.ExportOptions{}, fmt.Errorf("raw reasoning sharing is disabled by reasoning.source; set reasoning.source=all")
	default:
		opts.IncludeRawReasoning = true
	}
	return opts, nil
}
