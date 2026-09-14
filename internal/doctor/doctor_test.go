package doctor

import (
	"context"
	"errors"
	"testing"
)

type stubCheck struct {
	id       string
	title    string
	findings []Finding
	ran      bool
}

func (c *stubCheck) ID() string    { return c.id }
func (c *stubCheck) Title() string { return c.title }
func (c *stubCheck) Run(ctx context.Context) []Finding {
	c.ran = true
	return c.findings
}

func TestRunOnlySelectsByExactIDOrDottedPrefix(t *testing.T) {
	db := &stubCheck{id: "db", findings: []Finding{{Title: "db finding"}}}
	dbSchema := &stubCheck{id: "db.schema", findings: []Finding{{Title: "schema finding"}}}
	config := &stubCheck{id: "config", findings: []Finding{{Title: "config finding"}}}

	report := Run(context.Background(), []Check{db, dbSchema, config}, Options{Only: []string{"db"}})

	if !db.ran || !dbSchema.ran {
		t.Fatalf("expected db checks to run, got db=%v db.schema=%v", db.ran, dbSchema.ran)
	}
	if config.ran {
		t.Fatal("expected config check to be skipped")
	}
	if len(report.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(report.Findings))
	}
	if len(report.Skipped) != 1 || report.Skipped[0] != "config" {
		t.Fatalf("expected config to be reported as skipped, got %v", report.Skipped)
	}
}

type recordingObserver struct {
	events []string
}

func (o *recordingObserver) CheckStarted(check Check) {
	o.events = append(o.events, "start:"+check.ID())
}

func (o *recordingObserver) CheckFinished(check Check, findings []Finding) {
	o.events = append(o.events, "finish:"+check.ID())
	for _, finding := range findings {
		if finding.Fixable && !finding.Fixed {
			o.events = append(o.events, "unfixed:"+finding.Title)
		}
	}
}

// An observer must see a skipped check not at all, and must see finished
// findings only after repairs have been applied to them.
func TestRunReportsProgressAroundEachCheck(t *testing.T) {
	repaired := &stubCheck{id: "stale", findings: []Finding{
		{Title: "orphaned row", Severity: SeverityWarn, Fix: func(context.Context) error { return nil }},
	}}
	skipped := &stubCheck{id: "config"}
	observer := &recordingObserver{}

	Run(context.Background(), []Check{repaired, skipped}, Options{
		Only:     []string{"stale"},
		Fix:      true,
		Observer: observer,
	})

	want := []string{"start:stale", "finish:stale"}
	if len(observer.events) != len(want) {
		t.Fatalf("observer events = %v, want %v", observer.events, want)
	}
	for i, event := range want {
		if observer.events[i] != event {
			t.Fatalf("observer events = %v, want %v", observer.events, want)
		}
	}
}

func TestRunStampsCheckIDAndFixability(t *testing.T) {
	check := &stubCheck{id: "stale", findings: []Finding{
		{Title: "plain"},
		{Title: "repairable", Fix: func(context.Context) error { return nil }},
	}}

	report := Run(context.Background(), []Check{check}, Options{})

	for _, finding := range report.Findings {
		if finding.Check != "stale" {
			t.Fatalf("expected check id to be stamped, got %q", finding.Check)
		}
	}
	if report.Findings[0].Fixable {
		t.Fatal("finding without a repair callback must not be fixable")
	}
	if !report.Findings[1].Fixable {
		t.Fatal("finding with a repair callback must be fixable")
	}
	if report.Findings[1].Fixed {
		t.Fatal("repairs must not run unless fixing is enabled")
	}
	if !report.HasFixable() {
		t.Fatal("expected report to advertise a fixable finding")
	}
}

func TestRunAppliesConfirmedRepairs(t *testing.T) {
	repaired := false
	declined := false
	check := &stubCheck{id: "stale", findings: []Finding{
		{Title: "yes", Severity: SeverityWarn, Fix: func(context.Context) error { repaired = true; return nil }},
		{Title: "no", Severity: SeverityWarn, Fix: func(context.Context) error { declined = true; return nil }},
	}}

	report := Run(context.Background(), []Check{check}, Options{
		Fix:     true,
		Confirm: func(finding Finding) bool { return finding.Title == "yes" },
	})

	if !repaired {
		t.Fatal("expected the confirmed repair to run")
	}
	if declined {
		t.Fatal("expected the declined repair to be skipped")
	}
	if !report.Findings[0].Fixed || report.Findings[0].Severity != SeverityOK {
		t.Fatalf("repaired finding should be marked fixed and healthy, got %+v", report.Findings[0])
	}
	if report.Findings[1].Fixed || report.Findings[1].Severity != SeverityWarn {
		t.Fatalf("declined finding should keep its severity, got %+v", report.Findings[1])
	}
}

func TestRunRecordsRepairFailures(t *testing.T) {
	check := &stubCheck{id: "db", findings: []Finding{
		{Title: "broken", Severity: SeverityWarn, Fix: func(context.Context) error { return errors.New("disk is full") }},
	}}

	report := Run(context.Background(), []Check{check}, Options{Fix: true})

	finding := report.Findings[0]
	if finding.Fixed {
		t.Fatal("a failed repair must not be reported as fixed")
	}
	if finding.Severity != SeverityError {
		t.Fatalf("expected a failed repair to escalate to error, got %s", finding.Severity)
	}
	if finding.FixError != "disk is full" {
		t.Fatalf("expected the repair error to be recorded, got %q", finding.FixError)
	}
}

func TestRunReportsCancellationWithoutRunningChecks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	check := &stubCheck{id: "db", findings: []Finding{{Title: "never"}}}

	report := Run(ctx, []Check{check}, Options{})

	if check.ran {
		t.Fatal("expected no check to run after cancellation")
	}
	if len(report.Findings) != 1 || report.Findings[0].Severity != SeverityWarn {
		t.Fatalf("expected one cancellation warning, got %+v", report.Findings)
	}
}

func TestReportSummaryAndWorst(t *testing.T) {
	report := Report{Findings: []Finding{
		{Severity: SeverityOK},
		{Severity: SeverityInfo},
		{Severity: SeverityWarn},
		{Severity: SeverityWarn},
	}}

	if got := report.Worst(); got != SeverityWarn {
		t.Fatalf("expected worst severity warn, got %s", got)
	}
	if got := report.Summary(); got != "2 warn, 1 info, 1 ok" {
		t.Fatalf("unexpected summary %q", got)
	}
	if got := (Report{}).Summary(); got != "no findings" {
		t.Fatalf("unexpected empty summary %q", got)
	}
}

func TestSeverityJSONRoundTrip(t *testing.T) {
	for _, severity := range []Severity{SeverityOK, SeverityInfo, SeverityWarn, SeverityError} {
		encoded, err := severity.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal %s: %v", severity, err)
		}
		var decoded Severity
		if err := decoded.UnmarshalJSON(encoded); err != nil {
			t.Fatalf("unmarshal %s: %v", encoded, err)
		}
		if decoded != severity {
			t.Fatalf("round trip changed %s into %s", severity, decoded)
		}
	}
}
