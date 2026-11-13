package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/filetrack/shellscope"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// Bounds for shell file-change snapshots. They cap the cost of a single shell
// call, not correctness: files beyond the content-read budgets are still
// detected via stat and recorded metadata-only (truncated).
const (
	maxShellGlobMatches   = shellscope.MaxGlobMatches
	maxShellContentReads  = 200              // full-content snapshots per phase and shell call
	maxShellSnapshotBytes = 64 * 1024 * 1024 // total content bytes read per phase and shell call
	maxShellGitRepos      = 64               // distinct candidate repositories inspected post-command
	gitCommandTimeout     = shellscope.GitCommandTimeout
)

type shellCandidateSource uint8

const (
	shellSourceLiteral shellCandidateSource = 1 << iota
	shellSourceGlob
	shellSourceSession
	shellSourceGit
)

func (source shellCandidateSource) preservesCleanGitState() bool {
	return source&(shellSourceLiteral|shellSourceSession) != 0
}

// shellSnapshotEntry captures one file's pre-exec state.
type shellSnapshotEntry struct {
	existed bool
	size    int64
	modTime time.Time
	content []byte // nil when not read (oversized or beyond the pre-snapshot budget)
}

// shellSnapshot holds bounded pre-execution detection evidence. Mutation
// scope, Git state, and session paths influence inspection only; attribution is
// decided later from the pre-execution claims retained with the snapshot.
type normalizedOutputClaim struct {
	pattern   string
	kind      string
	literal   bool
	coverage  string
	matched   int
	confirmed int
}

type shellTrackingResult struct {
	FileChanges         []core.FileChange
	Observations        []core.FilesystemObservationSummary
	Diagnostics         []core.OutputClaimDiagnostic
	pendingObservations map[string]*filetrack.FilesystemObservation
}

type shellSnapshot struct {
	sessionID      string
	workDir        string
	patterns       []string
	claims         []normalizedOutputClaim
	files          map[string]*shellSnapshotEntry
	sources        map[string]shellCandidateSource
	gitRoot        string
	gitStatus      map[string]string // absolute path -> pre-command porcelain XY status
	coverageStatus string

	preContentReads  int
	preContentBytes  int64
	postContentReads int
	postContentBytes int64
	maxFileBytes     int
}

// canCapturePreContent reports whether a file may be retained in the bounded
// pre-command snapshot. Post-command comparison uses separate accounting so a
// full pre snapshot cannot prevent deterministic comparison of captured files.
func (snap *shellSnapshot) canCapturePreContent(size int64) bool {
	return size <= int64(snap.maxFileBytes) &&
		snap.preContentReads < maxShellContentReads &&
		snap.preContentBytes+size <= maxShellSnapshotBytes
}

func (snap *shellSnapshot) notePreContentRead(content []byte) {
	snap.preContentReads++
	snap.preContentBytes += int64(len(content))
}

func (snap *shellSnapshot) canReadPostContent(size int64) bool {
	return size <= int64(snap.maxFileBytes) &&
		snap.postContentReads < maxShellContentReads &&
		snap.postContentBytes+size <= maxShellSnapshotBytes
}

func (snap *shellSnapshot) notePostContentRead(content []byte) {
	snap.postContentReads++
	snap.postContentBytes += int64(len(content))
}

// preShellSnapshot records the relevant filesystem state before a shell
// command runs. Returns nil when tracking is inactive.
func preShellSnapshot(ctx context.Context, recorder FileChangeRecorder, workDir string, patterns []string) *shellSnapshot {
	return preShellSnapshotWithClaims(ctx, recorder, workDir, patterns, nil)
}

