package tooldiscovery

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/samsaffron/term-llm/internal/mcp"
)

const schemaTraversalDepth = 6

var tokenSeparator = regexp.MustCompile(`[^\pL\pN]+`)

type indexEntry struct {
	ID          string
	Name        string
	Original    string
	Server      string
	NameTerms   []string
	TitleTerms  []string
	DescTerms   []string
	ParamTerms  []string
	OutputTerms []string
	Hints       []string
	all         map[string]int
	length      int
}

type indexSnapshot struct {
	generation uint64
	entries    []indexEntry
	docFreq    map[string]int
	avgLength  float64
}

type searchIndex struct {
	current atomic.Pointer[indexSnapshot]
}

type SearchResult struct {
	ID    string
	Score float64
}

func newSearchIndex(tools []mcp.CatalogTool, generation uint64) *searchIndex {
	idx := &searchIndex{}
	idx.current.Store(buildIndexSnapshot(tools, generation))
	return idx
}

func (i *searchIndex) replace(tools []mcp.CatalogTool, generation uint64) {
	i.current.Store(buildIndexSnapshot(tools, generation))
}

func buildIndexSnapshot(tools []mcp.CatalogTool, generation uint64) *indexSnapshot {
	snapshot := &indexSnapshot{generation: generation, docFreq: make(map[string]int)}
	for _, tool := range tools {
		entry := indexEntry{
			ID:          tool.Name,
			Name:        strings.ToLower(tool.Name),
			Original:    strings.ToLower(tool.OriginalName),
			Server:      strings.ToLower(tool.Server),
			NameTerms:   tokenize(tool.Name + " " + tool.OriginalName),
			TitleTerms:  tokenize(tool.Title),
			DescTerms:   tokenize(tool.Description + " " + tool.NamespaceDescription),
			ParamTerms:  schemaTerms(tool.InputSchema, schemaTraversalDepth),
			OutputTerms: schemaTerms(tool.OutputSchema, schemaTraversalDepth),
			Hints:       annotationTerms(tool.Annotations),
			all:         make(map[string]int),
		}
		for _, terms := range [][]string{entry.NameTerms, entry.TitleTerms, entry.DescTerms, entry.ParamTerms, entry.OutputTerms, entry.Hints} {
			for _, term := range terms {
				entry.all[term]++
				entry.length++
			}
		}
		seen := make(map[string]struct{}, len(entry.all))
		for term := range entry.all {
			seen[term] = struct{}{}
		}
		for term := range seen {
			snapshot.docFreq[term]++
		}
		snapshot.avgLength += float64(entry.length)
		snapshot.entries = append(snapshot.entries, entry)
	}
	if len(snapshot.entries) > 0 {
		snapshot.avgLength /= float64(len(snapshot.entries))
	}
	sort.Slice(snapshot.entries, func(i, j int) bool { return snapshot.entries[i].ID < snapshot.entries[j].ID })
	return snapshot
}

