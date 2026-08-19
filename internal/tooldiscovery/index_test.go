package tooldiscovery

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/mcp"
)

func TestResolveModeBoundaries(t *testing.T) {
	for _, count := range []int{0, 6, 24} {
		if got := ResolveMode(ModeAuto, 24, count, false); got != ModeEager {
			t.Fatalf("auto count %d = %s, want eager", count, got)
		}
	}
	for _, count := range []int{25, 42, 200} {
		if got := ResolveMode(ModeAuto, 24, count, false); got != ModeDeferred {
			t.Fatalf("auto count %d = %s, want deferred", count, got)
		}
	}
	if got := ResolveMode(ModeAuto, 0, 0, false); got != ModeEager {
		t.Fatalf("threshold zero empty = %s, want eager", got)
	}
	if got := ResolveMode(ModeAuto, 0, 1, false); got != ModeDeferred {
		t.Fatalf("threshold zero one = %s, want deferred", got)
	}
	if got := ResolveMode(ModeDeferred, 0, 200, true); got != ModeEager {
		t.Fatalf("external harness = %s, want eager", got)
	}
}

func TestTokenizeStructuredNames(t *testing.T) {
	cases := map[string][]string{
		"snake_case":         {"snake", "case"},
		"kebab-case":         {"kebab", "case"},
		"dot.name":           {"dot", "name"},
		"server__tool":       {"server", "tool"},
		"getPullRequest42ID": {"get", "pull", "request42", "id"},
	}
	for input, want := range cases {
		if got := tokenize(input); !reflect.DeepEqual(got, want) {
			t.Errorf("tokenize(%q) = %#v, want %#v", input, got, want)
		}
	}
}

func TestIndexExactNamesAndStableTieBreak(t *testing.T) {
	tools := []mcp.CatalogTool{
		{ID: "github__search_issues", Name: "github__search_issues", OriginalName: "search_issues", Server: "github", Description: "Find tickets by text"},
		{ID: "sentry__search_issues", Name: "sentry__search_issues", OriginalName: "search_issues", Server: "sentry", Description: "Find monitoring incidents"},
		{ID: "docs__find", Name: "docs__find", OriginalName: "find", Server: "docs", Description: "Search issues in documents"},
	}
	idx := newSearchIndex(tools, 1)
	results := idx.search("github__search_issues", 5, nil)
	if len(results) == 0 || results[0].ID != "github__search_issues" {
		t.Fatalf("exact prefixed ranking = %#v", results)
	}
	results = idx.search("search_issues", 5, nil)
	if len(results) < 2 || (results[0].ID != "github__search_issues" && results[0].ID != "sentry__search_issues") || (results[1].ID != "github__search_issues" && results[1].ID != "sentry__search_issues") {
		t.Fatalf("exact original names did not outrank description-only match: %#v", results)
	}

	tied := newSearchIndex([]mcp.CatalogTool{
		{ID: "alpha__same", Name: "alpha__same", OriginalName: "same", Server: "service", Description: "identical description"},
		{ID: "beta__same", Name: "beta__same", OriginalName: "same", Server: "service", Description: "identical description"},
	}, 2).search("same", 5, nil)
	if len(tied) != 2 || tied[0].ID != "alpha__same" || tied[1].ID != "beta__same" {
		t.Fatalf("stable tie order = %#v", tied)
	}
}

func TestIndexEligibilityFilterExcludesDeniedTool(t *testing.T) {
	idx := newSearchIndex([]mcp.CatalogTool{
		{ID: "allowed__search", Name: "allowed__search", OriginalName: "search", Description: "Find records"},
		{ID: "denied__search", Name: "denied__search", OriginalName: "search", Description: "Find records"},
	}, 1)
	results := idx.search("search", 5, func(id string) bool { return id != "denied__search" })
	if len(results) != 1 || results[0].ID != "allowed__search" {
		t.Fatalf("eligible search leaked denied tool: %#v", results)
	}
}

func TestIndexReplacementIsAtomicDuringSearch(t *testing.T) {
	first := []mcp.CatalogTool{{ID: "alpha__find", Name: "alpha__find", OriginalName: "find", Description: "Find records"}}
	second := []mcp.CatalogTool{{ID: "beta__find", Name: "beta__find", OriginalName: "find", Description: "Find records"}}
	idx := newSearchIndex(first, 1)
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for generation := uint64(2); generation < 1000; generation++ {
			if generation%2 == 0 {
				idx.replace(second, generation)
			} else {
				idx.replace(first, generation)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			results := idx.search("find", 5, nil)
			if len(results) != 1 || (results[0].ID != "alpha__find" && results[0].ID != "beta__find") {
				select {
				case errs <- fmt.Errorf("partial index result: %#v", results):
				default:
				}
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestSchemaTermsAreDepthAndSizeBounded(t *testing.T) {
	root := map[string]any{"type": "object"}
	current := root
	for i := 0; i < 100; i++ {
		next := map[string]any{"description": "deep hidden field"}
		current["properties"] = map[string]any{"level": next}
		current = next
	}
	terms := schemaTerms(root, schemaTraversalDepth)
	if len(terms) == 0 || len(terms) > 512 {
		t.Fatalf("schema terms len = %d, want bounded non-empty", len(terms))
	}
}

func TestIndexUsesCanonicalNamespacedNameAsKey(t *testing.T) {
	idx := newSearchIndex([]mcp.CatalogTool{{
		ID: "legacy-id", Name: "server__canonical_name", OriginalName: "canonical_name", Server: "server",
	}}, 1)
	results := idx.search("canonical name", 1, nil)
	if len(results) != 1 || results[0].ID != "server__canonical_name" {
		t.Fatalf("search key = %#v, want canonical namespaced tool name", results)
	}
}

func TestAnnotationHintsUseNormalTokenization(t *testing.T) {
	if terms := annotationTerms(mcp.ToolAnnotations{}); terms != nil {
		t.Fatalf("annotationTerms(absent) = %#v, want nil", terms)
	}
	terms := annotationTerms(mcp.ToolAnnotations{Present: true, ReadOnly: true})
	if !reflect.DeepEqual(terms, []string{"read", "readonly", "read", "only"}) {
		t.Fatalf("annotationTerms(read-only) = %#v", terms)
	}
}