func preShellSnapshotWithClaims(ctx context.Context, recorder FileChangeRecorder, workDir string, patterns []string, claims []normalizedOutputClaim) *shellSnapshot {
	if recorder == nil {
		return nil
	}
	sessionID := core.SessionIDFromContext(ctx)
	if sessionID == "" {
		return nil
	}

	patterns = normalizeShellPatterns(patterns)
	// Claimed literals are reserved first so unrelated glob expansion cannot
	// consume their inspection budget. All claims implicitly join scope.
	inspectionPatterns := make([]string, 0, len(patterns)+len(claims))
	for _, claim := range claims {
		if claim.literal {
			inspectionPatterns = append(inspectionPatterns, claim.pattern)
		}
	}
	for _, claim := range claims {
		if !claim.literal {
			inspectionPatterns = append(inspectionPatterns, claim.pattern)
		}
	}
	inspectionPatterns = append(inspectionPatterns, patterns...)
	inspectionPatterns = normalizeShellPatterns(inspectionPatterns)
	snap := &shellSnapshot{
		sessionID:      sessionID,
		workDir:        workDir,
		patterns:       inspectionPatterns,
		claims:         append([]normalizedOutputClaim(nil), claims...),
		files:          make(map[string]*shellSnapshotEntry),
		sources:        make(map[string]shellCandidateSource),
		maxFileBytes:   recorder.MaxFileBytes(),
		coverageStatus: filetrack.CoverageComplete,
	}

	resolver := newShellRepoResolver()
	snap.gitRoot = resolver.owningRepo(workDir)

	var sessionPaths []string
	if len(inspectionPatterns) == 0 {
		sessionPaths = recorder.SessionPaths(ctx, sessionID)
	}
	// When the caller supplied bounded hints, trust that scope and avoid both
	// historical session paths and the repo-wide git status fallback. Commands
	// that omit hints still get the broader best-effort detection below.
	hasBoundedHints := len(inspectionPatterns) > 0 || len(sessionPaths) > 0
	if snap.gitRoot != "" && (!hasBoundedHints || len(claims) > 0) {
		// Claims need bounded pre-state ownership information even when their
		// inspection scope is explicit; Git state can label the baseline but can
		// never create or erase attribution.
		snap.gitStatus = gitStatusPorcelain(ctx, snap.gitRoot)
	}

	expandedPatterns := expandShellPatterns(workDir, inspectionPatterns)
	if len(expandedPatterns) >= maxShellGlobMatches {
		snap.coverageStatus = filetrack.CoverageTruncated
		for i := range snap.claims {
			if !snap.claims[i].literal {
				snap.claims[i].coverage = filetrack.CoverageTruncated
			}
		}
	}
	for _, candidate := range expandedPatterns {
		snap.sources[candidate.path] |= candidate.source
	}
	if snap.gitStatus != nil {
		// Snapshot paths that were already dirty before the command. Otherwise a
		// command that edits a dirty tracked file, or an existing untracked file,
		// can leave porcelain status unchanged (e.g. " M" -> " M", "??" ->
		// "??") and would be invisible after the command.
		for path := range snap.gitStatus {
			if !hasGitAdminComponent(path) {
				snap.sources[path] |= shellSourceGit
			}
		}
	}
	for _, path := range sessionPaths {
		path = filepath.Clean(path)
		if !hasGitAdminComponent(path) {
			snap.sources[path] |= shellSourceSession
		}
	}

	paths := make([]string, 0, len(snap.sources))
	for path := range snap.sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		snap.files[path] = snap.statAndMaybeRead(path)
	}
	return snap
}

type shellPostCandidate struct {
	entry           *shellSnapshotEntry
	source          shellCandidateSource
	postOnlyPattern bool
	repoRoot        string
	postStatus      string
	gitBefore       []byte
	gitBeforeOK     bool
}

type shellRepoStatus struct {
	paths     map[string]string
	available bool
}

// postShellChanges diffs the filesystem against a pre-exec snapshot and
// records every detected change. Runs regardless of the command's exit code —
// partial writes are real changes.
func postShellChanges(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot) []core.FileChange {
	return postShellTracking(ctx, recorder, snap).FileChanges
}

func collectShellPostCandidates(ctx context.Context, snap *shellSnapshot) (map[string]*shellPostCandidate, map[string]shellRepoStatus) {
	candidates := make(map[string]*shellPostCandidate, len(snap.files))
	for path, entry := range snap.files {
		candidates[path] = &shellPostCandidate{entry: entry, source: snap.sources[path]}
	}

	// Re-expanding the same patterns catches files created by the command. A
	// glob match seen only post-command is treated as a creation; exact literals
	// were included in the pre snapshot even when missing.
	for _, expanded := range expandShellPatterns(snap.workDir, snap.patterns) {
		candidate, seen := candidates[expanded.path]
		if !seen {
			candidate = &shellPostCandidate{}
			candidates[expanded.path] = candidate
			candidate.postOnlyPattern = expanded.source&shellSourceGlob != 0
		}
		candidate.source |= expanded.source
	}

	// For the no-hints Git fallback, one post-command status supplies both the
	// discovery set and the clean/dirty baseline for the owning repository.
	repoStatuses := make(map[string]shellRepoStatus)
	if snap.gitRoot != "" && snap.gitStatus != nil {
		postStatus := gitStatusPorcelain(ctx, snap.gitRoot)
		repoStatuses[snap.gitRoot] = shellRepoStatus{paths: postStatus, available: postStatus != nil}
		for path := range postStatus {
			if hasGitAdminComponent(path) {
				continue
			}
			candidate := candidates[path]
			if candidate == nil {
				candidate = &shellPostCandidate{}
				candidates[path] = candidate
			}
			candidate.source |= shellSourceGit
		}
	}
	return candidates, repoStatuses
}

