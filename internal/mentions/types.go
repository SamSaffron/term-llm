package mentions

import "time"

// Kind identifies a filesystem mention target.
type Kind uint8

const (
	KindFile Kind = iota
	KindDirectory
)

// Candidate is an immutable project-relative index entry. LowerPath and
// ASCII are precomputed so queries do not lowercase or rescan the corpus.
type Candidate struct {
	Path       string
	LowerPath  string
	BaseOffset uint32
	ASCII      [4]uint64
	Kind       Kind
}

func (c Candidate) BaseName() string {
	if int(c.BaseOffset) >= len(c.Path) {
		return c.Path
	}
	return c.Path[c.BaseOffset:]
}

// Match is a ranked candidate and byte positions in its relative path.
type Match struct {
	Candidate int
	Score     int
	Positions []int
}

// Snapshot is an immutable index for one root.
type Snapshot struct {
	Root       string
	Candidates []Candidate
	BuiltAt    time.Time
	Truncated  bool
	Source     string
	PathBytes  int64
}

// BuildOptions bounds discovery work and memory.
type BuildOptions struct {
	MaxCandidates int
	MaxPathBytes  int64
}

func DefaultBuildOptions() BuildOptions {
	return BuildOptions{MaxCandidates: 250_000, MaxPathBytes: 64 << 20}
}

// ActiveToken describes a cursor-local @ token in valid UTF-8 composer text.
type ActiveToken struct {
	Start  int
	End    int
	Query  string
	Quoted bool
}

// SubmittedMention is an ordinary textual path mention discovered when a
// prompt is submitted. Line numbers are one-based; zero means no range.
type SubmittedMention struct {
	Path      string
	LineStart int
	LineEnd   int
}

// EagerAttachment is bounded context loaded for one submitted textual mention.
type EagerAttachment struct {
	Path       string
	Kind       Kind
	Content    string
	LineStart  int
	LineEnd    int
	Truncated  bool
	EntryCount int
}
