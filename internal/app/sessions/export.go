package sessions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents/gist"
	"github.com/samsaffron/term-llm/internal/config"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func (a App) Export(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	messages, _, err := session.LoadScrollbackWithBoundary(ctx, store, sess)
	if err != nil {
		return fmt.Errorf("failed to get messages: %w", err)
	}
	messages = session.VisibleExportMessages(messages)

	var outputPath string
	if len(args) > 1 {
		outputPath = args[1]
	} else {
		name := sess.PreferredShortTitle()
		if name == "" {
			name = fmt.Sprintf("session-%d", sess.Number)
		}
		outputPath = fmt.Sprintf("%s.md", name)
	}

	// Apply export policy from config and explicit flags.
	opts := buildSessionExportOptions(
		sess,
		a.ExportIncludeSystem,
		a.ExportIncludeReasoning,
		a.ExportIncludeRawReasoning,
	)
	opts.IncludeAudio = a.ExportIncludeAudio
	content, exportResult, err := RenderExport(sess, messages, opts, outputPath)
	if err != nil {
		return err
	}

	if opts.IncludeAudio {
		if !isHTMLExportPath(outputPath) {
			fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: --include-audio is ignored for non-HTML output; use an .html or .htm filename to embed recordings.")
		} else {
			reportAudioOmissions(cmd, exportResult)
		}
	}

	if err := WriteExport(outputPath, content, exportResult.AudioEmbedded > 0); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Exported %d messages to %s\n", len(messages), outputPath)
	return nil
}

// RenderExport keeps Markdown text-only; an explicit HTML filename
// selects the self-contained player-capable exporter.
func RenderExport(sess *session.Session, messages []session.Message, opts session.ExportOptions, outputPath string) (string, session.ExportResult, error) {
	if isHTMLExportPath(outputPath) {
		return session.ExportToHTMLWithResult(sess, messages, opts)
	}
	return session.ExportToMarkdown(sess, messages, opts), session.ExportResult{}, nil
}

func isHTMLExportPath(outputPath string) bool {
	ext := strings.ToLower(filepath.Ext(outputPath))
	return ext == ".html" || ext == ".htm"
}

// Protect both new files and an existing, previously public export before any
// private recording bytes are written. Audio-free exports keep their old mode.
func WriteExport(path, content string, embeddedAudio bool) error {
	if !embeddedAudio {
		return os.WriteFile(path, []byte(content), 0644)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		return err
	}
	return file.Close()
}

func sessionExportReasoningSurface(sess *session.Session) string {
	if sess == nil {
		return "chat"
	}
	if sess.Origin == session.OriginWeb || sess.Origin == session.OriginTelegram {
		return "serve"
	}
	switch sess.Mode {
	case session.ModeAsk:
		return "ask"
	default:
		return "chat"
	}
}

func loadSessionReasoningConfig(surface string) config.ReasoningConfig {
	if cfg, err := config.Load(); err == nil && cfg != nil {
		return cfg.ResolveReasoning(surface)
	}

	for _, path := range candidateConfigFiles() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var raw struct {
			Reasoning config.ReasoningConfig `yaml:"reasoning"`
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			continue
		}
		return (&config.Config{Reasoning: raw.Reasoning}).ResolveReasoning(surface)
	}
	return (&config.Config{}).ResolveReasoning(surface)
}

func candidateConfigFiles() []string {
	var paths []string
	if dir, err := config.GetConfigDir(); err == nil && dir != "" {
		paths = append(paths, filepath.Join(dir, "config.yaml"))
	}
	paths = append(paths, "config.yaml")
	return paths
}

func buildSessionExportOptions(sess *session.Session, includeSystem, includeReasoningSummaries, includeRawReasoning bool) session.ExportOptions {
	reasoningCfg := loadSessionReasoningConfig(sessionExportReasoningSurface(sess))

	rawRequested := includeRawReasoning || strings.EqualFold(strings.TrimSpace(reasoningCfg.Export), config.ReasoningExportRaw)
	includeSummaries := includeReasoningSummaries || includeRawReasoning || internalreasoning.ExportSummaries(reasoningCfg)
	includeRaw := false
	if rawRequested {
		switch {
		case !reasoningCfg.Raw:
			fmt.Fprintln(os.Stderr, "Raw reasoning export is disabled. Set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.")
		case !internalreasoning.SourceAllowsRaw(reasoningCfg):
			fmt.Fprintln(os.Stderr, "Raw reasoning export is disabled by reasoning.source. Set reasoning.source=all to allow it.")
		default:
			includeRaw = true
			includeSummaries = true
		}
	}

	return session.ExportOptions{
		IncludeSystem:             includeSystem,
		IncludeReasoningSummaries: includeSummaries,
		IncludeRawReasoning:       includeRaw,
	}
}
func (a App) ExportGist(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	messages, _, err := session.LoadScrollbackWithBoundary(ctx, store, sess)
	if err != nil {
		return fmt.Errorf("failed to get messages: %w", err)
	}
	messages = session.VisibleExportMessages(messages)

	opts := buildSessionExportOptions(
		sess,
		a.GistIncludeSystem,
		a.GistIncludeReasoning,
		a.GistIncludeRawReasoning,
	)
	files, err := session.GistFiles(sess, messages, opts)
	if err != nil {
		return err
	}

	client, err := gist.NewClient()
	if err != nil {
		return err
	}

	name := sess.Name
	if name == "" {
		name = fmt.Sprintf("#%d", sess.Number)
	}
	description := fmt.Sprintf("term-llm session: %s", name)

	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Creating Gist...")
	g, err := client.Create(description, a.GistPublic, files)
	if err != nil {
		return fmt.Errorf("create gist: %w", err)
	}
	preview := session.GistPreviewURL(g.ID)
	if preview == "" {
		return fmt.Errorf("created Gist returned an invalid ID %q", g.ID)
	}

	fmt.Fprintln(out, "Share:")
	fmt.Fprintln(out, preview)
	fmt.Fprintf(out, "Gist/source: %s\n", g.URL)
	return nil
}