func resolveShellCandidateRepos(ctx context.Context, snap *shellSnapshot, candidates map[string]*shellPostCandidate, paths []string, repoStatuses map[string]shellRepoStatus) map[string]bool {
	// Resolve the deepest owning repository from post-command filesystem state.
	// This catches repositories cloned or created by the command and handles
	// nested repos/worktrees without invoking Git once per candidate.
	resolver := newShellRepoResolver()
	reposNeedingStatus := make(map[string]struct{})
	literalPathsByRepo := make(map[string][]string)
	for _, path := range paths {
		candidate := candidates[path]
		if hasGitAdminComponent(path) {
			delete(candidates, path)
			continue
		}
		candidate.repoRoot = resolver.owningRepoForCandidate(path, snap.workDir, snap.gitRoot)
		if candidate.repoRoot == "" {
			continue
		}
		if candidate.source&shellSourceLiteral != 0 && candidate.source&shellSourceSession == 0 {
			literalPathsByRepo[candidate.repoRoot] = append(literalPathsByRepo[candidate.repoRoot], path)
		}
		if !candidate.source.preservesCleanGitState() {
			if _, loaded := repoStatuses[candidate.repoRoot]; !loaded {
				reposNeedingStatus[candidate.repoRoot] = struct{}{}
			}
		}
	}

	statusRoots := sortedMapKeys(reposNeedingStatus)
	for i, root := range statusRoots {
		if ctx.Err() != nil {
			snap.coverageStatus = filetrack.CoverageUnavailable
			break
		}
		if i >= maxShellGitRepos {
			snap.coverageStatus = filetrack.CoverageTruncated
			break // unavailable status suppresses broad candidates in excess repos below
		}
		status := gitStatusPorcelain(ctx, root)
		repoStatuses[root] = shellRepoStatus{paths: status, available: status != nil}
	}

	// Exact literal affected_paths are deliberate snapshots even when the
	// command leaves Git clean, but ignored literals are still excluded. Batch
	// check-ignore once per owning repo rather than shelling out per path.
	ignoredLiterals := make(map[string]bool)
	literalRoots := sortedMapKeys(literalPathsByRepo)
	for i, root := range literalRoots {
		if ctx.Err() != nil {
			snap.coverageStatus = filetrack.CoverageUnavailable
			break
		}
		if i >= maxShellGitRepos {
			snap.coverageStatus = filetrack.CoverageTruncated
			break
		}
		for path := range gitIgnoredCandidates(ctx, root, literalPathsByRepo[root]) {
			ignoredLiterals[path] = true
		}
	}
	return ignoredLiterals
}

func filterShellCandidates(snap *shellSnapshot, candidates map[string]*shellPostCandidate, paths []string, repoStatuses map[string]shellRepoStatus, ignoredLiterals map[string]bool) []string {
	filteredPaths := paths[:0]
	for _, path := range paths {
		candidate := candidates[path]
		claimed := len(matchingOutputClaims(snap.claims, path)) > 0
		if candidate == nil || (ignoredLiterals[filepath.Clean(path)] && !claimed) {
			continue
		}
		if candidate.repoRoot != "" {
			state, statusLoaded := repoStatuses[candidate.repoRoot]
			if statusLoaded && state.available {
				candidate.postStatus = state.paths[path]
			}
			_, wasDirty := snap.gitStatus[path]
			knownDirtyBaseline := wasDirty && candidate.entry != nil && candidate.entry.existed
			if !claimed && !candidate.source.preservesCleanGitState() && !knownDirtyBaseline {
				if !statusLoaded || !state.available {
					// Repo caps and status failures fail closed for broad discovery:
					// emitting every changed glob match would turn a clean clone or
					// checkout into materialization noise. Exact literals, session
					// paths, and captured pre-dirty baselines remain preserved.
					continue
				}
				if candidate.postStatus == "" {
					// Globs are discovery scopes, so a Git-clean path at return is
					// intentionally omitted. Exact literals preserve edit+commit.
					continue
				}
			}
		}
		filteredPaths = append(filteredPaths, path)
	}
	return filteredPaths
}

