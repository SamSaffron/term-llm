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
	sessionsAutotitleDryRun  bool
	sessionsAutotitleForce   bool
	sessionsAutotitleMinAge  time.Duration
	sessionsAutotitleVerbose bool
)

type autotitleSkipStats struct {
	recent         int
	customName     int
	alreadyTitled  int
	trivial        int
	rejected       int
	generationErrs int
}

func (s autotitleSkipStats) total() int {
	return s.recent + s.customName + s.alreadyTitled + s.trivial + s.rejected + s.generationErrs
}

func (s autotitleSkipStats) print(out interface{ Write([]byte) (int, error) }) {
	if s.total() == 0 {
		return
	}
	fmt.Fprintln(out, "Skipped summary:")
	if s.recent > 0 {
		fmt.Fprintf(out, "  recent (< %s): %d\n", sessionsAutotitleMinAge, s.recent)
	}
	if s.customName > 0 {
		fmt.Fprintf(out, "  custom name present: %d\n", s.customName)
	}
	if s.alreadyTitled > 0 {
		fmt.Fprintf(out, "  already titled: %d\n", s.alreadyTitled)
	}
	if s.trivial > 0 {
		fmt.Fprintf(out, "  trivial (skipped until updated): %d\n", s.trivial)
	}
	if s.rejected > 0 {
		fmt.Fprintf(out, "  generated titles rejected: %d\n", s.rejected)
	}
	if s.generationErrs > 0 {
		fmt.Fprintf(out, "  generation errors: %d\n", s.generationErrs)
	}
}

func init() {
	sessionsAutotitleCmd.Flags().IntVar(&sessionsLimit, "limit", 50, "Maximum number of recent sessions to inspect")
	sessionsAutotitleCmd.Flags().BoolVar(&sessionsAutotitleDryRun, "dry-run", false, "Preview generated titles without saving")
	sessionsAutotitleCmd.Flags().BoolVar(&sessionsAutotitleForce, "force", false, "Regenerate even when a custom name already exists")
	sessionsAutotitleCmd.Flags().DurationVar(&sessionsAutotitleMinAge, "min-age", 3*time.Minute, "Skip sessions updated more recently than this duration")
	sessionsAutotitleCmd.Flags().BoolVarP(&sessionsAutotitleVerbose, "verbose", "v", false, "Print rejected candidates with rejection reason")
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
	var (
		candidates []candidate
		skips      autotitleSkipStats
	)
	for _, summary := range summaries {
		// Skip sessions with no messages at all (no point loading full session).
		if summary.MessageCount == 0 {
			continue
		}
		sess, err := store.Get(ctx, summary.ID)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "#%d load failed: %v\n", summary.Number, err)
			continue
		}
		if sess == nil {
			continue
		}
		if sessionsAutotitleMinAge > 0 && time.Since(sess.UpdatedAt) < sessionsAutotitleMinAge {
			skips.recent++
			continue
		}
		// Only skip custom-named sessions if they already have a generated title too.
		// If Name is set but GeneratedShortTitle is empty, still generate — the generated
		// title serves as a backup and the Name will still win in the UI.
		if sess.Name != "" && sess.GeneratedShortTitle != "" && !sessionsAutotitleForce {
			skips.customName++
			continue
		}
		if sess.GeneratedShortTitle != "" && !sessionsAutotitleForce {
			skips.alreadyTitled++
			continue
		}
		// Skip sessions previously marked trivial unless they've been updated since.
		if !sess.TitleSkippedAt.IsZero() && !sess.UpdatedAt.After(sess.TitleSkippedAt) && !sessionsAutotitleForce {
			skips.trivial++
			continue
		}
		candidates = append(candidates, candidate{summary: summary, sess: sess})
	}

	if len(candidates) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions need titles.")
		skips.print(cmd.OutOrStdout())
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

		// Only the first few user+assistant messages are used for titling.
		// Fetch at most 50 rows to avoid loading entire large sessions.
		messages, err := store.GetMessages(ctx, sess.ID, 50, 0)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "#%d messages failed: %v\n", sess.Number, err)
			continue
		}

		cand, err := sessiontitle.Generate(ctx, fastProvider, sess, messages)
		current := sess.PreferredShortTitle()
		if strings.TrimSpace(current) == "" {
			current = "Untitled"
		}
		if err != nil {
			if strings.Contains(err.Error(), "generated titles rejected") {
				skips.rejected++
				if sessionsAutotitleVerbose {
					fmt.Fprintf(cmd.ErrOrStderr(), "#%d rejected: %v\n", sess.Number, err)
				}
				// Mark session as trivial so we don't retry until it changes.
				// Uses MarkTitleSkipped (not Update) to avoid bumping updated_at,
				// which would cause the skip check to immediately re-qualify the session.
				if !sessionsAutotitleDryRun {
					if uerr := store.MarkTitleSkipped(ctx, sess.ID, time.Now().UTC()); uerr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "#%d skip-mark failed: %v\n", sess.Number, uerr)
					}
				}
			} else {
				skips.generationErrs++
				fmt.Fprintf(cmd.ErrOrStderr(), "#%d generation failed: %v\n", sess.Number, err)
			}
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "#%d\n  current: %s\n", sess.Number, current)
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

	skips.print(cmd.OutOrStdout())
	if !sessionsAutotitleDryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "Saved titles for %d sessions.\n", generated)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Generated titles for %d sessions (dry run).\n", generated)
	}
	return nil
}
