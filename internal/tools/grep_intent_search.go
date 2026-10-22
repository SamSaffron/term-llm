package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sync"

	"github.com/samsaffron/term-llm/internal/greprerank"
	"github.com/samsaffron/term-llm/internal/grepsearch"
)

type intentSearchResult struct {
	ripgrepResult
	snapshot func() []byte
	message  string
}

// searchIntent preserves an immutable ordinary prefix while expanding once.
func (t *GrepTool) searchIntent(ctx context.Context, a GrepArgs, root string, contextLines, maxResults int, attempt *greprerank.Attempt, publish func(intentSearchResult)) (intentSearchResult, error) {
	if ripgrepAvailable() {
		var prefix bytes.Buffer
		var mu sync.Mutex
		ordinary := newRipgrepResultLimiter(maxResults)
		seen := 0
		sent, frozen := false, false
		snapshot := func() []byte { mu.Lock(); defer mu.Unlock(); return bytes.Clone(prefix.Bytes()) }
		observer := func(line []byte) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if frozen {
				return false, nil
			}
			stop, _ := ordinary.consumeJSONLine(line)
			if stop {
				frozen = true
				return false, nil
			}
			mu.Lock()
			prefix.Write(line)
			mu.Unlock()
			var event rgMatch
			if json.Unmarshal(line, &event) == nil && event.Type == "match" {
				seen++
			}
			if !sent && seen >= maxResults {
				sent = true
				attempt.Start()
				publish(intentSearchResult{snapshot: snapshot, ripgrepResult: ripgrepResult{truncated: true}})
			}
			return false, nil
		}
		cap := t.reranker.MaxCandidates()
		// --max-count is per file: allow one match of headroom so a single file
		// with more than cap matches still reaches the global limiter, which
		// stops at cap and reports truncation.
		raw, truncated, err := runRipgrep(ctx, buildRipgrepArgs(a, root, contextLines, cap+1), t.rgCaptureLimits, cap, false, observer)
		if err == nil {
			attempt.Start() // parsing/candidate construction spends the remaining budget
			if !sent {
				publish(intentSearchResult{snapshot: func() []byte { return raw }, ripgrepResult: ripgrepResult{truncated: truncated}})
			}
			matches, err := parseRipgrepOutput(raw, cap, contextLines)
			value := intentSearchResult{ripgrepResult: ripgrepResult{matches: matches, truncated: truncated}}
			return value, err
		}
		if ctx.Err() != nil {
			return intentSearchResult{}, ctx.Err()
		}
	}
	return t.searchIntentGo(ctx, a, root, contextLines, maxResults, attempt, publish)
}

func (t *GrepTool) searchIntentGo(ctx context.Context, a GrepArgs, root string, contextLines, maxResults int, attempt *greprerank.Attempt, publish func(intentSearchResult)) (intentSearchResult, error) {
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return intentSearchResult{message: formatToolError(NewToolErrorf(ErrInvalidParams, "invalid regex pattern: %v", err))}, nil
	}
	files, err := grepsearch.CollectFiles(root, a.Include, a.Exclude)
	if err != nil {
		return intentSearchResult{message: formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to collect files: %v", err))}, nil
	}
	grepsearch.SortFilesByMtime(files)
	scan := t.intentScan
	if scan == nil {
		scan = grepsearch.Scan
	}
	var raw []GrepMatch
	var blocks []matchBlock
	onMatch := func(m GrepMatch) {
		raw = append(raw, m)
		if len(raw) == maxResults {
			attempt.Start()
			publish(intentSearchResult{ripgrepResult: ripgrepResult{matches: append([]GrepMatch(nil), raw...), truncated: true}})
		}
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return intentSearchResult{}, err
		}
		if len(raw) >= t.reranker.MaxCandidates() {
			break
		}
		_, _ = searchFileWithControl(file, re, t.reranker.MaxCandidates()-len(raw), contextLines, grepSearchControl{ctx: ctx, scan: scan, match: onMatch, blocks: func(found []matchBlock) { blocks = append(blocks, found...) }})
	}
	attempt.Start() // merging is added work, never part of the untimed scan
	truncated := len(raw) >= t.reranker.MaxCandidates()
	if len(raw) < maxResults {
		publish(intentSearchResult{ripgrepResult: ripgrepResult{matches: raw, truncated: truncated}})
	}
	value := intentSearchResult{ripgrepResult: ripgrepResult{matches: mergeIntentBlocks(blocks), truncated: truncated}}
	return value, ctx.Err()
}

func mergeIntentBlocks(blocks []matchBlock) []GrepMatch {
	var merged []matchBlock
	for _, b := range blocks {
		n := len(merged)
		if n > 0 && merged[n-1].filePath == b.filePath && b.lines[0].number <= merged[n-1].lines[len(merged[n-1].lines)-1].number+1 {
			merged[n-1] = mergeBlocks(merged[n-1], b)
		} else {
			merged = append(merged, b)
		}
	}
	out := make([]GrepMatch, len(merged))
	for i, b := range merged {
		out[i] = blockToGrepMatch(b)
	}
	return out
}