func recoverShellIndexContent(ctx context.Context, snap *shellSnapshot, candidates map[string]*shellPostCandidate, paths []string) {
	// Clean tracked paths discovered only by the Git fallback were not statted
	// pre-command. Recover their index content in one bounded Git process rather
	// than invoking git show once per candidate.
	var indexPaths []string
	for _, path := range paths {
		candidate := candidates[path]
		_, wasDirty := snap.gitStatus[path]
		indexUnchanged := len(candidate.postStatus) == 2 && candidate.postStatus[0] == ' '
		if candidate.entry == nil && candidate.source&shellSourceGit != 0 &&
			candidate.repoRoot == snap.gitRoot && !wasDirty && indexUnchanged {
			indexPaths = append(indexPaths, path)
		}
	}
	indexContent := gitShowIndexBatch(ctx, snap.gitRoot, indexPaths, snap.maxFileBytes,
		maxShellContentReads-snap.postContentReads, maxShellSnapshotBytes-snap.postContentBytes)
	for path, content := range indexContent {
		candidate := candidates[path]
		candidate.gitBefore = content
		candidate.gitBeforeOK = true
		snap.notePostContentRead(content)
	}
}

func shellCandidateCoverageUncertain(snap *shellSnapshot, candidate *shellPostCandidate, matches []int, result *shellTrackingResult) bool {
	coverageUncertain := false
	for _, index := range matches {
		effectiveCoverage := worstShellCoverage(snap.claims[index].coverage, snap.coverageStatus)
		if candidate.postOnlyPattern && effectiveCoverage != filetrack.CoverageComplete {
			coverageUncertain = true
			result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{NormalizedPattern: snap.claims[index].pattern, ClaimKind: snap.claims[index].kind, Reason: "claim_unconfirmed_budget", CoverageStatus: effectiveCoverage, MatchingPathCount: 1, Message: "pre-command existence was outside complete claim coverage"})
		}
	}
	return coverageUncertain
}

func recordClaimedShellChange(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot, path string, rec *filetrack.ChangeRecord, claim normalizedOutputClaim, matchIndex int, kind string, result *shellTrackingResult) {
	switch claim.kind {
	case filetrack.ClaimMaterialize:
		persistShellObservation(ctx, recorder, rec, filetrack.ObservationMaterialized, result)
	case filetrack.ClaimTransform, filetrack.ClaimGenerate:
		if claim.kind == filetrack.ClaimTransform && kind == filetrack.KindCreate {
			result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{NormalizedPattern: claim.pattern, ClaimKind: claim.kind, Reason: "claim_mismatch", CoverageStatus: claim.coverage, MatchingPathCount: 1, Message: "transform claims do not cover creates: " + path})
			persistShellObservation(ctx, recorder, rec, filetrack.ObservationClaimMismatch, result)
			return
		}
		if claim.kind == filetrack.ClaimTransform {
			rec.Provenance = filetrack.ProvenanceDeclaredTransform
		} else {
			rec.Provenance = filetrack.ProvenanceDeclaredGenerate
		}
		explicit, ok := recorder.(AttributedFileRecorder)
		if !ok {
			result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{NormalizedPattern: claim.pattern, ClaimKind: claim.kind, Reason: "claim_unconfirmed_tracker_error", CoverageStatus: filetrack.CoverageUnavailable, Message: "file recorder does not support classified attribution"})
			return
		}
		fc, err := explicit.RecordAttributedChange(ctx, *rec)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{NormalizedPattern: claim.pattern, ClaimKind: claim.kind, Reason: "claim_unconfirmed_tracker_error", CoverageStatus: filetrack.CoverageUnavailable, Message: err.Error()})
		} else if fc != nil {
			result.FileChanges = append(result.FileChanges, *fc)
			snap.claims[matchIndex].confirmed++
		}
	}
}

