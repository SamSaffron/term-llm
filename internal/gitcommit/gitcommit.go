// Package gitcommit provides the host-owned Git transaction used by native
// commit workflows. It deliberately has no UI or HTTP dependencies.
package gitcommit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	maxCommandOutput = 16 << 20
	maxStatusRecords = 5000
	maxIdentityBytes = 64 << 20
	maxCommitOutput  = 1 << 20
)

type ErrorKind string

const (
	ErrGitMissing           ErrorKind = "git_missing"
	ErrNotRepository        ErrorKind = "not_repository"
	ErrBareRepository       ErrorKind = "bare_repository"
	ErrUnsafeRepository     ErrorKind = "unsafe_repository"
	ErrConflict             ErrorKind = "conflict"
	ErrUnsupportedOperation ErrorKind = "unsupported_operation"
	ErrIntentToAdd          ErrorKind = "intent_to_add"
	ErrEmptyIndex           ErrorKind = "empty_index"
	ErrStale                ErrorKind = "stale"
	ErrInvalidSelection     ErrorKind = "invalid_selection"
	ErrIndexLock            ErrorKind = "index_lock"
	ErrMissingIdentity      ErrorKind = "missing_identity"
	ErrHook                 ErrorKind = "hook_failure"
	ErrSigning              ErrorKind = "signing_failure"
	ErrCommit               ErrorKind = "commit_failure"
	ErrUncertain            ErrorKind = "uncertain_outcome"
	ErrOutputLimit          ErrorKind = "output_limit"
)

type Error struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
	Output  string    `json:"output,omitempty"`
	Cause   error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return string(e.Kind)
}
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
func IsKind(err error, kind ErrorKind) bool {
	var target *Error
	return errors.As(err, &target) && target.Kind == kind
}

func typed(kind ErrorKind, message string, cause error) error {
	return &Error{Kind: kind, Message: message, Cause: cause}
}

type HeadState string

const (
	HeadBorn   HeadState = "born"
	HeadUnborn HeadState = "unborn"
)

type OperationKind string

const (
	OperationNone       OperationKind = "none"
	OperationMerge      OperationKind = "merge"
	OperationCherryPick OperationKind = "cherry_pick"
	OperationRevert     OperationKind = "revert"
	OperationRebase     OperationKind = "rebase"
	OperationSequencer  OperationKind = "sequencer"
)

type OperationState struct {
	Kind        OperationKind `json:"kind"`
	Description string        `json:"description,omitempty"`
}
type OperationFingerprint struct {
	Kind     OperationKind `json:"kind"`
	HeadOIDs []string      `json:"head_oids"`
	Digest   string        `json:"digest"`
}
type Fingerprint struct {
	CheckoutID string               `json:"checkout_id"`
	HeadState  HeadState            `json:"head_state"`
	HeadOID    string               `json:"head_oid,omitempty"`
	IndexTree  string               `json:"index_tree"`
	Operation  OperationFingerprint `json:"operation"`
}

type ChangeKind string

const (
	ChangeModified    ChangeKind = "modified"
	ChangeAdded       ChangeKind = "added"
	ChangeDeleted     ChangeKind = "deleted"
	ChangeRenamed     ChangeKind = "renamed"
	ChangeCopied      ChangeKind = "copied"
	ChangeTypeChanged ChangeKind = "type_changed"
	ChangeUntracked   ChangeKind = "untracked"
	ChangeConflicted  ChangeKind = "conflicted"
)

