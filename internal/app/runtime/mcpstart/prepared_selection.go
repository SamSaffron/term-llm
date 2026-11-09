package mcpstart

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
)

// ConfigLoader is the config-loading dependency shared by preparation and
// startup. Nil uses Manager.LoadConfig. Overrides should load the given manager,
// not replace it, so the immutable run identity and last-good policy are retained.
type ConfigLoader func(*mcp.Manager) error

// PreparedSelection owns the single identity-bound manager used for selection
// validation/repair and startup. Active is safe for this run; Durable retains
// the requested value after an implicit load failure. Empty selections have no
// manager and Loaded is false. Preparing never starts servers.
type PreparedSelection struct {
	Manager  *mcp.Manager
	Active   string
	Durable  string
	Loaded   bool
	explicit bool
}

// PrepareSelection loads once, validates explicit selections before any startup,
// and repairs implicit selections using the same manager that Enable will use.
func PrepareSelection(identity, selection string, explicit bool, warnings io.Writer, load ConfigLoader) (*PreparedSelection, error) {
	prepared := &PreparedSelection{Durable: selection, explicit: explicit}
	if len(ParseServerList(selection)) == 0 {
		if explicit {
			prepared.Durable = ""
		}
		return prepared, nil
	}
	prepared.Manager = mcp.NewUnloadedManager(mcp.WithPolicyIdentity(identity))
	if load == nil {
		load = (*mcp.Manager).LoadConfig
	}
	if err := load(prepared.Manager); err != nil {
		if explicit {
			return nil, fmt.Errorf("load MCP config: %w", err)
		}
		if warnings != nil {
			fmt.Fprintf(warnings, "warning: failed to load MCP config: %v\n", err)
		}
		return prepared, nil
	}
	prepared.Loaded = true
	cleaned, err := CleanSelection(prepared.Manager, selection, explicit, warnings)
	if err != nil {
		return nil, err
	}
	prepared.Active, prepared.Durable = cleaned, cleaned
	return prepared, nil
}

// CanPersist permits successful repair or an explicit override (including an
// intentional empty selection). An implicit load failure never erases state.
func (p *PreparedSelection) CanPersist() bool { return p.Loaded || p.explicit }

// Enable reuses preparation's manager. Sampling/provider configuration can be
// supplied after provider/engine construction; no connection config is re-read.
// Enable and tool-call policy rechecks still use the manager's metadata cache.
func (p *PreparedSelection) Enable(ctx context.Context, engine *engine.Engine, warnings io.Writer, opts *MCPOptions) (*mcp.Manager, error) {
	if !p.Loaded || p.Active == "" {
		return nil, nil
	}
	if !p.explicit {
		// Refresh policy, not connection config: a restriction added between
		// preparation and startup must still repair an implicit selection.
		if err := p.Manager.RefreshPolicy(); err != nil {
			return nil, err
		}
		cleaned, _ := CleanSelection(p.Manager, p.Active, false, warnings)
		p.Active, p.Durable = cleaned, cleaned
		if p.Active == "" {
			return nil, nil
		}
	}
	manager, err := enableLoadedServers(ctx, p.Manager, p.Active, engine, warnings, opts)
	if err != nil {
		return nil, err
	}
	p.Active = strings.Join(manager.EnabledServers(), ",")
	p.Durable = p.Active
	return manager, nil
}
