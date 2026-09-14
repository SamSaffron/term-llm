package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/doctor"
	"github.com/samsaffron/term-llm/internal/exitcode"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

// doctorOutputInteractive reports whether the destination can host an animated
// progress line. It is a variable so tests can force either mode.
var doctorOutputInteractive = func(out io.Writer) bool {
	file, ok := out.(*os.File)
	return ok && terminalpolicy.OutputInteractive(file)
}

var (
	doctorJSON         bool
	doctorFix          bool
	doctorAssumeYes    bool
	doctorOnly         []string
	doctorVerbose      bool
	doctorSkipDeferred bool
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check this install for broken configuration, credentials, and databases",
	Long: `Inspect the local term-llm install and report problems.

Every check is offline and read-only by default. Databases are compared against
the schema their migrations produce on a fresh install, config keys term-llm
ignores are reported with a suggestion, stored credentials are checked for
permissions and expiry, leftover files and rows are listed, and external
commands are probed on PATH.

Findings that can be repaired are marked "fixable". Re-run with --fix to apply
those repairs, which prompts for each one unless --yes is given.`,
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "Output findings as JSON")
	doctorCmd.Flags().BoolVar(&doctorFix, "fix", false, "Apply repairs for fixable findings")
	doctorCmd.Flags().BoolVarP(&doctorAssumeYes, "yes", "y", false, "Do not prompt before each repair")
	doctorCmd.Flags().StringSliceVar(&doctorOnly, "only", nil, "Run only these checks (db, config, credentials, mcp, stale, binaries)")
	doctorCmd.Flags().BoolVarP(&doctorVerbose, "verbose", "v", false, "Show checks that passed")
	doctorCmd.Flags().BoolVar(&doctorSkipDeferred, "skip-deferred", false, "Skip resolving op://, srv://, file:// and $() config values")
}

func runDoctor(cmd *cobra.Command, args []string) error {
	cfg, configErr := config.Load()
	checks, err := doctorChecks(cfg)
	if err != nil {
		return err
	}

	if err := validateDoctorOnly(doctorOnly, checks); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	opts := doctor.Options{Only: doctorOnly, Fix: doctorFix}

	// Live progress needs a terminal to rewrite its own line. Everything else
	// (JSON, pipes, CI) gets the same body printed once the run finishes.
	var live *doctorLiveRenderer
	if !doctorJSON && doctorOutputInteractive(out) {
		styles := doctorColorStyles()
		if os.Getenv("NO_COLOR") != "" {
			styles = doctorPlainStyles()
		}
		live = &doctorLiveRenderer{
			out:      out,
			styles:   styles,
			progress: &doctorProgress{out: out, styles: styles},
			verbose:  doctorVerbose,
		}
		opts.Observer = live
	}
	if !doctorJSON && configErr != nil {
		fmt.Fprintf(out, "note: config failed to load, defaults are in use: %v\n\n", configErr)
	}
	if doctorFix && !doctorAssumeYes {
		opts.Confirm = doctorConfirmer(cmd, live)
	}

	report := doctor.Run(cmd.Context(), checks, opts)

	switch {
	case doctorJSON:
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
	case live != nil:
		renderDoctorSummary(out, live.styles, report)
	default:
		renderDoctorReport(out, checks, report, doctorVerbose)
	}

	if report.Worst() == doctor.SeverityError {
		return exitcode.ExitError{Code: exitcode.Error, Message: "doctor found problems that need attention"}
	}
	return nil
}

