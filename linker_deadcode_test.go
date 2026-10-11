package main

import (
	"bufio"
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinkerKeepsMethodDeadCodeElimination fails if anything reachable from
// main calls a reflect method lookup with a non-constant name, such as
// reflect.Value.MethodByName. text/template and html/template execution are
// the usual culprits.
//
// When the linker sees such a call reachable it can no longer prove which
// methods are unused, so it keeps every exported method of every type that is
// converted to an interface. With the generated provider SDKs linked in, that
// made the stripped binary 82 MB instead of 64 MB.
//
// The check relinks the real binary with -ldflags=-dumpdep and looks for
// symbols the linker marked <ReflectMethod>. It reuses the build cache, so it
// costs little more than a link.
func TestLinkerKeepsMethodDeadCodeElimination(t *testing.T) {
	if testing.Short() {
		t.Skip("relinks the binary")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found")
	}
	cmd := exec.Command(goTool, "build", "-ldflags=-dumpdep", "-o", filepath.Join(t.TempDir(), "term-llm"), ".")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	const mark = " <ReflectMethod>"
	parent := make(map[string]string) // first symbol that made each symbol reachable
	var marked []string
	isMarked := make(map[string]bool)
	noteMark := func(sym string) string {
		if !strings.HasSuffix(sym, mark) {
			return sym
		}
		sym = strings.TrimSuffix(sym, mark)
		if !isMarked[sym] {
			isMarked[sym] = true
			marked = append(marked, sym)
		}
		return sym
	}
	var other strings.Builder
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		from, to, ok := strings.Cut(scanner.Text(), " -> ")
		if !ok {
			other.WriteString(scanner.Text() + "\n")
			continue
		}
		from, to = noteMark(from), noteMark(to)
		if _, seen := parent[to]; !seen {
			parent[to] = from
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("go build -ldflags=-dumpdep: %v\n%s%s", err, stdout.String(), other.String())
	}
	if len(parent) == 0 {
		t.Fatalf("linker produced no -dumpdep output; the check is not working\n%s", other.String())
	}

	if len(marked) == 0 {
		return
	}
	// Report the first mark: once one dynamic lookup is reachable, the linker
	// marks further reflect wrappers as a consequence, which is just noise.
	sym := marked[0]
	chain := []string{sym}
	for cur, depth := sym, 0; depth < 25; depth++ {
		next, ok := parent[cur]
		if !ok || next == "" || next == "_" || next == cur {
			break
		}
		chain = append(chain, next)
		cur = next
	}
	t.Fatalf("%s calls a reflect method lookup with a dynamic name, which disables the linker's\n"+
		"dead-method elimination for the whole binary (usually text/template or html/template\n"+
		"execution; see internal/htmlesc). Reached via:\n    %s\n(%d symbol(s) marked in total)",
		sym, strings.Join(chain, "\n    <- "), len(marked))
}
