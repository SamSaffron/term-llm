// Package doctor implements offline health checks for a term-llm install.
//
// Checks are read-only by default. A check may attach a repair callback to a
// finding; the runner invokes it only when fixing is explicitly enabled and the
// optional confirmation hook approves it.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Severity ranks a finding from healthy to broken.
type Severity int

const (
	// SeverityOK marks a check that passed.
	SeverityOK Severity = iota
	// SeverityInfo marks a neutral observation that needs no action.
	SeverityInfo
	// SeverityWarn marks something degraded but still working.
	SeverityWarn
	// SeverityError marks something broken that needs attention.
	SeverityError
)

func (s Severity) String() string {
	switch s {
	case SeverityOK:
		return "ok"
	case SeverityInfo:
		return "info"
	case SeverityWarn:
		return "warn"
	case SeverityError:
		return "error"
	default:
		return "unknown"
	}
}

// MarshalJSON renders the severity as its lowercase label.
func (s Severity) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON parses a lowercase severity label.
func (s *Severity) UnmarshalJSON(data []byte) error {
	var label string
	if err := json.Unmarshal(data, &label); err != nil {
		return err
	}
	switch label {
	case "ok":
		*s = SeverityOK
	case "info":
		*s = SeverityInfo
	case "warn":
		*s = SeverityWarn
	case "error":
		*s = SeverityError
	default:
		return fmt.Errorf("unknown severity %q", label)
	}
	return nil
}

// Finding is a single observation produced by a check.
type Finding struct {
	Check    string   `json:"check"`
	Title    string   `json:"title"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail,omitempty"`
	Remedy   string   `json:"remedy,omitempty"`
	Fixable  bool     `json:"fixable"`
	Fixed    bool     `json:"fixed,omitempty"`
	FixError string   `json:"fix_error,omitempty"`

	// Fix repairs this finding. It must be safe to skip: the runner calls it
	// only when fixing is enabled and confirmed.
	Fix func(context.Context) error `json:"-"`
}

// Check is one independently runnable group of related diagnostics.
type Check interface {
	// ID is a stable dotted identifier such as "db" or "config".
	ID() string
	// Title is a short human-readable label.
	Title() string
	// Run executes the check. Implementations must not mutate user state.
	Run(ctx context.Context) []Finding
}

// Observer receives progress events while checks run, so a caller can report
// work in progress before the whole run finishes. Every Started call is
// followed by exactly one Finished call for the same check.
type Observer interface {
	// CheckStarted fires immediately before a check runs.
	CheckStarted(check Check)
	// CheckFinished fires once a check has run and its confirmed repairs have
	// been applied. Findings are final, including fix outcomes.
	CheckFinished(check Check, findings []Finding)
}

// Options controls a doctor run.
type Options struct {
	// Only restricts execution to check IDs matching one of these values,
	// either exactly or as a dotted prefix. Empty runs every check.
	Only []string
	// Fix enables repair callbacks attached to findings.
	Fix bool
	// Confirm is consulted before each repair when set. A nil hook approves
	// every repair.
	Confirm func(Finding) bool
	// Observer reports progress as checks start and finish. Optional.
	Observer Observer
}

// Report is the aggregated outcome of a doctor run.
type Report struct {
	Findings []Finding `json:"findings"`
	Ran      []string  `json:"ran"`
	Skipped  []string  `json:"skipped,omitempty"`
}

// Run executes the selected checks in order and applies confirmed repairs.
func Run(ctx context.Context, checks []Check, opts Options) Report {
	report := Report{Findings: []Finding{}}
	for _, check := range checks {
		if check == nil {
			continue
		}
		if !selected(check.ID(), opts.Only) {
			report.Skipped = append(report.Skipped, check.ID())
			continue
		}
		report.Ran = append(report.Ran, check.ID())
		findings := runCheck(ctx, check, opts)
		report.Findings = append(report.Findings, findings...)
	}
	return report
}

// runCheck runs one check, applies its confirmed repairs, and reports progress
// around the whole unit of work so an observer never sees partial findings.
func runCheck(ctx context.Context, check Check, opts Options) []Finding {
	if opts.Observer != nil {
		opts.Observer.CheckStarted(check)
	}
	var findings []Finding
	if err := ctx.Err(); err != nil {
		findings = append(findings, Finding{
			Check:    check.ID(),
			Title:    "check cancelled",
			Severity: SeverityWarn,
			Detail:   err.Error(),
		})
	} else {
		for _, finding := range check.Run(ctx) {
			if finding.Check == "" {
				finding.Check = check.ID()
			}
			finding.Fixable = finding.Fix != nil
			applyFix(ctx, &finding, opts)
			findings = append(findings, finding)
		}
	}
	if opts.Observer != nil {
		opts.Observer.CheckFinished(check, findings)
	}
	return findings
}

func applyFix(ctx context.Context, finding *Finding, opts Options) {
	if !opts.Fix || finding.Fix == nil {
		return
	}
	if opts.Confirm != nil && !opts.Confirm(*finding) {
		return
	}
	if err := finding.Fix(ctx); err != nil {
		finding.FixError = err.Error()
		finding.Severity = SeverityError
		return
	}
	finding.Fixed = true
	finding.Severity = SeverityOK
}

// selected reports whether an ID matches any selector exactly or by dotted prefix.
func selected(id string, only []string) bool {
	if len(only) == 0 {
		return true
	}
	for _, want := range only {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if id == want || strings.HasPrefix(id, want+".") {
			return true
		}
	}
	return false
}

// Counts tallies findings by severity.
func (r Report) Counts() map[Severity]int {
	counts := make(map[Severity]int, 4)
	for _, finding := range r.Findings {
		counts[finding.Severity]++
	}
	return counts
}

// Worst returns the highest severity in the report.
func (r Report) Worst() Severity {
	return WorstSeverity(r.Findings)
}

// WorstSeverity returns the highest severity among findings. An empty slice is
// healthy, so it reports SeverityOK.
func WorstSeverity(findings []Finding) Severity {
	worst := SeverityOK
	for _, finding := range findings {
		if finding.Severity > worst {
			worst = finding.Severity
		}
	}
	return worst
}

// HasFixable reports whether any unfixed finding carries a repair callback.
func (r Report) HasFixable() bool {
	for _, finding := range r.Findings {
		if finding.Fixable && !finding.Fixed {
			return true
		}
	}
	return false
}

// Summary renders a one-line severity tally.
func (r Report) Summary() string {
	counts := r.Counts()
	parts := make([]string, 0, 4)
	for _, severity := range []Severity{SeverityError, SeverityWarn, SeverityInfo, SeverityOK} {
		if counts[severity] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[severity], severity))
		}
	}
	if len(parts) == 0 {
		return "no findings"
	}
	return strings.Join(parts, ", ")
}

// CheckIDs returns the sorted IDs of the supplied checks.
func CheckIDs(checks []Check) []string {
	ids := make([]string, 0, len(checks))
	for _, check := range checks {
		if check != nil {
			ids = append(ids, check.ID())
		}
	}
	sort.Strings(ids)
	return ids
}