type Change struct {
	Path            string     `json:"path"`
	OldPath         string     `json:"old_path,omitempty"`
	Kind            ChangeKind `json:"kind"`
	Staged          bool       `json:"staged,omitempty"`
	Unstaged        bool       `json:"unstaged,omitempty"`
	Untracked       bool       `json:"untracked,omitempty"`
	PartiallyStaged bool       `json:"partially_staged,omitempty"`
	Submodule       bool       `json:"submodule,omitempty"`
	Additions       int        `json:"additions,omitempty"`
	Deletions       int        `json:"deletions,omitempty"`
	Binary          bool       `json:"binary,omitempty"`
}
type DiffSummary struct {
	Files       int `json:"files"`
	Additions   int `json:"additions"`
	Deletions   int `json:"deletions"`
	BinaryFiles int `json:"binary_files"`
}
type RepositoryState struct {
	CheckoutRoot               string         `json:"-"`
	GitDirID                   string         `json:"git_dir_id"`
	Branch                     string         `json:"branch,omitempty"`
	Detached                   bool           `json:"detached"`
	Unborn                     bool           `json:"unborn"`
	HeadOID                    string         `json:"head_oid,omitempty"`
	Operation                  OperationState `json:"operation"`
	Staged                     []Change       `json:"staged"`
	Unstaged                   []Change       `json:"unstaged"`
	Untracked                  []Change       `json:"untracked"`
	Conflicted                 []Change       `json:"conflicted"`
	Summary                    DiffSummary    `json:"summary"`
	Fingerprint                Fingerprint    `json:"fingerprint"`
	StatusToken                string         `json:"status_token,omitempty"`
	SelectionAvailable         bool           `json:"selection_available"`
	SelectionUnavailableReason string         `json:"selection_unavailable_reason,omitempty"`
	Truncated                  bool           `json:"truncated"`
	TotalStaged                int            `json:"total_staged"`
	TotalUnstaged              int            `json:"total_unstaged"`
	TotalUntracked             int            `json:"total_untracked"`
}

type StageMode string

const (
	StageAll            StageMode = "all"
	StageExactSelection StageMode = "exact_selection"
)

type StageRequest struct {
	Mode        StageMode `json:"mode"`
	Paths       []string  `json:"paths,omitempty"`
	StatusToken string    `json:"status_token,omitempty"`
}
type CommitResult struct {
	BeforeHead       string `json:"before_head,omitempty"`
	HeadOID          string `json:"head_oid"`
	TreeOID          string `json:"tree_oid"`
	ShortOID         string `json:"short_oid"`
	Subject          string `json:"subject"`
	Message          string `json:"message"`
	TreeChanged      bool   `json:"tree_changed"`
	OutcomeUncertain bool   `json:"outcome_uncertain"`
	Stdout           string `json:"stdout,omitempty"`
	Stderr           string `json:"stderr,omitempty"`
}

type MutationCoordinator interface {
	Acquire(context.Context, string) (func(), error)
}

type Repository struct {
	root        string
	gitDir      string
	checkoutID  string
	coordinator MutationCoordinator
}

func Open(ctx context.Context, dir string) (*Repository, error) {
	return OpenWithCoordinator(ctx, dir, nil)
}
func OpenWithCoordinator(ctx context.Context, dir string, coordinator MutationCoordinator) (*Repository, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, typed(ErrNotRepository, "the active session directory is not in a Git checkout", nil)
	}
	rootOut, err := runGitAt(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, classifyDiscovery(err)
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(string(rootOut)))
	if err != nil {
		return nil, typed(ErrNotRepository, "resolve Git checkout root", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	bare, err := runGitAt(ctx, root, nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return nil, classifyDiscovery(err)
	}
	if strings.TrimSpace(string(bare)) == "true" {
		return nil, typed(ErrBareRepository, "bare repositories cannot be committed by /commit", nil)
	}
	gitDirOut, err := runGitAt(ctx, root, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, classifyDiscovery(err)
	}
	gitDir := strings.TrimSpace(string(gitDirOut))
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout Git directory: %w", err)
	}
	h := sha256.Sum256([]byte(root + "\x00" + gitDir))
	return &Repository{root: root, gitDir: gitDir, checkoutID: hex.EncodeToString(h[:]), coordinator: coordinator}, nil
}

func (r *Repository) CheckoutRoot() string                 { return r.root }
func (r *Repository) CheckoutID() string                   { return r.checkoutID }
func (r *Repository) SetCoordinator(c MutationCoordinator) { r.coordinator = c }

var checkoutLocks sync.Map