// validateDoctorOnly rejects unknown selectors so a typo reports nothing
// silently instead of looking like a clean bill of health.
func validateDoctorOnly(only []string, checks []doctor.Check) error {
	if len(only) == 0 {
		return nil
	}
	known := doctor.CheckIDs(checks)
	for _, want := range only {
		want = strings.TrimSpace(want)
		matched := false
		for _, id := range known {
			if id == want || strings.HasPrefix(id, want+".") {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("unknown check %q; available checks: %s", want, strings.Join(known, ", "))
		}
	}
	return nil
}

// doctorChecks assembles every check with paths resolved from the effective
// config, so doctor inspects the same files term-llm itself would use.
func doctorChecks(cfg *config.Config) ([]doctor.Check, error) {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}

	sessionsPath, err := session.ResolveDBPath(sessionConfigPath(cfg))
	if err != nil {
		return nil, fmt.Errorf("resolve sessions database: %w", err)
	}
	memoryPath, err := memory.GetDBPath()
	if err != nil {
		return nil, fmt.Errorf("resolve memory database: %w", err)
	}
	jobsPath := filepath.Join(dataDir, "jobs_v2.db")
	fileHistoryPath := fileHistoryDBPath(cfg, dataDir)
	attentionPath := filepath.Join(dataDir, "hub", "attention.db")

	databases := &doctor.DatabaseCheck{Targets: []doctor.DatabaseTarget{
		{
			Name:           "sessions.db",
			Path:           sessionsPath,
			FTS:            []doctor.FTSIndex{{Name: "messages_fts", Source: "messages"}},
			BuildReference: buildSessionsReference,
		},
		{
			Name: "memory.db",
			Path: memoryPath,
			FTS: []doctor.FTSIndex{
				{Name: "memory_fts", Source: "memory_fragments"},
				{Name: "memory_insights_fts", Source: "memory_insights"},
				{Name: "generated_images_fts", Source: "generated_images"},
			},
			BuildReference: buildMemoryReference,
		},
		{
			Name:           "jobs_v2.db",
			Path:           jobsPath,
			BuildReference: buildJobsReference,
		},
		{
			Name:           "file_history.db",
			Path:           fileHistoryPath,
			BuildReference: buildFileHistoryReference,
		},
		{
			// The file-tracking store opens this sidecar next to its primary
			// database, so it drifts independently and needs its own check.
			Name:           "file_observations.db",
			Path:           filepath.Join(filepath.Dir(fileHistoryPath), "file_observations.db"),
			BuildReference: buildFileObservationsReference,
		},
		{
			Name:           "attention.db",
			Path:           attentionPath,
			BuildReference: buildAttentionReference,
		},
	}}

	stale := &doctor.StaleCheck{
		DataDir:   dataDir,
		ConfigDir: configDir,
		Orphans: []doctor.OrphanTarget{
			{
				Name: "sessions.db",
				Path: sessionsPath,
				Rules: []doctor.OrphanRule{
					{
						Label:     "messages whose session was deleted",
						CountSQL:  `SELECT COUNT(*) FROM messages m WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = m.session_id)`,
						DeleteSQL: `DELETE FROM messages WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = messages.session_id)`,
					},
					{
						Label:     "workspace grants whose session was deleted",
						CountSQL:  `SELECT COUNT(*) FROM session_workspace_grants g WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = g.session_id)`,
						DeleteSQL: `DELETE FROM session_workspace_grants WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = session_workspace_grants.session_id)`,
					},
					{
						Label:     "queued push notifications whose subscription is gone",
						CountSQL:  `SELECT COUNT(*) FROM completion_push_outbox o WHERE NOT EXISTS (SELECT 1 FROM push_subscriptions p WHERE p.id = o.subscription_id)`,
						DeleteSQL: `DELETE FROM completion_push_outbox WHERE NOT EXISTS (SELECT 1 FROM push_subscriptions p WHERE p.id = completion_push_outbox.subscription_id)`,
					},
				},
			},
			{
				Name: "jobs_v2.db",
				Path: jobsPath,
				Rules: []doctor.OrphanRule{
					{
						Label:     "job runs whose job was deleted",
						CountSQL:  `SELECT COUNT(*) FROM job_runs_v2 r WHERE NOT EXISTS (SELECT 1 FROM jobs_v2 j WHERE j.id = r.job_id)`,
						DeleteSQL: `DELETE FROM job_runs_v2 WHERE NOT EXISTS (SELECT 1 FROM jobs_v2 j WHERE j.id = job_runs_v2.job_id)`,
					},
					{
						Label:     "job run events whose run was deleted",
						CountSQL:  `SELECT COUNT(*) FROM job_run_events_v2 e WHERE NOT EXISTS (SELECT 1 FROM job_runs_v2 r WHERE r.id = e.run_id)`,
						DeleteSQL: `DELETE FROM job_run_events_v2 WHERE NOT EXISTS (SELECT 1 FROM job_runs_v2 r WHERE r.id = job_run_events_v2.run_id)`,
					},
				},
			},
		},
	}

	configPath, err := config.GetConfigPath()
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	declared := doctor.DeclaredProviderNames(configPath)

	return []doctor.Check{
		databases,
		&doctor.ConfigCheck{Config: cfg, ResolveDeferred: !doctorSkipDeferred},
		&doctor.CredentialsCheck{Config: cfg, Declared: declared},
		&doctor.MCPCheck{},
		stale,
		&doctor.BinaryCheck{Config: cfg},
	}, nil
}

