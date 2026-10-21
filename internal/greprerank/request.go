package greprerank

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/classify"
)

const (
	maxBlockLines = 12
	maxBlockRunes = 1200
	matchPrefix   = "> "
)

type chunkRequest struct {
	start, count int // start is the 0-based global index of the first item
	req          classify.Request
}

func questionID(n int) string { return fmt.Sprintf("r%d", n) }

func instructions(n int) string {
	return fmt.Sprintf("Is search result #%d relevant to the search intent, i.e. something the agent actually wants to see (not an incidental pattern match)?", n)
}

func buildRequests(model, intent, pattern string, items []Item) ([]chunkRequest, error) {
	var reqs []chunkRequest
	for start := 0; start < len(items); start += ChunkSize {
		end := min(start+ChunkSize, len(items))
		var listing strings.Builder
		qs := make(map[string]classify.Question, end-start)
		for i := start; i < end; i++ {
			if i > start {
				listing.WriteString("\n\n")
			}
			fmt.Fprintf(&listing, "%d. %s\n%s", i+1, toValid(items[i].Path), BoundBlock(items[i].Block))
			ins, err := marshal(instructions(i + 1))
			if err != nil {
				return nil, err
			}
			qs[questionID(i+1)] = classify.Question{Type: classify.TypeNoul, Instructions: ins}
		}
		state, err := marshal(map[string]string{
			"search_intent": toValid(intent),
			"grep_pattern":  toValid(pattern),
			"results":       listing.String(),
		})
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, chunkRequest{start: start, count: end - start,
			req: classify.Request{State: state, Model: model, Questions: qs}})
	}
	return reqs, nil
}

func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

func toValid(s string) string { return strings.ToValidUTF8(s, "�") }

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// BoundBlock limits a block to 12 lines and 1200 runes, always preserving
// matched ("> "-prefixed) lines (truncated only if they alone exceed the
// budget). Output is valid UTF-8.
func BoundBlock(block string) string {
	block = toValid(block)
	lines := strings.Split(block, "\n")
	var matched []int
	for i, l := range lines {
		if strings.HasPrefix(l, matchPrefix) {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		if len(lines) > maxBlockLines {
			lines = lines[:maxBlockLines]
		}
		return truncRunes(strings.Join(lines, "\n"), maxBlockRunes)
	}
	if len(matched) > maxBlockLines {
		matched = matched[:maxBlockLines]
	}
	keep := map[int]string{}
	capPer := (maxBlockRunes - len(matched)) / len(matched)
	used := 0
	for _, i := range matched {
		l := truncRunes(lines[i], capPer)
		keep[i] = l
		used += utf8.RuneCountInString(l) + 1
	}
	count := len(matched)
	for i, l := range lines {
		if _, ok := keep[i]; ok || count >= maxBlockLines {
			continue
		}
		n := utf8.RuneCountInString(l) + 1
		if used+n > maxBlockRunes {
			continue
		}
		keep[i] = l
		used += n
		count++
	}
	out := make([]string, 0, len(keep))
	for i := range lines {
		if l, ok := keep[i]; ok {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