func checkoutMutex(id string) *sync.Mutex {
	value, _ := checkoutLocks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}
func (r *Repository) acquire(ctx context.Context) (func(), error) {
	if r.coordinator != nil {
		releaseOuter, err := r.coordinator.Acquire(ctx, r.root)
		if err != nil {
			return nil, err
		}
		checkoutMutex(r.checkoutID).Lock()
		return func() { checkoutMutex(r.checkoutID).Unlock(); releaseOuter() }, nil
	}
	checkoutMutex(r.checkoutID).Lock()
	return func() { checkoutMutex(r.checkoutID).Unlock() }, nil
}

func (r *Repository) Inspect(ctx context.Context) (RepositoryState, error) {
	release, err := r.acquire(ctx)
	if err != nil {
		return RepositoryState{}, err
	}
	defer release()
	return r.inspectUnlocked(ctx)
}

func (r *Repository) inspectUnlocked(ctx context.Context) (RepositoryState, error) {
	state := RepositoryState{
		CheckoutRoot:       r.root,
		GitDirID:           r.checkoutID,
		Operation:          OperationState{Kind: OperationNone},
		Staged:             []Change{},
		Unstaged:           []Change{},
		Untracked:          []Change{},
		Conflicted:         []Change{},
		SelectionAvailable: true,
	}
	raw, err := r.git(ctx, nil, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		return state, classifyGitError("inspect repository", err)
	}
	if len(raw) >= maxCommandOutput {
		return state, typed(ErrOutputLimit, "repository status exceeds the supported output limit", nil)
	}
	canonical, paths, intentToAdd, err := parseStatus(raw, &state)
	if err != nil {
		return state, err
	}
	r.populateChangeStats(ctx, &state)
	state.Summary = summarizeChanges(state.Staged)
	state.TotalStaged, state.TotalUnstaged, state.TotalUntracked = len(state.Staged), len(state.Unstaged), len(state.Untracked)
	if len(paths) > maxStatusRecords {
		state.SelectionAvailable = false
		state.Truncated = true
		state.SelectionUnavailableReason = "too many changed files for exact selection"
		state.Staged = truncateChanges(state.Staged)
		state.Unstaged = truncateChanges(state.Unstaged)
		state.Untracked = truncateChanges(state.Untracked)
	}
	if len(state.Conflicted) > 0 {
		return state, &Error{Kind: ErrConflict, Message: "resolve Git conflicts before committing"}
	}
	op, opfp, err := r.operationState(ctx)
	if err != nil {
		return state, err
	}
	state.Operation = op
	if op.Kind != OperationNone {
		state.Fingerprint.Operation = opfp
		return state, &Error{Kind: ErrUnsupportedOperation, Message: op.Description}
	}
	headOID, headState, branch, detached, err := r.head(ctx, raw)
	if err != nil {
		return state, err
	}
	state.HeadOID = headOID
	state.Unborn = headState == HeadUnborn
	state.Branch = branch
	state.Detached = detached
	tree, err := r.git(ctx, nil, "write-tree")
	if err != nil {
		return state, classifyGitError("fingerprint index", err)
	}
	state.Fingerprint = Fingerprint{CheckoutID: r.checkoutID, HeadState: headState, HeadOID: headOID, IndexTree: strings.TrimSpace(string(tree)), Operation: opfp}
	if intentToAdd {
		state.SelectionAvailable = false
		state.SelectionUnavailableReason = "intent-to-add entries require manual Git staging"
		return state, &Error{Kind: ErrIntentToAdd, Message: state.SelectionUnavailableReason}
	}
	if state.SelectionAvailable {
		token, ok := r.statusToken(ctx, canonical, paths)
		if ok {
			state.StatusToken = token
		} else {
			state.SelectionAvailable = false
			state.SelectionUnavailableReason = "changed content exceeds exact-selection limits"
		}
	}
	return state, nil
}

func truncateChanges(v []Change) []Change {
	if len(v) > maxStatusRecords {
		return append([]Change(nil), v[:maxStatusRecords]...)
	}
	return v
}