// sessionConfigPath mirrors how commands pick the sessions database so doctor
// inspects the same file, including the --session-db override.
func sessionConfigPath(cfg *config.Config) string {
	if override := strings.TrimSpace(sessionDBPath); override != "" {
		return override
	}
	if cfg == nil {
		return ""
	}
	return cfg.Sessions.Path
}

func fileHistoryDBPath(cfg *config.Config, dataDir string) string {
	if cfg != nil && strings.TrimSpace(cfg.FileTracking.Path) != "" {
		return cfg.FileTracking.Path
	}
	return filepath.Join(dataDir, "file_history.db")
}

func buildSessionsReference(ctx context.Context, path string) error {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		return err
	}
	return store.Close()
}

func buildMemoryReference(ctx context.Context, path string) error {
	store, err := memory.NewStore(memory.Config{Path: path})
	if err != nil {
		return err
	}
	return store.Close()
}

func buildJobsReference(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return initJobsV2Schema(ctx, db)
}

func buildFileHistoryReference(ctx context.Context, path string) error {
	store, err := filetrack.Open(path, filetrack.Options{})
	if err != nil {
		return err
	}
	return store.Close()
}

// buildFileObservationsReference opens a file-tracking store beside the target
// path, which is how the sidecar observation database gets created.
func buildFileObservationsReference(ctx context.Context, path string) error {
	return buildFileHistoryReference(ctx, filepath.Join(filepath.Dir(path), "file_history.db"))
}

func buildAttentionReference(ctx context.Context, path string) error {
	store, err := hub.OpenAttentionProjectionStore(path)
	if err != nil {
		return err
	}
	return store.Close()
}

// doctorConfirmer prompts once per repair on a shared reader so buffered input
// is not lost between questions. The prompt clears any progress line first so
// the question is not written over the spinner.
func doctorConfirmer(cmd *cobra.Command, live *doctorLiveRenderer) func(doctor.Finding) bool {
	reader := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()
	return func(finding doctor.Finding) bool {
		if live != nil {
			live.progress.clear()
		}
		fmt.Fprintf(out, "Fix: %s? [y/N]: ", finding.Title)
		line, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintln(out)
			return false
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes"
	}
}

// renderDoctorReport prints every check that ran, in order, followed by the
// summary. It is the batch counterpart of the live renderer and produces the
// same body, so piped output matches what a terminal shows.
func renderDoctorReport(out io.Writer, checks []doctor.Check, report doctor.Report, verbose bool) {
	styles := doctorPlainStyles()
	grouped := map[string][]doctor.Finding{}
	var order []string
	for _, finding := range report.Findings {
		if _, seen := grouped[finding.Check]; !seen {
			order = append(order, finding.Check)
		}
		grouped[finding.Check] = append(grouped[finding.Check], finding)
	}
	// Ran includes checks that produced no findings at all; those still deserve
	// a line so a clean install visibly passes rather than printing nothing.
	if len(report.Ran) > 0 {
		order = report.Ran
	}

	titles := map[string]string{}
	for _, check := range checks {
		if check != nil {
			titles[check.ID()] = check.Title()
		}
	}
	for _, id := range order {
		title := titles[id]
		if title == "" {
			title = id
		}
		renderDoctorGroup(out, styles, title, grouped[id], verbose)
	}
	renderDoctorSummary(out, styles, report)
}

// renderDoctorGroup prints one check's status line and its visible findings.
func renderDoctorGroup(out io.Writer, styles doctorStyles, title string, findings []doctor.Finding, verbose bool) {
	severity := doctor.WorstSeverity(findings)
	fmt.Fprintf(out, "%s %s\n", styles.mark(severity), title)
	for _, finding := range findings {
		if finding.Severity == doctor.SeverityOK && !verbose && !finding.Fixed {
			continue
		}
		suffix := ""
		if finding.Fixable && !finding.Fixed {
			suffix = " (fixable)"
		}
		fmt.Fprintf(out, "    %s %s%s\n", styles.label(finding), finding.Title, suffix)
		for _, line := range strings.Split(finding.Detail, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Fprintf(out, "          %s\n", styles.muted(line))
		}
		if finding.FixError != "" {
			fmt.Fprintf(out, "          %s\n", styles.bad("fix failed: "+finding.FixError))
		}
		if finding.Remedy != "" && !finding.Fixed {
			fmt.Fprintf(out, "          → %s\n", finding.Remedy)
		}
	}
}

