package cmd

import (
	"testing"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeJobsRunnerOptionsUseCapturedResolvedApproval(t *testing.T) {
	oldFlags := serveFlags
	serveFlags = ServeOptions{Provider: "global-provider", Auto: false, Yolo: true}
	t.Cleanup(func() { serveFlags = oldFlags })

	resolved := resolvedApprovalMode{Mode: tools.ModeAuto, Source: approvalModeSourceSurfaceConfig}
	source := serveRuntimeFixtureOptions()
	source.Provider, source.Tools, source.SystemMessage, source.Debug = "captured-provider", "read_file", "captured-system", true
	source.Search = true
	sessionStore := appruntime.SessionStoreOverrides{NoSession: true, DBPath: "sessions.db"}
	paths := appmemory.Paths{DBPath: "memory.db", DefaultDBPath: "default.db"}
	opts := appserve.ServeJobsRunnerOptions(source, true, resolved, sessionStore, paths)
	if opts.ApprovalMode != tools.ModeAuto || opts.ApprovalSource != approvalModeSourceSurfaceConfig || !opts.ApprovalModeSet || !opts.ApprovalHeadless {
		t.Fatalf("serve jobs approval options = mode:%v source:%s set:%t headless:%t", opts.ApprovalMode, opts.ApprovalSource, opts.ApprovalModeSet, opts.ApprovalHeadless)
	}
	if opts.SessionStore != sessionStore || opts.MemoryPaths != paths {
		t.Fatalf("serve jobs runtime inputs = %#v / %#v, want %#v / %#v", opts.SessionStore, opts.MemoryPaths, sessionStore, paths)
	}
	if opts.Provider != source.Provider || opts.Tools != source.Tools || opts.SystemMessage != source.SystemMessage || !opts.Search || !opts.Debug || !opts.DebugRaw {
		t.Fatalf("serve jobs explicit inputs = %#v", opts)
	}
}