func recordShellCandidate(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot, path string, candidate *shellPostCandidate, result *shellTrackingResult) {
	rec := snap.buildChangeRecord(path, candidate)
	if rec == nil {
		return
	}
	rec.ToolName = ShellToolName
	rec.ToolCallID = core.CallIDFromContext(ctx)
	rec.RunID = core.ToolRunIDFromContext(ctx)
	rec.ClaimCoverage = snap.coverageStatus
	rec.BaselineState = filetrack.BaselineUnknown
	if snap.gitStatus != nil && snap.gitStatus[path] != "" {
		rec.BaselineState = filetrack.BaselinePreexistingDirty
	}

	matches := matchingOutputClaims(snap.claims, path)
	coveredClaims := allMatchingOutputClaims(snap.claims, path)
	if shellCandidateCoverageUncertain(snap, candidate, matches, result) {
		return
	}
	for _, index := range coveredClaims {
		snap.claims[index].matched++
	}
	kind := transitionKind(*rec)
	if conflictingClaimKinds(snap.claims, matches) {
		diagnostic := claimDiagnosticForPath(snap.claims, matches, "claim_conflict", path)
		result.Diagnostics = append(result.Diagnostics, diagnostic)
		rec.ClaimPattern = diagnostic.NormalizedPattern
		rec.ClaimKind = diagnostic.ClaimKind
		persistShellObservation(ctx, recorder, rec, filetrack.ObservationClaimConflict, result)
		return
	}
	if len(matches) == 0 {
		persistShellObservation(ctx, recorder, rec, filetrack.ObservationUnclaimed, result)
		return
	}
	claim := snap.claims[matches[0]]
	rec.ClaimKind = claim.kind
	rec.ClaimPattern = claim.pattern
	rec.ClaimLiteral = claim.literal
	rec.ClaimCoverage = worstShellCoverage(claim.coverage, snap.coverageStatus)
	recordClaimedShellChange(ctx, recorder, snap, path, rec, claim, matches[0], kind, result)
}

func reportShellClaimOutcomes(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot, result *shellTrackingResult) {
	for _, claim := range snap.claims {
		coverage := worstShellCoverage(claim.coverage, snap.coverageStatus)
		if claim.confirmed > 0 {
			result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{NormalizedPattern: claim.pattern, ClaimKind: claim.kind, Reason: "claim_confirmed", CoverageStatus: coverage, MatchingPathCount: claim.confirmed})
		}
		if claim.matched == 0 {
			reason := "claim_noop"
			if coverage != filetrack.CoverageComplete {
				reason = "claim_unconfirmed_budget"
			}
			result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{NormalizedPattern: claim.pattern, ClaimKind: claim.kind, Reason: reason, CoverageStatus: coverage})
			claim.coverage = coverage
			persistClaimObservation(ctx, recorder, snap, claim, reason, result)
		}
	}
	if snap.coverageStatus != filetrack.CoverageComplete {
		persistCoverageObservation(ctx, recorder, snap, result)
	}
}

func postShellTracking(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot) shellTrackingResult {
	var result shellTrackingResult
	if snap == nil || recorder == nil {
		return result
	}
	// The exec context may have timed out; recording should still proceed after
	// already-applied filesystem mutations, but keep a short timeout so tracking
	// can never hang the shell tool indefinitely.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fileRecordTimeout)
	defer cancel()

	candidates, repoStatuses := collectShellPostCandidates(ctx, snap)
	paths := sortedMapKeys(candidates)
	ignoredLiterals := resolveShellCandidateRepos(ctx, snap, candidates, paths, repoStatuses)
	paths = filterShellCandidates(snap, candidates, paths, repoStatuses, ignoredLiterals)
	recoverShellIndexContent(ctx, snap, candidates, paths)

	for _, path := range paths {
		if ctx.Err() != nil {
			snap.coverageStatus = filetrack.CoverageUnavailable
			break
		}
		recordShellCandidate(ctx, recorder, snap, path, candidates[path], &result)
	}
	reportShellClaimOutcomes(ctx, recorder, snap, &result)
	flushShellObservations(ctx, recorder, &result)
	return result
}

