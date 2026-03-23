package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEditFileTool_ConcurrentEditsPreserved exercises the real EditFileTool
// with concurrent goroutines editing distinct lines of the same file.
// Without serialization, read-modify-write races would lose edits.
func TestEditFileTool_ConcurrentEditsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	const n = 20
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditFileTool(nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args, _ := json.Marshal(EditFileArgs{
				Path:    path,
				OldText: fmt.Sprintf("line%d", i),
				NewText: fmt.Sprintf("line%d-edited", i),
			})
			out, err := tool.Execute(ctx, args)
			if err != nil {
				t.Errorf("goroutine %d: Execute error: %v", i, err)
				return
			}
			if strings.Contains(out.Content, "ERROR") {
				t.Errorf("goroutine %d: tool error: %s", i, out.Content)
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result := string(data)
	for i := 0; i < n; i++ {
		expected := fmt.Sprintf("line%d-edited", i)
		if !strings.Contains(result, expected) {
			t.Errorf("edit for line %d was lost", i)
		}
	}
}

// TestEditFileTool_DifferentPathsIndependent verifies that holding a lock on
// one path does not block acquisition of a lock on a different path.
func TestEditFileTool_DifferentPathsIndependent(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")

	// Hold pathA's lock for the duration of the test.
	unlockA := lockFilePath(pathA)
	defer unlockA()

	// pathB should be acquirable immediately even though pathA is held.
	done := make(chan struct{})
	go func() {
		unlockB := lockFilePath(pathB)
		unlockB()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("acquiring lock on pathB blocked despite pathA being a different path")
	}
}
