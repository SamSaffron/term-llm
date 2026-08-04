package mentions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMentionTestFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEagerFileSizeBoundary(t *testing.T) {
	root := t.TempDir()
	prefix := strings.Repeat("x\n", MaxEagerFallbackLines+1)
	exact := prefix + strings.Repeat("y", int(MaxEagerFileBytes)-len(prefix))
	writeMentionTestFile(t, root, "exact.txt", exact)
	writeMentionTestFile(t, root, "over.txt", exact+"z")

	got := LoadEagerAttachments(context.Background(), root, "@exact.txt @over.txt", nil)
	if len(got) != 1 || got[0].Path != "exact.txt" {
		t.Fatalf("boundary attachments = %#v", got)
	}
	if !got[0].Truncated || got[0].LineStart != 1 || got[0].LineEnd != MaxEagerFallbackLines {
		t.Fatalf("exact-boundary token fallback = %#v", got[0])
	}
}

func TestGiantOneLine10MBFileIsNeverOpenedOrAttached(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "giant.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	block := []byte(strings.Repeat("x", 64<<10))
	for written := 0; written < 10<<20; written += len(block) {
		if _, err := file.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	opens := 0
	got := loadEagerAttachments(context.Background(), root, "@giant.txt", nil, eagerLoadOptions{
		openFile: func(secureMentionRoot, string) (*os.File, error) {
			opens++
			return nil, errors.New("unexpected open")
		},
	})
	if len(got) != 0 || opens != 0 || FormatEagerAttachments(got) != "" {
		t.Fatalf("oversized file attachments=%#v opens=%d", got, opens)
	}
}

func TestLineRangeDoesNotBypassTotalSizeGate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("wanted\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(10 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	opens := 0
	got := loadEagerAttachments(context.Background(), root, "@large.txt#L1", nil, eagerLoadOptions{
		openFile: func(secureMentionRoot, string) (*os.File, error) {
			opens++
			return nil, errors.New("unexpected open")
		},
	})
	if len(got) != 0 || opens != 0 {
		t.Fatalf("oversized ranged file attachments=%#v opens=%d", got, opens)
	}
}

func TestTokenOverflowFallsBackToRequestedTwoThousandLines(t *testing.T) {
	root := t.TempDir()
	var content strings.Builder
	for i := 1; i <= 3_200; i++ {
		fmt.Fprintf(&content, "%04d %s\n", i, strings.Repeat("x", 34))
	}
	writeMentionTestFile(t, root, "tokens.txt", content.String())

	got := LoadEagerAttachments(context.Background(), root, "@tokens.txt#L101-3200", nil)
	if len(got) != 1 {
		t.Fatalf("attachments = %#v", got)
	}
	attachment := got[0]
	if !attachment.Truncated || attachment.LineStart != 101 || attachment.LineEnd != 2100 {
		t.Fatalf("fallback range = %#v", attachment)
	}
	if !strings.HasPrefix(attachment.Content, "0101 ") || strings.Contains(attachment.Content, "2101 ") {
		t.Fatalf("fallback content bounds are wrong")
	}
}

func TestLocalTokenLimitBoundary(t *testing.T) {
	root := t.TempDir()
	writeMentionTestFile(t, root, "exact-tokens.txt", strings.Repeat("x", MaxEagerContentTokens*4))
	writeMentionTestFile(t, root, "over-tokens.txt", strings.Repeat("x", MaxEagerContentTokens*4+1))

	got := LoadEagerAttachments(context.Background(), root, "@exact-tokens.txt @over-tokens.txt", nil)
	if len(got) != 1 || got[0].Path != "exact-tokens.txt" || got[0].Truncated {
		t.Fatalf("token boundary attachments = %#v", got)
	}
}

func TestTokenOverflowGiantLineAttachesNothing(t *testing.T) {
	root := t.TempDir()
	writeMentionTestFile(t, root, "one-line.txt", strings.Repeat("x", MaxEagerContentTokens*4+1))
	if got := LoadEagerAttachments(context.Background(), root, "@one-line.txt", nil); len(got) != 0 {
		t.Fatalf("giant one-line attachment = %#v", got)
	}
}

func TestRawDuplicateMentionsAttachOnceWithoutCanonicalDeduplication(t *testing.T) {
	root := t.TempDir()
	writeMentionTestFile(t, root, "manual.txt", "first\nsecond\n")
	input := "manual @manual.txt and @./manual.txt then @manual.txt and @manual.txt#L2"
	got := LoadEagerAttachments(context.Background(), root, input, nil)
	if len(got) != 3 {
		t.Fatalf("raw-deduplicated attachments = %#v", got)
	}
	if got[0].Content != "first\nsecond\n" || got[1].Content != "first\nsecond\n" || got[2].Content != "second\n" {
		t.Fatalf("attachment contents = %#v", got)
	}
	formatted := FormatEagerAttachments(got)
	if strings.Count(formatted, "Contents of @manual.txt") != 3 || strings.Count(input, "@manual.txt") != 3 {
		t.Fatalf("formatted=%q input=%q", formatted, input)
	}
}

func TestQuotedSpaceMentionAndLineRange(t *testing.T) {
	root := t.TempDir()
	writeMentionTestFile(t, root, "docs/design notes.md", "one\ntwo\nthree\n")
	got := LoadEagerAttachments(context.Background(), root, `review @"docs/design notes.md#L2-3"`, nil)
	if len(got) != 1 || got[0].Path != "docs/design notes.md" || got[0].Content != "two\nthree\n" {
		t.Fatalf("quoted attachment = %#v", got)
	}
}

func TestQuotedAttachmentsPrecedeUnquotedAttachments(t *testing.T) {
	root := t.TempDir()
	writeMentionTestFile(t, root, "plain.txt", "plain\n")
	writeMentionTestFile(t, root, "quoted name.txt", "quoted\n")

	got := LoadEagerAttachments(context.Background(), root, `@plain.txt then @"quoted name.txt"`, nil)
	if len(got) != 2 || got[0].Path != "quoted name.txt" || got[1].Path != "plain.txt" {
		t.Fatalf("attachment order = %#v", got)
	}
}

func TestDirectoryListingCapsAtOneThousandNames(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "many")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxEagerDirectoryEntries+5; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%04d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := LoadEagerAttachments(context.Background(), root, "@many/", nil)
	if len(got) != 1 || got[0].Kind != KindDirectory || got[0].EntryCount != 1005 || !got[0].Truncated {
		t.Fatalf("directory attachment = %#v", got)
	}
	lines := strings.Split(got[0].Content, "\n")
	if len(lines) != MaxEagerDirectoryEntries+1 || lines[len(lines)-1] != "… and 5 more entries" {
		t.Fatalf("directory listing line count=%d tail=%q", len(lines), lines[len(lines)-1])
	}
}