func normalizeOutputClaims(workDir string, claims []OutputClaim) ([]normalizedOutputClaim, error) {
	seen := make(map[string]string)
	result := make([]normalizedOutputClaim, 0, len(claims))
	for _, raw := range claims {
		path := strings.TrimSpace(raw.Path)
		kind := strings.ToLower(strings.TrimSpace(raw.Kind))
		if path == "" {
			return nil, fmt.Errorf("output claim path must not be empty")
		}
		if kind != filetrack.ClaimTransform && kind != filetrack.ClaimGenerate && kind != filetrack.ClaimMaterialize {
			return nil, fmt.Errorf("output claim kind for %q must be transform, generate, or materialize", path)
		}
		if expanded, ok := expandTilde(path); ok {
			path = expanded
		} else {
			return nil, fmt.Errorf("cannot expand output claim path %q", path)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workDir, path)
		}
		path = filepath.Clean(path)
		if hasGitAdminComponent(path) {
			return nil, fmt.Errorf("output claims may not target Git administrative paths: %q", raw.Path)
		}
		pattern := filepath.ToSlash(path)
		literal := !strings.ContainsAny(pattern, "*?[{")
		if prior, ok := seen[pattern]; ok {
			if prior != kind {
				return nil, fmt.Errorf("conflicting output claim kinds for normalized pattern %q", pattern)
			}
			continue
		}
		seen[pattern] = kind
		result = append(result, normalizedOutputClaim{pattern: pattern, kind: kind, literal: literal, coverage: filetrack.CoverageComplete})
	}
	return result, nil
}

func validateOutputClaimAuthority(approval *ApprovalManager, workDir string, claims []normalizedOutputClaim) error {
	capabilities := []WorkspaceCapability(nil)
	if approval != nil {
		capabilities = approval.WorkspaceCapabilities()
	}
	for _, claim := range claims {
		path := filepath.FromSlash(claim.pattern)
		if !claim.literal {
			base, _ := doublestar.SplitPattern(claim.pattern)
			path = filepath.FromSlash(base)
		}
		path = filepath.Clean(path)
		allowed := pathWithinRoot(path, filepath.Clean(workDir))
		if len(capabilities) > 0 {
			allowed = allowed || approval.IsWorkspacePathAllowed(path, true)
		}
		if !allowed {
			return fmt.Errorf("output claim is outside the working directory or active write-capable workspaces: %q", claim.pattern)
		}
	}
	return nil
}

func allMatchingOutputClaims(claims []normalizedOutputClaim, path string) []int {
	path = filepath.ToSlash(filepath.Clean(path))
	matches := make([]int, 0, len(claims))
	for i, claim := range claims {
		if claim.literal {
			if path == claim.pattern {
				matches = append(matches, i)
			}
		} else if ok, _ := doublestar.Match(claim.pattern, path); ok {
			matches = append(matches, i)
		}
	}
	return matches
}

func matchingOutputClaims(claims []normalizedOutputClaim, path string) []int {
	path = filepath.ToSlash(filepath.Clean(path))
	var literals, globs []int
	for i, claim := range claims {
		if claim.literal {
			if path == claim.pattern {
				literals = append(literals, i)
			}
			continue
		}
		if ok, _ := doublestar.Match(claim.pattern, path); ok {
			globs = append(globs, i)
		}
	}
	if len(literals) > 0 {
		return literals
	}
	return globs
}

func conflictingClaimKinds(claims []normalizedOutputClaim, matches []int) bool {
	if len(matches) < 2 {
		return false
	}
	kind := claims[matches[0]].kind
	for _, index := range matches[1:] {
		if claims[index].kind != kind {
			return true
		}
	}
	return false
}

func transitionKind(rec filetrack.ChangeRecord) string {
	if rec.BeforeMissing {
		return filetrack.KindCreate
	}
	if rec.AfterMissing {
		return filetrack.KindDelete
	}
	return filetrack.KindModify
}

func claimDiagnosticForPath(claims []normalizedOutputClaim, matches []int, reason, path string) core.OutputClaimDiagnostic {
	patterns := make([]string, 0, len(matches))
	kinds := make([]string, 0, len(matches))
	coverage := filetrack.CoverageComplete
	for _, index := range matches {
		patterns = append(patterns, claims[index].pattern)
		kinds = append(kinds, claims[index].kind)
		coverage = worstShellCoverage(coverage, claims[index].coverage)
	}
	return core.OutputClaimDiagnostic{NormalizedPattern: strings.Join(patterns, ", "), ClaimKind: strings.Join(kinds, ", "), Reason: reason, CoverageStatus: coverage, MatchingPathCount: 1, Message: reason + " for " + path}
}

