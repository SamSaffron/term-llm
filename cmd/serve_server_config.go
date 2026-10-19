package cmd

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/widgets"
)

type serveServerConfig struct {
	host                    string
	port                    int
	requireAuth             bool
	token                   string
	ui                      bool
	api                     bool
	suppressServerTools     bool
	verbose                 bool
	debug                   bool // CLI flag verbatim; raw implies debug, so readers derive it (liveDebugOptions)
	debugRaw                bool
	basePath                string // e.g. "/ui" or "/chat", always without trailing slash
	publicURL               string // explicit browser-visible origin + optional prefix for OAuth callbacks
	uiTitle                 string
	locationSharingDisabled bool
	sidebarSessions         []string
	agentName               string
	agentNames              []string
	corsOrigins             []string
	filesDir                string   // opt-in directory for serving arbitrary files (videos, PDFs, etc)
	writeDirs               []string // tool write-dirs (CLI + config); tool-reported files inside these are trusted sources for ensureFileServeable
	responseTimeout         time.Duration
	// hubURL/hubNodeID/hubNodeName describe the term-llm Hub this node
	// belongs to. When hubURL is set, the web UI gets window.TERM_LLM_HUB and
	// renders a Back to Hub link. The hub proxy injects the same context
	// server-side, so these are only needed when a node should be hub-aware
	// even when opened directly.
	hubURL      string
	hubNodeID   string
	hubNodeName string
}

// uiRoute returns the base-path with trailing slash, e.g. "/ui/" or "/chat/".
func (c serveServerConfig) uiRoute() string { return c.basePath + "/" }

// imagesRoute returns the images sub-route, e.g. "/ui/images/" or "/chat/images/".
func (c serveServerConfig) imagesRoute() string { return c.basePath + "/images/" }

// mediaRoute returns the durable media sub-route.
func (c serveServerConfig) mediaRoute() string { return c.basePath + "/media/" }

// uploadsRoute returns the first-party upload download sub-route, e.g.
// "/ui/uploads/" or "/chat/uploads/".
func (c serveServerConfig) uploadsRoute() string { return c.basePath + "/uploads/" }

// filesRoute returns the files sub-route, e.g. "/ui/files/" or "/chat/files/".
func (c serveServerConfig) filesRoute() string { return c.basePath + "/files/" }

// agentAvailable reports whether a first-party request may select name.
func (c serveServerConfig) agentAvailable(name string) bool {
	if name != "" && name == c.agentName {
		return true
	}
	for _, available := range c.agentNames {
		if name == available {
			return true
		}
	}
	return false
}

// resolveFilesDir returns the files-dir from the flag if set, otherwise from config.
func resolveFilesDir(flagVal string, cfg *config.Config) string {
	if flagVal != "" {
		return flagVal
	}
	return cfg.Serve.FilesDir
}

// serveWidgetsEnabled reports whether this process should discover and serve widgets.
func serveWidgetsEnabled(hasWeb, disabled bool) bool {
	return hasWeb && !disabled
}

// resolveWidgetsDir returns the widgets directory, defaulting to ~/.config/term-llm/widgets.
// setupServeWidgets builds the widgets manager when widgets are enabled and
// installs its stop signal. The returned stop function is always safe to defer.
func setupServeWidgets(ctx context.Context, cfg *config.Config, hasWeb bool, opts *ServeOptions) (*widgets.Manager, func(), error) {
	if !serveWidgetsEnabled(hasWeb, opts.DisableWidgets) {
		return nil, func() {}, nil
	}
	wDir, err := resolveWidgetsDir(opts.WidgetsDir, cfg)
	if err != nil {
		return nil, nil, err
	}
	mgr := widgets.NewManager(wDir, opts.BasePath)
	stop := installWidgetStopSignal(ctx, mgr)
	log.Printf("widgets enabled, dir: %s", wDir)
	return mgr, stop, nil
}

func resolveWidgetsDir(flagVal string, cfg *config.Config) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if cfg.Serve.WidgetsDir != "" {
		return cfg.Serve.WidgetsDir, nil
	}
	cfgDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve widgets dir: %w", err)
	}
	return cfgDir + "/widgets", nil
}

func resolveServeResponseTimeout(flagSet bool, flagVal time.Duration, configVal string) (time.Duration, error) {
	if flagSet {
		if flagVal <= 0 {
			return 0, fmt.Errorf("invalid --response-timeout %s (must be > 0)", flagVal)
		}
		return flagVal, nil
	}
	if strings.TrimSpace(configVal) == "" {
		return defaultServeRequestTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(configVal))
	if err != nil {
		return 0, fmt.Errorf("invalid serve.response_timeout %q (use a Go duration like 30m or 1h): %w", configVal, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("invalid serve.response_timeout %q (must be > 0)", configVal)
	}
	return timeout, nil
}

func (s *serveServer) responseTimeout() time.Duration {
	if s == nil || s.cfg.responseTimeout <= 0 {
		return defaultServeRequestTimeout
	}
	return s.cfg.responseTimeout
}

// resolveServeWriteDirs returns the merged effective write-dirs for the serve runtime,
// preserving order and de-duplicating.
func resolveServeWriteDirs(cliWriteDirs []string, cfg *config.Config) []string {
	seen := make(map[string]struct{}, len(cliWriteDirs)+len(cfg.Tools.WriteDirs))
	var out []string
	for _, d := range cfg.Tools.WriteDirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	for _, d := range cliWriteDirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

func normalizeServePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("must be an http(s) URL with a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must not contain a query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func normalizeBasePath(raw string) (string, error) { return appserve.NormalizeBasePath(raw) }
