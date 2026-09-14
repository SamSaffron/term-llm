package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/doctor"
)

func doctorTestChecks(t *testing.T) []doctor.Check {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	checks, err := doctorChecks(&config.Config{})
	if err != nil {
		t.Fatalf("build checks: %v", err)
	}
	return checks
}

func doctorDatabaseTargets(t *testing.T, checks []doctor.Check) []doctor.DatabaseTarget {
	t.Helper()
	for _, check := range checks {
		if databases, ok := check.(*doctor.DatabaseCheck); ok {
			return databases.Targets
		}
	}
	t.Fatal("doctor is missing its database check")
	return nil
}

// Every store must migrate a fresh database to a schema that matches itself.
// This is the guard rail for the check: if a builder is nondeterministic or a
// migration fails on an empty file, doctor would report drift for everyone.
func TestDoctorDatabaseReferencesMatchFreshMigrations(t *testing.T) {
	checks := doctorTestChecks(t)
	targets := doctorDatabaseTargets(t, checks)
	if len(targets) == 0 {
		t.Fatal("expected database targets")
	}

	ctx := context.Background()
	for _, target := range targets {
		t.Run(target.Name, func(t *testing.T) {
			if target.BuildReference == nil {
				t.Skipf("%s has no reference builder", target.Name)
			}
			path := filepath.Join(t.TempDir(), target.Name)
			if err := target.BuildReference(ctx, path); err != nil {
				t.Fatalf("migrate a fresh %s: %v", target.Name, err)
			}

			check := &doctor.DatabaseCheck{Targets: []doctor.DatabaseTarget{{
				Name:           target.Name,
				Path:           path,
				FTS:            target.FTS,
				BuildReference: target.BuildReference,
			}}}
			if findings := check.Run(ctx); len(findings) != 0 {
				t.Fatalf("freshly migrated %s does not match its own schema: %+v", target.Name, findings)
			}
		})
	}
}

// Orphan-row SQL is skipped silently when it fails, so a typo would disable a
// check forever. Run every rule against a freshly migrated database instead.
func TestDoctorOrphanRulesRunAgainstCurrentSchemas(t *testing.T) {
	checks := doctorTestChecks(t)
	targets := doctorDatabaseTargets(t, checks)
	references := map[string]func(context.Context, string) error{}
	for _, target := range targets {
		references[target.Name] = target.BuildReference
	}

	var stale *doctor.StaleCheck
	for _, check := range checks {
		if candidate, ok := check.(*doctor.StaleCheck); ok {
			stale = candidate
		}
	}
	if stale == nil {
		t.Fatal("doctor is missing its stale-state check")
	}
	if len(stale.Orphans) == 0 {
		t.Fatal("expected orphan rules to be configured")
	}

	ctx := context.Background()
	for _, target := range stale.Orphans {
		build, ok := references[target.Name]
		if !ok || build == nil {
			t.Fatalf("orphan rules for %s have no matching database reference", target.Name)
		}
		path := filepath.Join(t.TempDir(), target.Name)
		if err := build(ctx, path); err != nil {
			t.Fatalf("migrate a fresh %s: %v", target.Name, err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open %s: %v", target.Name, err)
		}
		for _, rule := range target.Rules {
			var count int64
			if err := db.QueryRowContext(ctx, rule.CountSQL).Scan(&count); err != nil {
				t.Fatalf("%s: counting %q failed: %v", target.Name, rule.Label, err)
			}
			if count != 0 {
				t.Fatalf("%s: a fresh database reported %d orphaned rows for %q", target.Name, count, rule.Label)
			}
			if rule.DeleteSQL == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, rule.DeleteSQL); err != nil {
				t.Fatalf("%s: deleting %q failed: %v", target.Name, rule.Label, err)
			}
		}
		db.Close()
	}
}

func TestDoctorChecksResolvePathsUnderTheDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	checks, err := doctorChecks(&config.Config{})
	if err != nil {
		t.Fatalf("build checks: %v", err)
	}
	for _, target := range doctorDatabaseTargets(t, checks) {
		if !strings.HasPrefix(target.Path, dataDir) {
			t.Fatalf("%s resolved to %s, which is outside %s", target.Name, target.Path, dataDir)
		}
	}
}

func TestDoctorHonorsTheSessionDatabaseOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "elsewhere.db")
	previous := sessionDBPath
	sessionDBPath = override
	t.Cleanup(func() { sessionDBPath = previous })

	checks := doctorTestChecks(t)
	for _, target := range doctorDatabaseTargets(t, checks) {
		if target.Name != "sessions.db" {
			continue
		}
		if target.Path != override {
			t.Fatalf("sessions database resolved to %s, want %s", target.Path, override)
		}
		return
	}
	t.Fatal("no sessions database target")
}