func worstShellCoverage(a, b string) string {
	rank := map[string]int{filetrack.CoverageComplete: 0, filetrack.CoverageTruncated: 1, filetrack.CoverageUnavailable: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func persistCoverageObservation(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot, result *shellTrackingResult) {
	explicit, ok := recorder.(FilesystemObservationRecorder)
	if !ok {
		return
	}
	persisted, err := explicit.RecordFilesystemObservation(ctx, filetrack.FilesystemObservation{
		SessionID: snap.sessionID, RunID: core.ToolRunIDFromContext(ctx), ToolName: ShellToolName,
		ToolCallID: core.CallIDFromContext(ctx), Classification: filetrack.ObservationIncomplete,
		Root: snap.workDir, CoverageStatus: snap.coverageStatus,
		Details: map[string]any{"version": 1, "reason": "observation_" + snap.coverageStatus},
	})
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{Reason: "claim_unconfirmed_tracker_error", CoverageStatus: filetrack.CoverageUnavailable, Message: err.Error()})
		return
	}
	if persisted != nil {
		result.Observations = append(result.Observations, *persisted)
	}
}

func persistClaimObservation(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot, claim normalizedOutputClaim, reason string, result *shellTrackingResult) {
	explicit, ok := recorder.(FilesystemObservationRecorder)
	if !ok {
		return
	}
	persisted, err := explicit.RecordFilesystemObservation(ctx, filetrack.FilesystemObservation{
		SessionID: snap.sessionID, RunID: core.ToolRunIDFromContext(ctx), ToolName: ShellToolName,
		ToolCallID: core.CallIDFromContext(ctx), Classification: filetrack.ObservationUnconfirmedClaim,
		Root: claim.pattern, CoverageStatus: claim.coverage,
		Details: map[string]any{"version": 1, "reason": reason, "claim_kind": claim.kind, "normalized_pattern": claim.pattern},
	})
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{NormalizedPattern: claim.pattern, ClaimKind: claim.kind, Reason: "claim_unconfirmed_tracker_error", CoverageStatus: filetrack.CoverageUnavailable, Message: err.Error()})
		return
	}
	if persisted != nil {
		result.Observations = append(result.Observations, *persisted)
	}
}

func persistShellObservation(_ context.Context, _ FileChangeRecorder, rec *filetrack.ChangeRecord, classification string, result *shellTrackingResult) {
	if result.pendingObservations == nil {
		result.pendingObservations = make(map[string]*filetrack.FilesystemObservation)
	}
	root := rec.ClaimPattern
	key := classification + "\x00" + root
	obs := result.pendingObservations[key]
	if obs == nil {
		obs = &filetrack.FilesystemObservation{SessionID: rec.SessionID, RunID: rec.RunID, ToolName: rec.ToolName,
			ToolCallID: rec.ToolCallID, Classification: classification, Root: root, CoverageStatus: rec.ClaimCoverage}
		if classification == filetrack.ObservationClaimMismatch || classification == filetrack.ObservationClaimConflict {
			obs.Details = map[string]any{"version": 1, "reason": classification, "claim_kind": rec.ClaimKind,
				"normalized_pattern": rec.ClaimPattern, "message": classification + " for declared output"}
		}
		if obs.CoverageStatus == "" {
			obs.CoverageStatus = filetrack.CoverageComplete
		}
		result.pendingObservations[key] = obs
	}
	switch transitionKind(*rec) {
	case filetrack.KindCreate:
		obs.CreatedCount++
	case filetrack.KindDelete:
		obs.DeletedCount++
	default:
		obs.ModifiedCount++
	}
	if len(obs.SampledPaths) < 100 {
		obs.SampledPaths = append(obs.SampledPaths, rec.Path)
	} else {
		obs.SamplesTruncated = true
	}
}

func flushShellObservations(ctx context.Context, recorder FileChangeRecorder, result *shellTrackingResult) {
	if len(result.pendingObservations) == 0 {
		return
	}
	explicit, ok := recorder.(FilesystemObservationRecorder)
	if !ok {
		result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{Reason: "claim_unconfirmed_tracker_error", CoverageStatus: filetrack.CoverageUnavailable, Message: "file recorder does not support observations"})
		return
	}
	keys := make([]string, 0, len(result.pendingObservations))
	for key := range result.pendingObservations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		persisted, err := explicit.RecordFilesystemObservation(ctx, *result.pendingObservations[key])
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, core.OutputClaimDiagnostic{Reason: "claim_unconfirmed_tracker_error", CoverageStatus: filetrack.CoverageUnavailable, Message: err.Error()})
			continue
		}
		if persisted != nil {
			result.Observations = append(result.Observations, *persisted)
		}
	}
	result.pendingObservations = nil
}