func (r *Repository) head(ctx context.Context, status []byte) (string, HeadState, string, bool, error) {
	branch := ""
	oid := ""
	unborn := false
	for _, rec := range bytes.Split(status, []byte{0}) {
		s := string(rec)
		if strings.HasPrefix(s, "# branch.head ") {
			branch = strings.TrimPrefix(s, "# branch.head ")
		}
		if strings.HasPrefix(s, "# branch.oid ") {
			oid = strings.TrimPrefix(s, "# branch.oid ")
			unborn = oid == "(initial)"
		}
	}
	if unborn {
		return "", HeadUnborn, branch, false, nil
	}
	if oid == "" {
		out, err := r.git(ctx, nil, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return "", "", branch, false, classifyGitError("read HEAD", err)
		}
		oid = strings.TrimSpace(string(out))
	}
	detached := branch == "(detached)"
	if detached {
		branch = ""
	}
	return oid, HeadBorn, branch, detached, nil
}

func (r *Repository) operationState(ctx context.Context) (OperationState, OperationFingerprint, error) {
	type marker struct {
		path string
		kind OperationKind
		desc string
	}
	markers := []marker{{"MERGE_HEAD", OperationMerge, "A merge is in progress; complete or abort it with Git before using /commit."}, {"CHERRY_PICK_HEAD", OperationCherryPick, "A cherry-pick is in progress; complete or abort it with Git before using /commit."}, {"REVERT_HEAD", OperationRevert, "A revert is in progress; complete or abort it with Git before using /commit."}, {"rebase-merge", OperationRebase, "A rebase is in progress; complete or abort it with Git before using /commit."}, {"rebase-apply", OperationRebase, "A rebase is in progress; complete or abort it with Git before using /commit."}, {"sequencer", OperationSequencer, "A Git sequencer operation is in progress; complete or abort it before using /commit."}}
	for _, m := range markers {
		p, err := r.git(ctx, nil, "rev-parse", "--git-path", m.path)
		if err != nil {
			return OperationState{}, OperationFingerprint{}, err
		}
		path := strings.TrimSpace(string(p))
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.root, path)
		}
		info, statErr := os.Stat(path)
		if statErr == nil {
			h := sha256.New()
			io.WriteString(h, m.path)
			io.WriteString(h, info.ModTime().UTC().String())
			heads := []string{}
			if !info.IsDir() {
				b, _ := os.ReadFile(path)
				io.WriteString(h, string(b))
				heads = strings.Fields(string(b))
			}
			return OperationState{Kind: m.kind, Description: m.desc}, OperationFingerprint{Kind: m.kind, HeadOIDs: heads, Digest: hex.EncodeToString(h.Sum(nil))}, nil
		}
		if !os.IsNotExist(statErr) {
			return OperationState{}, OperationFingerprint{}, statErr
		}
	}
	h := sha256.Sum256([]byte("none"))
	return OperationState{Kind: OperationNone}, OperationFingerprint{Kind: OperationNone, HeadOIDs: []string{}, Digest: hex.EncodeToString(h[:])}, nil
}

type changeStat struct {
	additions int
	deletions int
	binary    bool
}

func (r *Repository) populateChangeStats(ctx context.Context, state *RepositoryState) {
	applyChangeStats(state.Staged, r.diffStats(ctx, "--cached"))
	applyChangeStats(state.Unstaged, r.diffStats(ctx))
	remaining := int64(16 << 20)
	for i := range state.Untracked {
		stat := r.untrackedStat(state.Untracked[i].Path, &remaining)
		state.Untracked[i].Additions = stat.additions
		state.Untracked[i].Deletions = stat.deletions
		state.Untracked[i].Binary = stat.binary
	}
}

func applyChangeStats(changes []Change, stats map[string]changeStat) {
	for i := range changes {
		stat, ok := stats[changes[i].Path]
		if !ok && changes[i].OldPath != "" {
			stat, ok = stats[changes[i].OldPath]
		}
		if !ok {
			continue
		}
		changes[i].Additions = stat.additions
		changes[i].Deletions = stat.deletions
		changes[i].Binary = stat.binary
	}
}

