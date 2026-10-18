package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

// FragmentsListOptions configures `memory fragments list`.
type FragmentsListOptions struct {
	Env
	Since      time.Duration
	Limit      int
	FilterPath string
}

// FragmentsShowOptions configures `memory fragments show`.
type FragmentsShowOptions struct {
	Env
	JSON   bool
	NoPath bool
}

// FragmentsSourcesOptions configures `memory fragments sources`.
type FragmentsSourcesOptions struct {
	Env
	JSON bool
}

// FragmentsGCOptions configures `memory fragments gc`.
type FragmentsGCOptions struct {
	Env
	HalfLife     float64
	PreviewDecay bool
	Delete       bool
}

// FragmentsSyncOptions configures `memory fragments sync`.
type FragmentsSyncOptions struct {
	Env
	Dir string
}

// FragmentsAddOptions configures `memory fragments add`.
type FragmentsAddOptions struct {
	Env
	Content string
	Source  string
}

// FragmentsUpdateOptions configures `memory fragments update`.
type FragmentsUpdateOptions struct {
	Env
	Content string
}

// FragmentsDeleteOptions configures `memory fragments delete`.
type FragmentsDeleteOptions struct {
	Env
}

// FragmentsList runs `memory fragments list`.
func FragmentsList(opts FragmentsListOptions) error {
	env := opts.Env
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	listOpts := memorydb.ListOptions{
		Agent:      env.AgentFilter(),
		Limit:      opts.Limit,
		PathFilter: opts.FilterPath,
	}
	if opts.Since > 0 {
		cutoff := time.Now().Add(-opts.Since)
		listOpts.Since = &cutoff
	}

	fragments, err := store.ListFragments(context.Background(), listOpts)
	if err != nil {
		return err
	}
	if len(fragments) == 0 {
		fmt.Println("No memory fragments found.")
		return nil
	}

	pathCol := 4 // minimum width for "PATH" header
	for _, f := range fragments {
		if l := len(f.Path); l > pathCol {
			pathCol = l
		}
	}
	if pathCol > 84 {
		pathCol = 84
	}
	pathCol++ // one extra space of breathing room

	if env.AgentFilter() == "" {
		fmt.Printf("%-6s %-14s %-*s %-10s\n", "ID", "AGENT", pathCol, "PATH", "CREATED")
		fmt.Println(strings.Repeat("-", 6+1+14+1+pathCol+1+10))
		for _, f := range fragments {
			fmt.Printf("%-6d %-14s %-*s %-10s\n", f.RowID, f.Agent, pathCol, truncateString(f.Path, pathCol), env.FormatRelativeTime(f.CreatedAt))
		}
		return nil
	}

	fmt.Printf("%-6s %-*s %-10s\n", "ID", pathCol, "PATH", "CREATED")
	fmt.Println(strings.Repeat("-", 6+1+pathCol+1+10))
	for _, f := range fragments {
		fmt.Printf("%-6d %-*s %-10s\n", f.RowID, pathCol, truncateString(f.Path, pathCol), env.FormatRelativeTime(f.CreatedAt))
	}

	return nil
}

// FragmentsShow runs `memory fragments show`.
func FragmentsShow(opts FragmentsShowOptions, args []string) error {
	env := opts.Env
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	arg := strings.TrimSpace(args[0])
	if arg == "" {
		return fmt.Errorf("path or id cannot be empty")
	}

	var frag *memorydb.Fragment

	// Numeric argument: look up by rowid.
	if rowID, err := strconv.ParseInt(arg, 10, 64); err == nil {
		frag, err = store.GetFragmentByRowID(ctx, rowID)
		if err != nil {
			return err
		}
	} else if env.AgentFilter() != "" {
		frag, err = store.GetFragment(ctx, env.AgentFilter(), arg)
		if err != nil {
			return err
		}
	} else {
		frags, err := store.FindFragmentsByPath(ctx, arg)
		if err != nil {
			return err
		}
		if len(frags) > 1 {
			return fmt.Errorf("fragment path %q exists for multiple agents; rerun with --agent", arg)
		}
		if len(frags) == 1 {
			frag = &frags[0]
		}
	}

	if frag == nil {
		return fmt.Errorf("fragment not found: %s", arg)
	}

	return printFragment(opts, frag)
}

