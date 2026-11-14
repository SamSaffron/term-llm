package tooldiscovery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/mcp"
)

type collisionLocalTool struct{ testLocalTool }

func (*collisionLocalTool) ToolOrigin() core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginHost, ID: "discovery-collision-fixture"}
}

func discoveryCollisionTarget(t *testing.T, manager *mcp.Manager) mcp.CatalogTool {
	t.Helper()
	for _, tool := range manager.CatalogueSnapshot().Tools {
		if tool.OriginalName == "special_action" {
			return tool
		}
	}
	t.Fatal("missing collision fixture target")
	return mcp.CatalogTool{}
}

func TestRejectedCatalogueRegistrationIsNotDiscovered(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	target := discoveryCollisionTarget(t, manager)
	for _, mode := range []string{"eager", "deferred"} {
		t.Run(mode, func(t *testing.T) {
			provider := mockprovider.NewMockProvider("collision")
			eng := engine.NewEngine(provider, nil)
			local := &collisionLocalTool{testLocalTool{name: target.Name}}
			if err := eng.Tools().Register(local); err != nil {
				t.Fatal(err)
			}
			planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: mode, Strategy: "portable"}, manager, eng)
			if err != nil {
				t.Fatal(err)
			}
			req := core.Request{SessionID: "collision", Tools: []core.ToolSpec{local.Spec()}}
			if _, err := planner.BeginRun(context.Background(), provider, &req, "collision-run"); err != nil {
				t.Fatal(err)
			}
			if hasTool(req.Tools, target.Name) || planner.CanActivateDeferredTool(target.Name) {
				t.Fatal("rejected remote schema was advertised or force-activatable")
			}
			for _, message := range req.Messages {
				if strings.Contains(core.MessageText(message), target.OriginalName) {
					t.Fatal("deferred vocabulary notice advertised a rejected remote name")
				}
			}
			if _, ok := planner.authorizedCatalogueTools(eng, manager.CatalogueSnapshot().Tools)[target.Name]; ok {
				t.Fatal("name-only local occupant was treated as authorized MCP")
			}
			if _, tracked := planner.registered[target.Name]; tracked {
				t.Fatal("rejected registration was tracked as owned")
			}
			for _, result := range planner.index.search("special deterministic action", 25, nil) {
				if result.ID == target.Name {
					t.Fatal("rejected registration remained in the search index")
				}
			}
			loaded, already, _, _, _, _, err := planner.activate("collision-run", searchInput{ToolNames: []string{target.Name}})
			if err == nil || len(loaded) != 0 || len(already) != 0 {
				t.Fatalf("collision activation = %v, %v, %v", loaded, already, err)
			}
			planner.syncCatalogue(manager.CatalogueSnapshot())
			if installed, _ := eng.Tools().Get(target.Name); installed != local {
				t.Fatal("catalogue refresh replaced the local collision occupant")
			}
		})
	}
}

func TestDiscoveryCleanupRetainsReplacementOccupants(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	target := discoveryCollisionTarget(t, manager)
	for _, cleanup := range []string{"catalogue removal", "engine detach"} {
		t.Run(cleanup, func(t *testing.T) {
			provider := mockprovider.NewMockProvider("cleanup")
			eng := engine.NewEngine(provider, nil)
			planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, eng)
			if err != nil {
				t.Fatal(err)
			}
			req := core.Request{SessionID: "cleanup"}
			if _, err := planner.BeginRun(context.Background(), provider, &req, "cleanup-run"); err != nil {
				t.Fatal(err)
			}
			local := &collisionLocalTool{testLocalTool{name: target.Name}}
			search := &collisionLocalTool{testLocalTool{name: ToolSearchName}}
			for _, tool := range []core.Tool{local, search} {
				if err := eng.Tools().RegisterOverride(tool, "explicit host replacement"); err != nil {
					t.Fatal(err)
				}
			}
			if cleanup == "catalogue removal" {
				snapshot := *manager.CatalogueSnapshot()
				snapshot.Tools = nil
				planner.syncCatalogue(&snapshot)
			} else {
				planner.AttachEngine(engine.NewEngine(mockprovider.NewMockProvider("replacement"), nil))
			}
			for _, tool := range []core.Tool{local, search} {
				if installed, _ := eng.Tools().Get(tool.Spec().Name); installed != tool {
					t.Fatalf("cleanup removed another origin's %s occupant", tool.Spec().Name)
				}
			}
		})
	}
}

