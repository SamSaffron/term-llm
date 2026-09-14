package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"sort"

	"github.com/samsaffron/term-llm/internal/config"
)

// BinarySpec describes an external command term-llm may shell out to.
type BinarySpec struct {
	// Name is the command looked up on PATH.
	Name string
	// Missing is the severity reported when the command is absent.
	Missing Severity
	// Purpose explains what stops working without it.
	Purpose string
	// Install is an optional hint for obtaining the command.
	Install string
}

// BinaryCheck reports which external commands are available. Provider CLIs are
// only required when a provider that shells out to them is configured.
type BinaryCheck struct {
	// Config is the loaded user config. A nil config is loaded on demand.
	Config *config.Config
	// Binaries overrides the default list.
	Binaries []BinarySpec
}

// ID implements Check.
func (c *BinaryCheck) ID() string { return "binaries" }

// Title implements Check.
func (c *BinaryCheck) Title() string { return "External commands" }

func defaultBinaries() []BinarySpec {
	return []BinarySpec{
		{Name: "git", Missing: SeverityError, Purpose: "workspace detection, diffs, commits, worktrees, and file-change tracking"},
		{Name: "sh", Missing: SeverityError, Purpose: "the shell tool and $(...) config value resolution"},
		{Name: "rg", Missing: SeverityWarn, Purpose: "fast code search; the grep tool falls back to a slower built-in scan", Install: "install ripgrep"},
		{Name: "ffprobe", Missing: SeverityInfo, Purpose: "duration checks for audio and video transcription", Install: "install ffmpeg"},
		{Name: "node", Missing: SeverityInfo, Purpose: "running the bundled JavaScript tests"},
	}
}

// providerBinaries maps provider types that shell out to their CLI.
var providerBinaries = map[config.ProviderType]string{
	config.ProviderTypeClaudeBin: "claude",
	config.ProviderTypeGrokBin:   "grok",
	config.ProviderTypeCursorBin: "cursor-agent",
	config.ProviderTypeAgyBin:    "agy",
}

// Run implements Check.
func (c *BinaryCheck) Run(ctx context.Context) []Finding {
	specs := c.Binaries
	if specs == nil {
		specs = defaultBinaries()
		specs = append(specs, configuredProviderBinaries(c.configOrLoad())...)
	}

	findings := make([]Finding, 0, len(specs))
	for _, spec := range specs {
		path, err := exec.LookPath(spec.Name)
		if err == nil {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("%s: found", spec.Name),
				Severity: SeverityOK,
				Detail:   path,
			})
			continue
		}
		remedy := spec.Install
		if remedy == "" {
			remedy = fmt.Sprintf("install %s and make sure it is on PATH", spec.Name)
		}
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("%s: not found on PATH", spec.Name),
			Severity: spec.Missing,
			Detail:   "needed for " + spec.Purpose,
			Remedy:   remedy,
		})
	}
	return findings
}

func (c *BinaryCheck) configOrLoad() *config.Config {
	if c.Config != nil {
		return c.Config
	}
	cfg, _ := config.Load()
	return cfg
}

// configuredProviderBinaries returns specs for CLIs required by configured
// providers, de-duplicated across providers that share one CLI.
func configuredProviderBinaries(cfg *config.Config) []BinarySpec {
	if cfg == nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := map[string]bool{}
	var specs []BinarySpec
	for _, name := range names {
		binary, ok := providerBinaries[config.InferProviderType(name, cfg.Providers[name].Type)]
		if !ok || seen[binary] {
			continue
		}
		seen[binary] = true
		specs = append(specs, BinarySpec{
			Name:    binary,
			Missing: SeverityError,
			Purpose: fmt.Sprintf("provider %q, which runs the %s CLI", name, binary),
		})
	}
	return specs
}