func (i *searchIndex) search(query string, limit int, eligible func(string) bool) []SearchResult {
	snapshot := i.current.Load()
	if snapshot == nil || limit <= 0 {
		return nil
	}
	queryLower := strings.ToLower(strings.TrimSpace(query))
	terms := tokenize(queryLower)
	if len(terms) == 0 {
		return nil
	}
	results := make([]SearchResult, 0)
	for _, entry := range snapshot.entries {
		if eligible != nil && !eligible(entry.ID) {
			continue
		}
		score := lexicalScore(snapshot, entry, queryLower, terms)
		if score > 0 {
			results = append(results, SearchResult{ID: entry.ID, Score: score})
		}
	}
	sort.Slice(results, func(a, b int) bool {
		if results[a].Score == results[b].Score {
			return results[a].ID < results[b].ID
		}
		return results[a].Score > results[b].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func lexicalScore(snapshot *indexSnapshot, entry indexEntry, query string, terms []string) float64 {
	score := 0.0
	matchedTerms := make(map[string]bool)
	queryTerms := make(map[string]bool)
	if query == entry.Name {
		score += 1000
	}
	if query == entry.Original {
		score += 900
	}
	if query == entry.Server {
		score += 80
	}
	nameSet := termSet(entry.NameTerms)
	titleSet := termSet(entry.TitleTerms)
	descSet := termSet(entry.DescTerms)
	paramSet := termSet(entry.ParamTerms)
	outputSet := termSet(entry.OutputTerms)
	for _, term := range terms {
		queryTerms[term] = true
		tf := entry.all[term]
		if tf > 0 {
			matchedTerms[term] = true
			df := snapshot.docFreq[term]
			idf := math.Log(1 + (float64(len(snapshot.entries)-df)+0.5)/(float64(df)+0.5))
			lengthNorm := 1.0
			if snapshot.avgLength > 0 {
				lengthNorm = 1.2*(1-0.75+0.75*float64(entry.length)/snapshot.avgLength) + float64(tf)
			}
			score += idf * (float64(tf) * 2.2) / lengthNorm
		}
		if nameSet[term] {
			score += 12
		}
		for _, nameTerm := range entry.NameTerms {
			if strings.HasPrefix(nameTerm, term) || strings.HasPrefix(term, nameTerm) {
				matchedTerms[term] = true
				score += 6
				break
			}
		}
		if entry.Server == term {
			matchedTerms[term] = true
			score += 12
		}
		if titleSet[term] {
			score += 8
		}
		if descSet[term] {
			score += 4
		}
		if paramSet[term] {
			score += 3
		}
		if outputSet[term] {
			score += 2
		}
	}
	if score > 0 && len(queryTerms) > 0 {
		coverage := float64(len(matchedTerms)) / float64(len(queryTerms))
		score = score*coverage + 10*float64(len(matchedTerms))
	}
	return score
}

func tokenize(value string) []string {
	if value == "" {
		return nil
	}
	var expanded strings.Builder
	var previous rune
	for i, r := range value {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			expanded.WriteByte(' ')
		}
		expanded.WriteRune(unicode.ToLower(r))
		previous = r
	}
	parts := tokenSeparator.Split(expanded.String(), -1)
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func schemaTerms(schema map[string]any, depth int) []string {
	var terms []string
	var visit func(any, int)
	visit = func(value any, remaining int) {
		if remaining < 0 || len(terms) >= 512 {
			return
		}
		switch v := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				child := v[key]
				switch key {
				case "properties":
					if props, ok := child.(map[string]any); ok {
						propNames := make([]string, 0, len(props))
						for name := range props {
							propNames = append(propNames, name)
						}
						sort.Strings(propNames)
						for _, name := range propNames {
							terms = append(terms, tokenize(name)...)
							visit(props[name], remaining-1)
						}
					}
				case "description", "title":
					if text, ok := child.(string); ok {
						terms = append(terms, tokenize(text)...)
					}
				case "items", "anyOf", "oneOf", "allOf", "$defs", "definitions":
					visit(child, remaining-1)
				}
			}
		case []any:
			for _, child := range v {
				visit(child, remaining-1)
			}
		}
	}
	visit(schema, depth)
	if len(terms) > 512 {
		terms = terms[:512]
	}
	return terms
}

func annotationTerms(annotations mcp.ToolAnnotations) []string {
	if !annotations.Present {
		return nil
	}
	var hints []string
	if annotations.ReadOnly {
		hints = append(hints, "read readonly read-only")
	} else {
		hints = append(hints, "write mutation")
	}
	if annotations.Destructive != nil && *annotations.Destructive {
		hints = append(hints, "destructive")
	}
	if annotations.Idempotent {
		hints = append(hints, "idempotent")
	}
	return tokenize(strings.Join(hints, " "))
}

func termSet(terms []string) map[string]bool {
	set := make(map[string]bool, len(terms))
	for _, term := range terms {
		set[term] = true
	}
	return set
}