func (r *Repository) diffStats(ctx context.Context, options ...string) map[string]changeStat {
	args := []string{"diff"}
	args = append(args, options...)
	args = append(args, "--numstat", "-z", "--")
	out, err := r.git(ctx, nil, args...)
	if err != nil {
		return nil
	}
	records := bytes.Split(out, []byte{0})
	stats := make(map[string]changeStat)
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) == 0 {
			continue
		}
		fields := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(fields) < 3 {
			continue
		}
		pathBytes := fields[2]
		if len(pathBytes) == 0 && i+2 < len(records) {
			// With -z, rename/copy records contain an empty path followed by
			// separate old-path and new-path records. Status uses the new path.
			i += 2
			pathBytes = records[i]
		}
		if len(pathBytes) == 0 {
			continue
		}
		stat := changeStat{}
		if string(fields[0]) == "-" || string(fields[1]) == "-" {
			stat.binary = true
		} else {
			fmt.Sscanf(string(fields[0]), "%d", &stat.additions)
			fmt.Sscanf(string(fields[1]), "%d", &stat.deletions)
		}
		stats[string(pathBytes)] = stat
	}
	return stats
}

func (r *Repository) untrackedStat(name string, remaining *int64) changeStat {
	if !safePath(name) || remaining == nil || *remaining <= 0 {
		return changeStat{binary: true}
	}
	full := filepath.Join(r.root, filepath.FromSlash(name))
	info, err := os.Lstat(full)
	if err != nil {
		return changeStat{}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return changeStat{additions: 1}
	}
	if !info.Mode().IsRegular() || info.Size() > *remaining {
		return changeStat{binary: true}
	}
	file, err := os.Open(full)
	if err != nil {
		return changeStat{}
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return changeStat{}
	}
	body, err := io.ReadAll(io.LimitReader(file, info.Size()+1))
	if err != nil || int64(len(body)) != info.Size() {
		return changeStat{}
	}
	*remaining -= int64(len(body))
	if bytes.IndexByte(body, 0) >= 0 {
		return changeStat{binary: true}
	}
	lines := bytes.Count(body, []byte{'\n'})
	if len(body) > 0 && body[len(body)-1] != '\n' {
		lines++
	}
	return changeStat{additions: lines}
}

func summarizeChanges(changes []Change) DiffSummary {
	summary := DiffSummary{Files: len(changes)}
	for _, change := range changes {
		if change.Binary {
			summary.BinaryFiles++
			continue
		}
		summary.Additions += change.Additions
		summary.Deletions += change.Deletions
	}
	return summary
}

func (r *Repository) statusToken(ctx context.Context, canonical []byte, paths []string) (string, bool) {
	h := sha256.New()
	h.Write(canonical)
	index, err := r.git(ctx, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", false
	}
	h.Write(index)
	sort.Strings(paths)
	total := int64(0)
	for _, p := range paths {
		if !safePath(p) {
			return "", false
		}
		full := filepath.Join(r.root, filepath.FromSlash(p))
		info, err := os.Lstat(full)
		if os.IsNotExist(err) {
			io.WriteString(h, "D\x00"+p+"\x00")
			continue
		}
		if err != nil {
			return "", false
		}
		io.WriteString(h, p+"\x00"+info.Mode().String()+"\x00"+fmt.Sprint(info.Size())+"\x00")
		if info.Mode().IsRegular() {
			total += info.Size()
			if total > maxIdentityBytes {
				return "", false
			}
			f, err := os.Open(full)
			if err != nil {
				return "", false
			}
			_, copyErr := io.Copy(h, f)
			f.Close()
			if copyErr != nil {
				return "", false
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return "", false
			}
			io.WriteString(h, target)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func safePath(p string) bool {
	return p != "" && p != "." && !filepath.IsAbs(p) && path.Clean(p) == p && !strings.HasPrefix(p, "../") && !strings.ContainsRune(p, 0)
}