func TestNativeDiscoveryRechecksInstalledOriginAndSchema(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	target := discoveryCollisionTarget(t, manager)
	for _, replacement := range []string{"local origin", "different manager", "different schema"} {
		t.Run(replacement, func(t *testing.T) {
			provider := &nativeScriptProvider{supported: true}
			eng := engine.NewEngine(provider, nil)
			planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "native"}, manager, eng)
			if err != nil {
				t.Fatal(err)
			}
			req := core.Request{SessionID: "native-conflict"}
			if _, err := planner.BeginRun(context.Background(), provider, &req, "native-conflict-run"); err != nil {
				t.Fatal(err)
			}
			if _, err := planner.PrepareTurn(context.Background(), provider, &req, "native-conflict-run", 0, 5); err != nil {
				t.Fatal(err)
			}
			call := core.ToolDiscoveryCall{ID: "discover", Arguments: json.RawMessage(`{"tool_names":["special_action"]}`)}
			output, err := planner.ResolveNativeToolDiscovery(context.Background(), "native-conflict-run", call)
			if err != nil || len(output.Tools) != 1 {
				t.Fatalf("initial discovery = %+v, %v", output, err)
			}
			switch replacement {
			case "local origin":
				err = eng.Tools().RegisterOverride(&collisionLocalTool{testLocalTool{name: target.Name}}, "host replacement")
			case "different manager":
				other := mcp.NewManagerWithConfig(manager.Config())
				err = eng.Tools().RegisterDeferred(mcp.NewCatalogMCPTool(other, target))
			case "different schema":
				changed := target
				changed.Description = "another schema binding"
				err = eng.Tools().RegisterDeferred(mcp.NewCatalogMCPTool(manager, changed))
			}
			if err != nil {
				t.Fatal(err)
			}
			if planner.CanActivateDeferredTool(target.Name) || hasTool(planner.ActiveToolSpecs(req.SessionID), target.Name) {
				t.Fatal("stale active/forced surface accepted a replacement binding")
			}
			if _, err := planner.ResolveProviderToolCall("native-conflict-run", core.ToolCall{Namespace: target.Namespace, ChildName: target.ChildName}); err == nil {
				t.Fatal("native child resolved to the replacement occupant")
			}
			if _, err := planner.ResolveNativeToolDiscovery(context.Background(), "native-conflict-run", call); err == nil {
				t.Fatal("native discovery re-advertised the replaced capability")
			}
			replay := core.Request{SessionID: req.SessionID, Messages: []core.Message{{Role: core.RoleAssistant, Parts: []core.Part{
				{Type: core.PartDiscoveryCall, DiscoveryCall: &call},
				{Type: core.PartDiscoveryOutput, DiscoveryOutput: &output},
			}}}}
			planner.restoreNativeHistory(&replay, planner.stateKey(req.SessionID, ""))
			for _, message := range replay.Messages {
				for _, part := range message.Parts {
					if part.Type == core.PartDiscoveryOutput {
						t.Fatal("native replay retained a rejected replacement schema")
					}
				}
			}
		})
	}
}

func TestPortableSearchCollisionCannotAcquirePlannerAllowance(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "before admission", true: "after admission"}[late], func(t *testing.T) {
			provider := mockprovider.NewMockProvider("portable-conflict")
			eng := engine.NewEngine(provider, nil)
			local := &collisionLocalTool{testLocalTool{name: ToolSearchName}}
			if !late {
				if err := eng.Tools().Register(local); err != nil {
					t.Fatal(err)
				}
			}
			planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred", Strategy: "portable"}, manager, eng)
			if err != nil {
				t.Fatal(err)
			}
			req := core.Request{SessionID: "portable-conflict"}
			_, err = planner.BeginRun(context.Background(), provider, &req, "portable-conflict-run")
			if late {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := planner.PrepareTurn(context.Background(), provider, &req, "portable-conflict-run", 0, 5); err != nil {
					t.Fatal(err)
				}
				if !planner.AllowsPlannerTool("portable-conflict-run", ToolSearchName) {
					t.Fatal("valid planner tool lacks its run allowance")
				}
				if err := eng.Tools().RegisterOverride(local, "host replacement"); err != nil {
					t.Fatal(err)
				}
				if planner.AllowsPlannerTool("portable-conflict-run", ToolSearchName) {
					t.Fatal("colliding local tool inherited the planner exception")
				}
				_, err = planner.PrepareTurn(context.Background(), provider, &req, "portable-conflict-run", 1, 5)
			}
			if err == nil || !strings.Contains(err.Error(), "register portable tool_search") {
				t.Fatalf("portable collision error = %v", err)
			}
			if hasTool(req.Tools, ToolSearchName) || planner.AllowsPlannerTool("portable-conflict-run", ToolSearchName) || local.executed {
				t.Fatal("rejected portable tool was advertised, allowed, or executed")
			}
			if installed, _ := eng.Tools().Get(ToolSearchName); installed != local {
				t.Fatal("portable registration displaced a local collision occupant")
			}
		})
	}
}

func TestIdenticalCatalogueRefreshKeepsDiscoveryBindingGeneration(t *testing.T) {
	manager := startDiscoveryTestManager(t)
	defer manager.StopAll()
	eng := engine.NewEngine(mockprovider.NewMockProvider("refresh"), nil)
	planner, err := NewPlanner(config.ToolDiscoveryConfig{Mode: "deferred"}, manager, eng)
	if err != nil {
		t.Fatal(err)
	}
	target := discoveryCollisionTarget(t, manager)
	before, _ := eng.Tools().BindingGeneration(target.Name)
	planner.syncCatalogue(manager.CatalogueSnapshot())
	after, _ := eng.Tools().BindingGeneration(target.Name)
	if before != after || !planner.CanActivateDeferredTool(target.Name) {
		t.Fatalf("identical refresh changed binding %d -> %d or lost availability", before, after)
	}
}