func renderDoctorSummary(out io.Writer, styles doctorStyles, report doctor.Report) {
	if report.Worst() <= doctor.SeverityInfo {
		fmt.Fprintf(out, "\n%s %s\n", styles.good("Nothing to report."), report.Summary())
		return
	}
	fmt.Fprintf(out, "\n%s\n", report.Summary())
	if report.HasFixable() && !doctorFix {
		fmt.Fprintln(out, "Re-run with --fix to repair the findings marked fixable.")
	}
}

// doctorLiveRenderer prints each check as it completes, replacing a transient
// progress line so long checks show activity instead of a frozen terminal.
type doctorLiveRenderer struct {
	out      io.Writer
	styles   doctorStyles
	progress *doctorProgress
	verbose  bool
}

func (r *doctorLiveRenderer) CheckStarted(check doctor.Check) {
	r.progress.start(check.Title())
}

func (r *doctorLiveRenderer) CheckFinished(check doctor.Check, findings []doctor.Finding) {
	r.progress.clear()
	renderDoctorGroup(r.out, r.styles, check.Title(), findings, r.verbose)
}

var doctorSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const doctorSpinnerInterval = 90 * time.Millisecond

// doctorProgress animates a single-line "checking ..." indicator. A nil
// receiver is a no-op, which is how non-terminal output disables it.
type doctorProgress struct {
	out    io.Writer
	styles doctorStyles

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

func (p *doctorProgress) start(label string) {
	if p == nil {
		return
	}
	p.clear()
	stop := make(chan struct{})
	done := make(chan struct{})
	p.stop, p.done = stop, done
	go func() {
		defer close(done)
		for frame := 0; ; frame++ {
			p.draw(label, frame)
			select {
			case <-stop:
				return
			case <-time.After(doctorSpinnerInterval):
			}
		}
	}()
}

func (p *doctorProgress) draw(label string, frame int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	spinner := doctorSpinnerFrames[frame%len(doctorSpinnerFrames)]
	fmt.Fprintf(p.out, "\r\x1b[K%s %s", p.styles.muted(spinner), p.styles.muted("checking "+label+"…"))
}

// clear stops the animation and erases the progress line so ordinary output,
// including repair prompts, starts on a clean line.
func (p *doctorProgress) clear() {
	if p == nil || p.stop == nil {
		return
	}
	close(p.stop)
	<-p.done
	p.stop, p.done = nil, nil
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprint(p.out, "\r\x1b[K")
}

// doctorStyles colors output only when the destination is an interactive
// terminal; every function is the identity otherwise.
type doctorStyles struct {
	good  func(string) string
	warn  func(string) string
	bad   func(string) string
	muted func(string) string
}

func doctorPlainStyles() doctorStyles {
	identity := func(s string) string { return s }
	return doctorStyles{good: identity, warn: identity, bad: identity, muted: identity}
}

func doctorColorStyles() doctorStyles {
	styles := ui.NewStyles(os.Stdout)
	render := func(style lipgloss.Style) func(string) string {
		return func(s string) string { return style.Render(s) }
	}
	return doctorStyles{
		good:  render(styles.Success),
		warn:  render(lipgloss.NewStyle().Foreground(styles.Theme().Warning)),
		bad:   render(styles.Error),
		muted: render(styles.Muted),
	}
}

// mark renders the per-check status symbol.
func (s doctorStyles) mark(severity doctor.Severity) string {
	switch severity {
	case doctor.SeverityError:
		return s.bad("✗")
	case doctor.SeverityWarn:
		return s.warn("!")
	case doctor.SeverityInfo:
		return s.muted("✓")
	default:
		return s.good("✓")
	}
}

// label renders the per-finding severity column, padded to a fixed width.
func (s doctorStyles) label(finding doctor.Finding) string {
	if finding.Fixed {
		return s.good("fixed")
	}
	switch finding.Severity {
	case doctor.SeverityError:
		return s.bad("error")
	case doctor.SeverityWarn:
		return s.warn("warn ")
	case doctor.SeverityInfo:
		return s.muted("info ")
	default:
		return s.good("ok   ")
	}
}