func TestDoctorChecksCoverEveryDocumentedArea(t *testing.T) {
	checks := doctorTestChecks(t)

	got := map[string]bool{}
	for _, check := range checks {
		got[check.ID()] = true
	}
	for _, want := range []string{"db", "config", "credentials", "mcp", "stale", "binaries"} {
		if !got[want] {
			t.Fatalf("missing the %q check; --only documents it", want)
		}
	}
}

func TestValidateDoctorOnlyRejectsUnknownSelectors(t *testing.T) {
	checks := doctorTestChecks(t)

	if err := validateDoctorOnly([]string{"db", "stale"}, checks); err != nil {
		t.Fatalf("known selectors must be accepted: %v", err)
	}
	if err := validateDoctorOnly(nil, checks); err != nil {
		t.Fatalf("an empty selection must be accepted: %v", err)
	}
	err := validateDoctorOnly([]string{"databases"}, checks)
	if err == nil {
		t.Fatal("expected an unknown selector to be rejected")
	}
	if !strings.Contains(err.Error(), "available checks") {
		t.Fatalf("expected the error to list the available checks, got %v", err)
	}
}

func TestRenderDoctorReportHidesHealthyFindingsUnlessVerbose(t *testing.T) {
	previousFix := doctorFix
	doctorFix = false
	t.Cleanup(func() { doctorFix = previousFix })

	checks := []doctor.Check{&doctor.BinaryCheck{}}
	report := doctor.Report{Findings: []doctor.Finding{
		{Check: "binaries", Title: "git: found", Severity: doctor.SeverityOK},
		{Check: "binaries", Title: "rg: not found on PATH", Severity: doctor.SeverityWarn, Remedy: "install ripgrep", Fixable: true},
	}}

	var quiet bytes.Buffer
	renderDoctorReport(&quiet, checks, report, false)
	if strings.Contains(quiet.String(), "git: found") {
		t.Fatalf("healthy findings must stay hidden by default:\n%s", quiet.String())
	}
	if !strings.Contains(quiet.String(), "rg: not found on PATH (fixable)") {
		t.Fatalf("expected the fixable warning to be shown:\n%s", quiet.String())
	}
	if !strings.Contains(quiet.String(), "install ripgrep") {
		t.Fatalf("expected the remedy to be shown:\n%s", quiet.String())
	}
	if !strings.Contains(quiet.String(), "Re-run with --fix") {
		t.Fatalf("expected the fix hint:\n%s", quiet.String())
	}

	var verbose bytes.Buffer
	renderDoctorReport(&verbose, checks, report, true)
	if !strings.Contains(verbose.String(), "git: found") {
		t.Fatalf("verbose output must include healthy findings:\n%s", verbose.String())
	}
}

// Every check that ran gets a status line, including checks with no findings,
// so a healthy install visibly passes instead of printing nothing.
func TestRenderDoctorReportShowsAStatusLinePerCheck(t *testing.T) {
	checks := []doctor.Check{&doctor.BinaryCheck{}, &doctor.MCPCheck{}}
	report := doctor.Report{
		Ran: []string{"binaries", "mcp"},
		Findings: []doctor.Finding{
			{Check: "binaries", Title: "rg: not found on PATH", Severity: doctor.SeverityWarn},
		},
	}

	var out bytes.Buffer
	renderDoctorReport(&out, checks, report, false)
	got := out.String()
	if !strings.Contains(got, "! External commands") {
		t.Fatalf("expected a warning status line for the binaries check:\n%s", got)
	}
	if !strings.Contains(got, "✓ MCP servers") {
		t.Fatalf("a check without findings must still report a status line:\n%s", got)
	}
}

func TestDoctorLiveRendererReplacesProgressWithAStatusLine(t *testing.T) {
	var out bytes.Buffer
	styles := doctorPlainStyles()
	renderer := &doctorLiveRenderer{
		out:      &out,
		styles:   styles,
		progress: &doctorProgress{out: &out, styles: styles},
	}

	check := &doctor.BinaryCheck{}
	renderer.CheckStarted(check)
	renderer.CheckFinished(check, []doctor.Finding{
		{Check: "binaries", Title: "git: found", Severity: doctor.SeverityOK},
	})

	got := out.String()
	if !strings.Contains(got, "checking External commands…") {
		t.Fatalf("expected a progress line while the check runs:\n%q", got)
	}
	if !strings.Contains(got, "\r\x1b[K✓ External commands\n") {
		t.Fatalf("expected the progress line to be erased before the result:\n%q", got)
	}
	if strings.Contains(got, "git: found") {
		t.Fatalf("healthy findings must stay hidden unless verbose:\n%q", got)
	}
}

func TestRenderDoctorReportSaysNothingToReport(t *testing.T) {
	var out bytes.Buffer
	renderDoctorReport(&out, nil, doctor.Report{Findings: []doctor.Finding{
		{Check: "binaries", Title: "git: found", Severity: doctor.SeverityOK},
	}}, false)

	if !strings.Contains(out.String(), "Nothing to report") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}
