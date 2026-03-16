package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sessiontitle"
	"github.com/spf13/cobra"
)

var sessionsAutotitleCmd = &cobra.Command{
	Use:   "autotitle",
	Short: "Generate candidate titles for recent sessions",
	Long: `Generate short and long candidate titles for recent sessions using the configured fast model.

Titles are saved by default. Use --dry-run to preview without saving.
User-set names always win in the UI and are not overwritten unless --force is provided.`,
	RunE: runSessionsAutotitle,
}

var (
	sessionsAutotitleDryRun bool
	sessionsAutotitleForce  bool
	sessionsAutotitleMinAge time.Duration
)

func init() {
	sessionsAutotitleCmd.Flags().IntVar(&sessionsLimit, "limit", 50, "Maximum number of recent sessions to inspect")
	sessionsAutotitleCmd.Flags().BoolVar(&sessionsAutotitleDryRun, "dry-run", false, "Preview generated titles without saving")
	sessionsAutotitleCmd.Flags().BoolVar(&sessionsAutotitleForce, "force", false, "Regenerate even when a custom name already exists")
	sessionsAutotitleCmd.Flags().DurationVar(&sessionsAutotitleMinAge, "min-age", 3*time.Minute, "Skip sessions updated more recently than this duration")
	sessionsCmd.AddCommand(sessionsAutotitleCmd)
}

func runSessionsAutotitle(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	summaries, err := store.List(ctx, session.ListOptions{Limit: sessionsLimit})
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	if len(summaries) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions found.")
		return nil
	}

	// Filter to sessions that actually need titling before creating the provider.
	type candidate struct {
		summary session.SessionSummary
		sess    *session.Session
	}
	var candidates []candidate
	for _, summary := range summaries {
		sess, err := store.Get(ctx, summary.ID)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "#%d load failed: %v\n", summary.Number, err)
			continue
		}
		if sess == nil || sess.UserTurns == 0 {
			continue
		}
		if sessionsAutotitleMinAge > 0 && time.Since(sess.UpdatedAt) < sessionsAutotitleMinAge {
			fmt.Fprintf(cmd.OutOrStdout(), "#%d\n  skipped: updated %s ago (min-age %s)\n\n",
				sess.Number, time.Since(sess.UpdatedAt).Truncate(time.Second), sessionsAutotitleMinAge)
			continue
		}
		if sess.Name != "" && !sessionsAutotitleForce {
			fmt.Fprintf(cmd.OutOrStdout(), "#%d\n  current: %s\n  skipped: custom name present\n\n", sess.Number, sess.Name)
			continue
		}
		if sess.GeneratedShortTitle != "" && !sessionsAutotitleForce {
			fmt.Fprintf(cmd.OutOrStdout(), "#%d\n  current: %s\n  skipped: already titled\n\n", sess.Number, sess.PreferredShortTitle())
			continue
		}
		candidates = append(candidates, candidate{summary: summary, sess: sess})
	}

	if len(candidates) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "All sessions already have titles.")
		return nil
	}

	fastProvider, err := llm.NewFastProvider(cfg, cfg.DefaultProvider)
	if err != nil {
		return fmt.Errorf("fast provider: %w", err)
	}
	if fastProvider == nil {
		return fmt.Errorf("no fast provider configured for %q", cfg.DefaultProvider)
	}

	generated := 0
	for _, c := range candidates {
		sess := c.sess

		messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "#%d messages failed: %v\n", sess.Number, err)
			continue
		}

		cand, err := sessiontitle.Generate(ctx, fastProvider, sess, messages)
		current := sess.PreferredShortTitle()
		if strings.TrimSpace(current) == "" {
			current = "Untitled"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "#%d\n  current: %s\n", sess.Number, current)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "  skipped: %v\n\n", err)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  short:   %s\n  long:    %s\n", cand.ShortTitle, cand.LongTitle)

		if !sessionsAutotitleDryRun {
			sess.GeneratedShortTitle = cand.ShortTitle
			sess.GeneratedLongTitle = cand.LongTitle
			sess.TitleSource = session.TitleSourceGenerated
			sess.TitleGeneratedAt = time.Now().UTC()
			if len(messages) > 0 {
				sess.TitleBasisMsgSeq = messages[len(messages)-1].Sequence
			}
			if err := store.Update(ctx, sess); err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "  save:    failed (%v)\n\n", err)
				continue
			}
			fmt.Fprintln(cmd.OutOrStdout(), "  save:    ok")
			generated++
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}

	if !sessionsAutotitleDryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "Saved titles for %d sessions.\n", generated)
	}
	return nil
}