func TestDeniedAndRootEscapeMentionsAreOmitted(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMentionTestFile(t, root, "denied.txt", "secret")
	writeMentionTestFile(t, parent, "outside.txt", "outside")

	checked := 0
	got := LoadEagerAttachments(context.Background(), root, "@denied.txt", func(string) bool {
		checked++
		return false
	})
	if len(got) != 0 || checked != 1 {
		t.Fatalf("denied attachments = %#v checks=%d", got, checked)
	}
	if got := LoadEagerAttachments(context.Background(), root, "@../outside.txt", nil); len(got) != 0 {
		t.Fatalf("root-escaping attachment = %#v", got)
	}

	if err := os.Symlink(filepath.Join(parent, "outside.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if got := LoadEagerAttachments(context.Background(), root, "@link.txt", nil); len(got) != 0 {
		t.Fatalf("symlink escape attachment = %#v", got)
	}
}

func TestSymlinkSwapAfterApprovalCannotEscapeSecureRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMentionTestFile(t, root, "note.txt", "approved-final")
	writeMentionTestFile(t, root, "nested/note.txt", "approved-parent")
	outside := writeMentionTestFile(t, parent, "outside.txt", "outside-secret")
	probeLink := filepath.Join(root, "symlink-probe")
	if err := os.Symlink(outside, probeLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Remove(probeLink); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(parent, "outside-dir")
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMentionTestFile(t, outsideDir, "note.txt", "outside-parent-secret")

	tests := []struct {
		name    string
		mention string
		swap    func()
	}{
		{
			name:    "final component",
			mention: "@note.txt",
			swap: func() {
				if err := os.Rename(filepath.Join(root, "note.txt"), filepath.Join(root, "approved-note.txt")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "note.txt")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:    "parent component",
			mention: "@nested/note.txt",
			swap: func() {
				if err := os.Rename(filepath.Join(root, "nested"), filepath.Join(root, "approved-nested")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideDir, filepath.Join(root, "nested")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approved := false
			got := loadEagerAttachments(context.Background(), root, test.mention, func(string) bool {
				approved = true
				return true
			}, eagerLoadOptions{beforeOpen: func(string) { test.swap() }})
			if !approved {
				t.Fatal("read policy was not evaluated before the deterministic swap")
			}
			if len(got) != 0 {
				t.Fatalf("replacement escaped through opened descriptor: %#v", got)
			}
		})
		if test.name == "final component" {
			if err := os.Remove(filepath.Join(root, "note.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(root, "approved-note.txt"), filepath.Join(root, "note.txt")); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestOpenedDescriptorMustMatchApprovedFile(t *testing.T) {
	root := t.TempDir()
	writeMentionTestFile(t, root, "note.txt", "approved-body")
	writeMentionTestFile(t, root, "replacement.txt", "replacement-body")

	got := loadEagerAttachments(context.Background(), root, "@note.txt", func(string) bool { return true }, eagerLoadOptions{
		beforeOpen: func(string) {
			if err := os.Rename(filepath.Join(root, "note.txt"), filepath.Join(root, "original.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(root, "replacement.txt"), filepath.Join(root, "note.txt")); err != nil {
				t.Fatal(err)
			}
		},
	})
	if len(got) != 0 {
		t.Fatalf("different post-approval descriptor was attached: %#v", got)
	}
}

func TestNetworkPathSpellingsCannotEscapeProjectRoot(t *testing.T) {
	root := t.TempDir()
	canonical, err := canonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		`//server/share/secret.txt`,
		`\\server\share\secret.txt`,
		`\\wsl$\Ubuntu\home\secret.txt`,
		`\\?\UNC\server\share\secret.txt`,
	}
	for _, mentioned := range paths {
		t.Run(mentioned, func(t *testing.T) {
			if resolved, _, err := resolveMentionPath(canonical, mentioned); err == nil {
				t.Fatalf("network path resolved outside project confinement: %q", resolved)
			}
		})
	}
}

func TestPDFAndImageMentionsRemainTextualReferences(t *testing.T) {
	root := t.TempDir()
	writeMentionTestFile(t, root, "small.pdf", "%PDF-1.7 text-like fixture")
	writeMentionTestFile(t, root, "small.svg", "<svg></svg>")
	if got := LoadEagerAttachments(context.Background(), root, "@small.pdf @small.svg", nil); len(got) != 0 {
		t.Fatalf("media mention attachments = %#v", got)
	}
}