// FragmentsSources runs `memory fragments sources`.
func FragmentsSources(opts FragmentsSourcesOptions, args []string) error {
	env := opts.Env
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	arg := strings.TrimSpace(args[0])
	if arg == "" {
		return fmt.Errorf("path or id cannot be empty")
	}

	var frag *memorydb.Fragment

	if rowID, err := strconv.ParseInt(arg, 10, 64); err == nil {
		frag, err = store.GetFragmentByRowID(ctx, rowID)
		if err != nil {
			return err
		}
		if frag == nil {
			return fmt.Errorf("fragment not found: rowid %d", rowID)
		}
	} else {
		agent := env.AgentFilter()
		if agent == "" {
			return fmt.Errorf("--agent is required when looking up by path")
		}
		frag, err = store.GetFragment(ctx, agent, arg)
		if err != nil {
			return err
		}
		if frag == nil {
			return fmt.Errorf("fragment not found: %s", arg)
		}
	}

	sources, err := store.GetFragmentSources(ctx, frag.Agent, frag.Path)
	if err != nil {
		return err
	}

	if opts.JSON {
		out := struct {
			Agent   string                    `json:"agent"`
			Path    string                    `json:"path"`
			Sources []memorydb.FragmentSource `json:"sources"`
		}{
			Agent:   frag.Agent,
			Path:    frag.Path,
			Sources: sources,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(sources) == 0 {
		fmt.Println("No sources recorded for this fragment.")
		return nil
	}

	sessionCol := len("SESSION")
	turnsCol := len("TURNS")
	for _, source := range sources {
		if len(source.SessionID) > sessionCol {
			sessionCol = len(source.SessionID)
		}
		turns := fmt.Sprintf("[%d, %d)", source.TurnStart, source.TurnEnd)
		if len(turns) > turnsCol {
			turnsCol = len(turns)
		}
	}

	fmt.Printf("SOURCES for %s\n", frag.Path)
	fmt.Printf("%-*s %-*s %s\n", sessionCol, "SESSION", turnsCol, "TURNS", "MINED")
	for _, source := range sources {
		turns := fmt.Sprintf("[%d, %d)", source.TurnStart, source.TurnEnd)
		fmt.Printf("%-*s %-*s %s\n", sessionCol, source.SessionID, turnsCol, turns, source.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	return nil
}

func printFragment(opts FragmentsShowOptions, frag *memorydb.Fragment) error {
	if opts.JSON {
		type jsonFrag struct {
			ID          string     `json:"id"`
			RowID       int64      `json:"row_id"`
			Agent       string     `json:"agent"`
			Path        string     `json:"path"`
			Content     string     `json:"content"`
			Source      string     `json:"source"`
			CreatedAt   time.Time  `json:"created_at"`
			UpdatedAt   time.Time  `json:"updated_at"`
			AccessedAt  *time.Time `json:"accessed_at,omitempty"`
			AccessCount int        `json:"access_count"`
			DecayScore  float64    `json:"decay_score"`
			Pinned      bool       `json:"pinned"`
		}
		out := jsonFrag{
			ID:          frag.ID,
			RowID:       frag.RowID,
			Agent:       frag.Agent,
			Path:        frag.Path,
			Content:     frag.Content,
			Source:      frag.Source,
			CreatedAt:   frag.CreatedAt,
			UpdatedAt:   frag.UpdatedAt,
			AccessedAt:  frag.AccessedAt,
			AccessCount: frag.AccessCount,
			DecayScore:  frag.DecayScore,
			Pinned:      frag.Pinned,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if !opts.NoPath {
		fmt.Println(frag.Path)
	}
	fmt.Print(frag.Content)
	if !strings.HasSuffix(frag.Content, "\n") {
		fmt.Println()
	}
	return nil
}

// FragmentsGC runs `memory fragments gc`.
func FragmentsGC(opts FragmentsGCOptions) error {
	env := opts.Env
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	agent := env.AgentFilter()

	if opts.PreviewDecay {
		if opts.HalfLife <= 0 {
			return fmt.Errorf("--half-life must be > 0")
		}
		count, err := store.CountDecayCandidates(ctx, agent, opts.HalfLife, memorydb.DefaultDecayGCThreshold)
		if err != nil {
			return err
		}
		fmt.Printf("gc: age-based decay preview would mark %d fragments below %.2f (non-destructive; no scores changed)\n", count, memorydb.DefaultDecayGCThreshold)
		return nil
	}

	count, err := store.CountGCCandidates(ctx, agent)
	if err != nil {
		return err
	}
	if env.DryRun || !opts.Delete {
		fmt.Printf("gc: would remove %d fragments (based on stored decay scores; no recalculation). Pass --delete to delete.\n", count)
		return nil
	}

	removed, err := store.GCFragments(ctx, agent)
	if err != nil {
		return err
	}
	fmt.Printf("gc: removed %d fragments (no decay recalculation)\n", removed)
	return nil
}

// FragmentsSync runs `memory fragments sync`.
func FragmentsSync(opts FragmentsSyncOptions) error {
	env := opts.Env
	agent := env.AgentFilter()
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return fmt.Errorf("--dir is required")
	}

	// Resolve to absolute path
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	if _, err := os.Stat(absDir); err != nil {
		return fmt.Errorf("dir not found: %w", err)
	}

	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	var created, updated, skipped, errors int

	err = filepath.Walk(absDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}

		// Compute DB path: "fragments/<relative-from-dir>"
		rel, err := filepath.Rel(absDir, path)
		if err != nil {
			return err
		}
		dbPath := "fragments/" + filepath.ToSlash(rel)

		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error reading %s: %v\n", rel, err)
			errors++
			return nil
		}
		contentStr := string(content)
		if strings.TrimSpace(contentStr) == "" {
			skipped++
			return nil
		}

		existing, err := store.GetFragment(ctx, agent, dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error checking %s: %v\n", dbPath, err)
			errors++
			return nil
		}

		if existing != nil {
			if existing.Content == contentStr {
				// Identical — nothing to do
				skipped++
				return nil
			}
			// Content changed — update
			if env.DryRun {
				fmt.Printf("  would update: %s\n", dbPath)
				updated++
				return nil
			}
			_, err = store.UpdateFragment(ctx, agent, dbPath, contentStr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  error updating %s: %v\n", dbPath, err)
				errors++
				return nil
			}
			fmt.Printf("  updated: %s\n", dbPath)
			updated++
			return nil
		}

		// New fragment — create it, using file mtime for timestamps
		mtime := info.ModTime()
		frag := &memorydb.Fragment{
			Agent:     agent,
			Path:      dbPath,
			Content:   contentStr,
			Source:    "sync",
			CreatedAt: mtime,
			UpdatedAt: mtime,
		}
		if env.DryRun {
			fmt.Printf("  would create: %s\n", dbPath)
			created++
			return nil
		}
		if err := store.CreateFragment(ctx, frag); err != nil {
			fmt.Fprintf(os.Stderr, "  error creating %s: %v\n", dbPath, err)
			errors++
			return nil
		}
		fmt.Printf("  created: %s\n", dbPath)
		created++
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk error: %w", err)
	}

	action := "sync"
	if env.DryRun {
		action = "dry-run"
	}
	fmt.Printf("\n%s complete: %d created, %d updated, %d skipped, %d errors\n",
		action, created, updated, skipped, errors)
	return nil
}

// --- add / update / delete ---

// FragmentsAdd runs `memory fragments add`.
func FragmentsAdd(opts FragmentsAddOptions, args []string) error {
	env := opts.Env
	agent := env.AgentFilter()
	if agent == "" {
		return fmt.Errorf("--agent is required for add/update/delete")
	}
	fragPath := strings.TrimSpace(args[0])
	content, err := readFragmentContent(opts.Content)
	if err != nil {
		return err
	}
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		source = "manual"
	}
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.CreateFragment(context.Background(), &memorydb.Fragment{
		Agent:   agent,
		Path:    fragPath,
		Content: content,
		Source:  source,
	}); err != nil {
		if isUniqueConstraintError(err) {
			return fmt.Errorf("fragment already exists: %s — use 'update' instead", fragPath)
		}
		return err
	}
	fmt.Printf("created: %s\n", fragPath)
	return nil
}

// FragmentsUpdate runs `memory fragments update`.
func FragmentsUpdate(opts FragmentsUpdateOptions, args []string) error {
	env := opts.Env
	arg := strings.TrimSpace(args[0])
	content, err := readFragmentContent(opts.Content)
	if err != nil {
		return err
	}
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()

	// Numeric arg → update by rowid (no --agent required).
	if rowID, err := strconv.ParseInt(arg, 10, 64); err == nil {
		updated, err := store.UpdateFragmentByRowID(ctx, rowID, content)
		if err != nil {
			return err
		}
		if !updated {
			return fmt.Errorf("fragment not found or content unchanged: rowid %d", rowID)
		}
		fmt.Printf("updated: rowid %d\n", rowID)
		return nil
	}

	// Path arg → require --agent.
	agent := env.AgentFilter()
	if agent == "" {
		return fmt.Errorf("--agent is required for add/update/delete")
	}
	updated, err := store.UpdateFragment(ctx, agent, arg, content)
	if err != nil {
		return err
	}
	if !updated {
		return fmt.Errorf("fragment not found: %s — use 'add' to create it", arg)
	}
	fmt.Printf("updated: %s\n", arg)
	return nil
}

// FragmentsDelete runs `memory fragments delete`.
func FragmentsDelete(opts FragmentsDeleteOptions, args []string) error {
	env := opts.Env
	arg := strings.TrimSpace(args[0])
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()

	// Numeric arg → delete by rowid (no --agent required).
	if rowID, err := strconv.ParseInt(arg, 10, 64); err == nil {
		deleted, err := store.DeleteFragmentByRowID(ctx, rowID)
		if err != nil {
			return err
		}
		if !deleted {
			return fmt.Errorf("fragment not found: rowid %d", rowID)
		}
		fmt.Printf("deleted: rowid %d\n", rowID)
		return nil
	}

	// Path arg → require --agent.
	agent := env.AgentFilter()
	if agent == "" {
		return fmt.Errorf("--agent is required for add/update/delete")
	}
	deleted, err := store.DeleteFragment(ctx, agent, arg)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("fragment not found: %s", arg)
	}
	fmt.Printf("deleted: %s\n", arg)
	return nil
}

// readFragmentContent returns s if non-empty, otherwise reads all of stdin.
func readFragmentContent(s string) (string, error) {
	if s != "" {
		return s, nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	return string(data), nil
}
