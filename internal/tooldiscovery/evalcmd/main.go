package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tooldiscovery"
	"github.com/samsaffron/term-llm/internal/tooldiscovery/evalfixture"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: evalcmd server|aggregate-server|generate-config|catalogue ...")
	}
	switch os.Args[1] {
	case "server":
		server(os.Args[2:])
	case "aggregate-server":
		aggregateServer(os.Args[2:])
	case "init":
		initialize(os.Args[2:])
	case "generate-config":
		generateConfig(os.Args[2:])
	case "catalogue":
		catalogue(os.Args[2:])
	case "retrieval":
		retrieval(os.Args[2:])
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func initialize(args []string) {
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	root := flags.String("root", "", "disposable evaluation workspace")
	_ = flags.Parse(args)
	if *root == "" {
		fatalf("init requires --root")
	}
	if err := evalfixture.InitializeWorkspace(*root); err != nil {
		fatalf("initialize fixtures: %v", err)
	}
}

func server(args []string) {
	flags := flag.NewFlagSet("server", flag.ExitOnError)
	name := flags.String("name", "", "evaluation server name")
	root := flags.String("root", "", "disposable evaluation workspace")
	limit := flags.Int("limit", 20, "number of tools to expose")
	_ = flags.Parse(args)
	if *name == "" || *root == "" {
		fatalf("server requires --name and --root")
	}
	if err := evalfixture.RunServer(context.Background(), *name, *root, *limit); err != nil {
		fatalf("run server: %v", err)
	}
}

func aggregateServer(args []string) {
	flags := flag.NewFlagSet("aggregate-server", flag.ExitOnError)
	root := flags.String("root", "", "disposable evaluation workspace")
	_ = flags.Parse(args)
	if *root == "" {
		fatalf("aggregate-server requires --root")
	}
	if err := evalfixture.RunAggregateServer(context.Background(), *root); err != nil {
		fatalf("run aggregate server: %v", err)
	}
}

func generateConfig(args []string) {
	flags := flag.NewFlagSet("generate-config", flag.ExitOnError)
	root := flags.String("root", "", "disposable evaluation workspace")
	binary := flags.String("binary", "", "absolute evalcmd binary path")
	size := flags.Int("size", 200, "total catalogue size")
	aggregate := flags.Bool("aggregate", false, "expose all 200 tools from one MCP server")
	output := flags.String("output", "", "mcp.json output path")
	_ = flags.Parse(args)
	if *root == "" || *binary == "" || *output == "" {
		fatalf("generate-config requires --root, --binary, and --output")
	}
	cfg, err := buildConfig(*root, *binary, *size, *aggregate)
	if err != nil {
		fatalf("generate config: %v", err)
	}
	writeJSON(*output, cfg)
}

func buildConfig(root, binary string, size int, aggregate bool) (*mcp.Config, error) {
	if aggregate {
		if size != 200 {
			return nil, fmt.Errorf("aggregate mode exposes exactly 200 tools, got size %d", size)
		}
		return &mcp.Config{Servers: map[string]mcp.ServerConfig{
			"federation": {Command: binary, Args: []string{"aggregate-server", "--root", root}},
		}}, nil
	}
	limits, err := evalfixture.ProfileLimits(size)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	cfg := &mcp.Config{Servers: make(map[string]mcp.ServerConfig)}
	for _, domain := range evalfixture.Domains() {
		limit := limits[domain.Name]
		if limit == 0 {
			continue
		}
		cfg.Servers[domain.Name] = mcp.ServerConfig{
			Command: binary,
			Args:    []string{"server", "--name", domain.Name, "--root", root, "--limit", fmt.Sprint(limit)},
		}
	}
	return cfg, nil
}

func catalogue(args []string) {
	flags := flag.NewFlagSet("catalogue", flag.ExitOnError)
	configPath := flags.String("mcp-config", "", "mcp.json path")
	output := flags.String("output", "", "manifest output path")
	timeout := flags.Duration("timeout", 45*time.Second, "startup timeout")
	_ = flags.Parse(args)
	if *configPath == "" || *output == "" {
		fatalf("catalogue requires --mcp-config and --output")
	}
	cfg, err := mcp.LoadConfigFromPath(*configPath)
	if err != nil {
		fatalf("load config: %v", err)
	}
	manager := mcp.NewManagerWithConfig(cfg)
	defer manager.StopAll()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	for _, name := range cfg.ServerNames() {
		if err := manager.Enable(ctx, name); err != nil {
			fatalf("enable %s: %v", name, err)
		}
	}
	for {
		ready := 0
		for _, name := range cfg.ServerNames() {
			status, statusErr := manager.ServerStatus(name)
			switch status {
			case mcp.StatusReady:
				ready++
			case mcp.StatusFailed:
				fatalf("server %s failed: %v", name, statusErr)
			}
		}
		if ready == len(cfg.Servers) {
			break
		}
		select {
		case <-ctx.Done():
			fatalf("wait for servers: %v", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	snapshot := manager.CatalogueSnapshot()
	writeJSON(*output, snapshot)
}

type taskManifest struct {
	Tasks []struct {
		ID         string   `json:"id"`
		Prompt     string   `json:"prompt"`
		Required   []string `json:"required_tools"`
		MaxResults int      `json:"max_results,omitempty"`
	} `json:"tasks"`
}

func retrieval(args []string) {
	flags := flag.NewFlagSet("retrieval", flag.ExitOnError)
	manifestPath := flags.String("manifest", "", "catalogue manifest JSON")
	tasksPath := flags.String("tasks", "", "scored task manifest JSON")
	output := flags.String("output", "", "retrieval results JSON")
	_ = flags.Parse(args)
	if *manifestPath == "" || *tasksPath == "" || *output == "" {
		fatalf("retrieval requires --manifest, --tasks, and --output")
	}
	var catalogue mcp.CatalogueSnapshot
	readJSON(*manifestPath, &catalogue)
	var tasks taskManifest
	readJSON(*tasksPath, &tasks)
	type caseResult struct {
		ID       string   `json:"id"`
		Top5     []string `json:"top5"`
		Results  []string `json:"results"`
		Required []string `json:"required"`
		Hits     int      `json:"hits"`
	}
	results := make([]caseResult, 0, len(tasks.Tasks))
	totalRequired, totalHits := 0, 0
	for _, task := range tasks.Tasks {
		limit := task.MaxResults
		if limit < 5 {
			limit = 5
		}
		search := tooldiscovery.SearchCatalogue(catalogue.Tools, task.Prompt, limit)
		candidateNames := make([]string, 0, len(search))
		set := make(map[string]bool, min(5, len(search)))
		for i, result := range search {
			candidateNames = append(candidateNames, result.ID)
			if i < 5 {
				set[result.ID] = true
			}
		}
		top := append([]string(nil), candidateNames[:min(5, len(candidateNames))]...)
		hits := 0
		for _, required := range task.Required {
			if set[required] {
				hits++
			}
		}
		totalRequired += len(task.Required)
		totalHits += hits
		results = append(results, caseResult{ID: task.ID, Top5: top, Results: candidateNames, Required: task.Required, Hits: hits})
	}
	recall := 0.0
	if totalRequired > 0 {
		recall = float64(totalHits) / float64(totalRequired)
	}
	writeJSON(*output, map[string]any{
		"recall_at_5": recall,
		"hits":        totalHits, "required": totalRequired,
		"cases": results,
	})
}

func readJSON(path string, destination any) {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		fatalf("decode %s: %v", path, err)
	}
}

func writeJSON(path string, value any) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		fatalf("create output dir: %v", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatalf("encode JSON: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		fatalf("write %s: %v", path, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mcp discovery eval: "+format+"\n", args...)
	os.Exit(1)
}