// buildChangeRecord compares one path's current state with its pre-exec state
// and returns the change to record, or nil when nothing changed (or change
// detection is impossible).
func (snap *shellSnapshot) buildChangeRecord(path string, candidate *shellPostCandidate) *filetrack.ChangeRecord {
	prev := candidate.entry
	info, statErr := os.Stat(path)
	existsNow := statErr == nil && info.Mode().IsRegular()
	if statErr == nil && !info.Mode().IsRegular() {
		return nil // directories, sockets, etc.
	}

	// For metadata-only snapshots, equal size and mtime are the only bounded
	// unchanged signal available. Captured content must still be byte-compared:
	// callers can preserve mtime while replacing same-size bytes.
	if prev != nil && prev.existed && prev.content == nil && existsNow && info.Size() == prev.size && info.ModTime().Equal(prev.modTime) {
		return nil
	}

	rec := &filetrack.ChangeRecord{SessionID: snap.sessionID, Path: path}

	// Establish the "before" side.
	switch {
	case prev != nil && !prev.existed:
		rec.BeforeMissing = true
	case prev != nil && prev.content != nil:
		rec.Before = prev.content
	case prev != nil:
		rec.BeforeUnknown = true
		rec.BeforeSizeHint = prev.size
	case candidate.postOnlyPattern:
		rec.BeforeMissing = true
	case candidate.gitBeforeOK:
		rec.Before = candidate.gitBefore
	case candidate.source&shellSourceGit != 0 && (candidate.postStatus == "??" || strings.HasPrefix(candidate.postStatus, "A")):
		rec.BeforeMissing = true
	default:
		rec.BeforeUnknown = true
	}

	// Establish the "after" side.
	if !existsNow {
		rec.AfterMissing = true
		if rec.BeforeMissing {
			return nil // never existed in either state
		}
		return rec
	}

	// A captured pre buffer always gets a post read when the file remains under
	// the per-file cap. These reads are transient and deliberately independent
	// of the bounded pre-snapshot and discretionary post-read budgets.
	mustCompareCaptured := prev != nil && prev.content != nil
	if info.Size() > int64(snap.maxFileBytes) {
		rec.AfterUnknown = true
		rec.AfterSizeHint = info.Size()
		return rec
	}
	if !mustCompareCaptured && !snap.canReadPostContent(info.Size()) {
		snap.coverageStatus = filetrack.CoverageTruncated
		rec.AfterUnknown = true
		rec.AfterSizeHint = info.Size()
		return rec
	}
	content, ok := readFileBounded(path, snap.maxFileBytes)
	if !ok {
		snap.coverageStatus = filetrack.CoverageUnavailable
		rec.AfterUnknown = true
		rec.AfterSizeHint = info.Size()
		return rec
	}
	if !mustCompareCaptured {
		snap.notePostContentRead(content)
	}
	rec.After = content

	if rec.Before != nil && bytes.Equal(rec.Before, rec.After) {
		return nil
	}
	return rec
}

// statAndMaybeRead captures one file's current state, reading content when it
// fits the per-file cap and the bounded pre-snapshot budget.
func (snap *shellSnapshot) statAndMaybeRead(path string) *shellSnapshotEntry {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return &shellSnapshotEntry{existed: false}
	}
	entry := &shellSnapshotEntry{existed: true, size: info.Size(), modTime: info.ModTime()}
	if !snap.canCapturePreContent(info.Size()) {
		if info.Size() <= int64(snap.maxFileBytes) {
			snap.coverageStatus = filetrack.CoverageTruncated
		}
		return entry
	}
	if content, ok := readFileBounded(path, snap.maxFileBytes); ok {
		snap.notePreContentRead(content)
		entry.content = content
	} else {
		snap.coverageStatus = filetrack.CoverageUnavailable
	}
	return entry
}

func readFileBounded(path string, maxBytes int) ([]byte, bool) {
	if maxBytes < 0 {
		return nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	return content, err == nil && len(content) <= maxBytes
}

func sortedMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
