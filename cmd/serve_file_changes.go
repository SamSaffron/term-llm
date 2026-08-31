package cmd

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/gitdiff"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	fileChangeScopeLastTurn    = "last_turn"
	fileChangeScopeLast3Turns  = "last_3_turns"
	fileChangeScopeUncommitted = string(gitdiff.ScopeUncommitted)
	fileChangeScopeUnstaged    = string(gitdiff.ScopeUnstaged)
	fileChangeScopeStaged      = string(gitdiff.ScopeStaged)
	fileChangeDefaultContext   = 3
	fileChangeMaxContext       = 100000
)

type fileChangeScopeSpec struct {
	name string
	runs int
}

var fileChangeScopeSpecs = [...]fileChangeScopeSpec{
	{name: fileChangeScopeLastTurn, runs: 1},
	{name: fileChangeScopeLast3Turns, runs: 3},
	{name: fileChangeScopeUncommitted},
	{name: fileChangeScopeUnstaged},
	{name: fileChangeScopeStaged},
}

func isMarkdownPath(path string) bool {
	ext := filepath.Ext(path)
	return strings.EqualFold(ext, ".md") || strings.EqualFold(ext, ".markdown")
}

func fileChangeLanguage(path string) string {
	name := strings.ToLower(filepath.Base(path))
	switch name {
	case "dockerfile", "containerfile":
		return "dockerfile"
	case "makefile", "gnumakefile":
		return "makefile"
	case "cmakelists.txt":
		return "cmake"
	case "jenkinsfile":
		return "groovy"
	case "gemfile", "rakefile", "guardfile", "vagrantfile", "podfile", "fastfile":
		return "ruby"
	case ".bashrc", ".bash_profile", ".zshrc", ".profile":
		return "bash"
	case "nginx.conf":
		return "nginx"
	case "apache2.conf", "httpd.conf":
		return "apache"
	case ".terraformrc", ".tofurc":
		return "terraform"
	case ".gitignore", ".dockerignore", ".prettierignore":
		return "plaintext"
	}
	if strings.HasPrefix(name, "dockerfile.") || strings.HasPrefix(name, "containerfile.") {
		return "dockerfile"
	}
	if strings.HasPrefix(name, "makefile.") || strings.HasPrefix(name, "gnumakefile.") {
		return "makefile"
	}
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return "ini"
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	switch ext {
	case "cjs", "mjs", "jsx":
		return "javascript"
	case "tsx":
		return "typescript"
	case "htm", "html", "xhtml", "svg", "vue", "svelte", "astro":
		return "xml"
	case "jsonc", "json5", "webmanifest":
		return "json"
	case "bash", "zsh":
		return "bash"
	case "kt", "kts":
		return "kotlin"
	case "ps1", "psm1", "psd1":
		return "powershell"
	case "gql":
		return "graphql"
	case "proto":
		return "protobuf"
	case "hbs", "mustache", "tmpl":
		return "handlebars"
	case "jinja", "jinja2":
		return "django"
	case "patch":
		return "diff"
	case "toml":
		return "toml"
	case "tf", "tfvars", "hcl":
		return "terraform"
	case "conf", "service", "timer", "socket", "mount", "target", "path", "slice", "automount", "network", "netdev", "link":
		return "ini"
	case "mdx":
		return "markdown"
	case "rake":
		return "ruby"
	case "mm", "objc":
		return "objectivec"
	case "fs", "fsi", "fsx":
		return "fsharp"
	case "ex", "exs":
		return "elixir"
	case "erl", "hrl":
		return "erlang"
	case "hs":
		return "haskell"
	case "jl":
		return "julia"
	case "coffee":
		return "coffeescript"
	case "gitignore", "dockerignore", "mod", "sum":
		return "plaintext"
	default:
		return ext
	}
}

func lookupFileChangeScope(value string) (fileChangeScopeSpec, bool) {
	for _, spec := range fileChangeScopeSpecs {
		if spec.name == value {
			return spec, true
		}
	}
	return fileChangeScopeSpec{}, false
}

func fileChangeScopeRunWindow(scope string) (int, bool) {
	spec, ok := lookupFileChangeScope(scope)
	return spec.runs, ok && spec.runs > 0
}

func fileChangeScopeNames() string {
	names := make([]string, 0, len(fileChangeScopeSpecs))
	for _, spec := range fileChangeScopeSpecs {
		names = append(names, spec.name)
	}
	return strings.Join(names, ", ")
}

func normalizeFileChangeScope(value string) (string, bool) {
	scope := strings.ToLower(strings.TrimSpace(value))
	if scope == "" {
		scope = fileChangeScopeLastTurn
	}
	_, ok := lookupFileChangeScope(scope)
	if !ok {
		return "", false
	}
	return scope, true
}

func requestedFileChangeScope(r *http.Request) (string, bool) {
	return normalizeFileChangeScope(r.URL.Query().Get("scope"))
}

func requestedFileChangeSnapshot(r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("snapshot_seq"))
	if raw == "" {
		return 0, true
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	return seq, err == nil && seq > 0
}

func requestedFileChangeContext(r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("context"))
	if raw == "" {
		return fileChangeDefaultContext, true
	}
	lines, err := strconv.Atoi(raw)
	return lines, err == nil && lines >= fileChangeDefaultContext && lines <= fileChangeMaxContext
}

type sessionGitRepoCacheEntry struct {
	repo      *gitdiff.Repo
	available bool
	checkedAt time.Time
}

const sessionGitRepoCacheTTL = 15 * time.Second

func (s *serveServer) sessionGitRepo(ctx context.Context, sessionID string) (*gitdiff.Repo, bool) {
	dir := strings.TrimSpace(s.startupDir)
	cacheKey := sessionID + "\x00" + dir
	var persisted *session.Session
	if s.store != nil {
		sess, err := s.store.Get(ctx, sessionID)
		if err != nil || sess == nil {
			return nil, false
		}
		persisted = sess
		cacheKey = sessionID + "\x00" + sess.ProjectID + "\x00" + sess.CWD + "\x00" + sess.WorktreeDir
	}
	s.fileChangeRepoCacheMu.Lock()
	if entry, ok := s.fileChangeRepoCache[cacheKey]; ok && time.Since(entry.checkedAt) < sessionGitRepoCacheTTL {
		s.fileChangeRepoCacheMu.Unlock()
		return entry.repo, entry.available
	}
	s.fileChangeRepoCacheMu.Unlock()

	if persisted != nil {
		if strings.TrimSpace(persisted.ProjectID) != "" {
			binding, err := s.resolvePersistedProjectWorkspace(ctx, *persisted)
			if err != nil {
				return nil, false
			}
			dir = strings.TrimSpace(binding.RuntimeDir)
			if dir == "" {
				dir = strings.TrimSpace(binding.RepoRoot)
			}
		} else if candidate := strings.TrimSpace(persisted.WorktreeDir); candidate != "" {
			dir = candidate
		} else if candidate := strings.TrimSpace(persisted.CWD); candidate != "" {
			dir = candidate
		}
	}
	repo, err := gitdiff.Open(ctx, dir)
	entry := sessionGitRepoCacheEntry{repo: repo, available: err == nil, checkedAt: time.Now()}
	s.fileChangeRepoCacheMu.Lock()
	if s.fileChangeRepoCache == nil {
		s.fileChangeRepoCache = make(map[string]sessionGitRepoCacheEntry)
	}
	if len(s.fileChangeRepoCache) >= 64 {
		var oldestKey string
		var oldest time.Time
		for key, cached := range s.fileChangeRepoCache {
			if oldestKey == "" || cached.checkedAt.Before(oldest) {
				oldestKey, oldest = key, cached.checkedAt
			}
		}
		delete(s.fileChangeRepoCache, oldestKey)
	}
	s.fileChangeRepoCache[cacheKey] = entry
	s.fileChangeRepoCacheMu.Unlock()
	return entry.repo, entry.available
}

// fileChangeSessionExists reports whether file-change history may be served
// for a session. Filetrack retention is independent of session pruning, so
// without this check stale diff content would stay retrievable by URL after
// its session was deleted (until the next GC sweep on store open). When the
// session store is unavailable (sessions disabled), existence cannot be
// verified and history is served as recorded.
func (s *serveServer) fileChangeSessionExists(ctx context.Context, sessionID string) bool {
	if s.store == nil {
		return true
	}
	sess, err := s.store.Get(ctx, sessionID)
	return err == nil && sess != nil
}

// handleSessionFileChanges serves GET /v1/sessions/{id}/file-changes. It
// defaults to the latest agent turn and accepts rolling turn and Git scopes
// exposed by the Changes selector.
func (s *serveServer) handleSessionFileChanges(w http.ResponseWriter, r *http.Request, sessionID string) {
	store := s.fileTrackStore()
	if store == nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "file tracking is not enabled")
		return
	}
	if !s.fileChangeSessionExists(r.Context(), sessionID) {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "session not found")
		return
	}

	scope, ok := requestedFileChangeScope(r)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "unsupported file change scope")
		return
	}
	runs, turnScope := fileChangeScopeRunWindow(scope)
	repo, isGit := s.sessionGitRepo(r.Context(), sessionID)
	var changes []filetrack.CumulativeChange
	var err error
	if turnScope {
		changes, err = store.ListRecentRunChanges(r.Context(), sessionID, runs)
	} else if !isGit {
		writeOpenAIError(w, http.StatusConflict, "invalid_request_error", "git file change scopes require a git repository")
		return
	} else {
		changes, err = repo.List(r.Context(), gitdiff.Scope(scope))
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load file changes")
		return
	}
	if changes == nil {
		changes = []filetrack.CumulativeChange{}
	}
	summary := map[string]any{"file_count": len(changes), "adds": 0, "dels": 0, "line_counts_unavailable_files": 0}
	for _, change := range changes {
		summary["adds"] = summary["adds"].(int) + change.Adds
		summary["dels"] = summary["dels"].(int) + change.Dels
		if turnScope && !change.ContentAvailable {
			summary["line_counts_unavailable_files"] = summary["line_counts_unavailable_files"].(int) + 1
		}
	}
	materializations := []filetrack.FilesystemObservation{}
	observationBatches := []filetrack.FilesystemObservation{}
	claimDiagnostics := []filetrack.OutputClaimDiagnostic{}
	coverage := filetrack.CoverageComplete
	observationTruncated := false
	totalCreated, totalModified, totalDeleted := 0, 0, 0
	if turnScope {
		runIDs, runErr := store.RecentRunIDs(r.Context(), sessionID, runs)
		if runErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load file tracking runs")
			return
		}
		batches, obsErr := store.ListRunObservations(r.Context(), sessionID, runIDs)
		if obsErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load filesystem observations")
			return
		}
		for _, batch := range batches {
			totalCreated += batch.CreatedCount
			totalModified += batch.ModifiedCount
			totalDeleted += batch.DeletedCount
			observationTruncated = observationTruncated || batch.SamplesTruncated
			if batch.CoverageStatus == filetrack.CoverageUnavailable || (batch.CoverageStatus == filetrack.CoverageTruncated && coverage == filetrack.CoverageComplete) {
				coverage = batch.CoverageStatus
			}
			if batch.Classification == filetrack.ObservationMaterialized {
				materializations = append(materializations, batch)
			} else {
				observationBatches = append(observationBatches, batch)
			}
			if batch.Classification == filetrack.ObservationUnconfirmedClaim || batch.Classification == filetrack.ObservationClaimMismatch || batch.Classification == filetrack.ObservationClaimConflict {
				reason := batch.Classification
				pattern, claimKind, message := "", "", ""
				if value, ok := batch.Details["reason"].(string); ok && value != "" {
					reason = value
				}
				if value, ok := batch.Details["normalized_pattern"].(string); ok {
					pattern = value
				}
				if value, ok := batch.Details["claim_kind"].(string); ok {
					claimKind = value
				}
				if value, ok := batch.Details["message"].(string); ok {
					message = value
				}
				claimDiagnostics = append(claimDiagnostics, filetrack.OutputClaimDiagnostic{NormalizedPattern: pattern, ClaimKind: claimKind, Reason: reason, CoverageStatus: batch.CoverageStatus, MatchingPathCount: batch.CreatedCount + batch.ModifiedCount + batch.DeletedCount, Message: message})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file_changes": changes, "file_change_summary": summary, "git": isGit, "scope": scope,
		"snapshot_token": store.CurrentSnapshotToken(r.Context(), sessionID), "materializations": materializations,
		"observations":      map[string]any{"total_created": totalCreated, "total_modified": totalModified, "total_deleted": totalDeleted, "batches": observationBatches, "truncated": observationTruncated, "coverage_status": coverage},
		"claim_diagnostics": claimDiagnostics,
	})
}

// handleSessionFileChangeDiff serves GET /v1/sessions/{id}/file-changes/diff?path=…:
// structured hunks for one file's baseline→current diff, computed from the
// recorded blobs (not live disk, so history stays inspectable after the fact).
func (s *serveServer) handleSessionFileChangeDiff(w http.ResponseWriter, r *http.Request, sessionID string) {
	store := s.fileTrackStore()
	if store == nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "file tracking is not enabled")
		return
	}
	if !s.fileChangeSessionExists(r.Context(), sessionID) {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "session not found")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "path query parameter is required")
		return
	}

	scope, ok := requestedFileChangeScope(r)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "unsupported file change scope")
		return
	}
	snapshotSeq, ok := requestedFileChangeSnapshot(r)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "snapshot_seq must be a positive integer")
		return
	}
	contextLines, ok := requestedFileChangeContext(r)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "context must be between 3 and 100000 lines")
		return
	}
	var content *filetrack.FileDiffContent
	var err error
	if runs, ok := fileChangeScopeRunWindow(scope); ok {
		content, err = store.GetRecentRunFileDiffContent(r.Context(), sessionID, path, runs, snapshotSeq)
	} else if repo, isGit := s.sessionGitRepo(r.Context(), sessionID); !isGit {
		writeOpenAIError(w, http.StatusConflict, "invalid_request_error", "git file change scopes require a git repository")
		return
	} else {
		content, err = repo.File(r.Context(), gitdiff.Scope(scope), path)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load file diff")
		return
	}
	if content == nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "no recorded changes for path")
		return
	}

	hunks := []filetrack.Hunk{}
	if !content.Truncated && !content.IsImage {
		if built := filetrack.BuildHunksWithContext(content.Path, content.Before, content.After, contextLines); built != nil {
			hunks = built
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":              content.Path,
		"kind":              content.Kind,
		"lang":              fileChangeLanguage(content.Path),
		"truncated":         content.Truncated,
		"content_status":    content.ContentStatus,
		"content_available": content.ContentAvailable,
		"provenance":        content.Provenance,
		"baseline_state":    content.BaselineState,
		"claim_coverage":    content.ClaimCoverage,
		"image":             content.IsImage,
		"context":           contextLines,
		"old_line_count":    filetrack.LineCount(content.Before),
		"new_line_count":    filetrack.LineCount(content.After),
		"hunks":             hunks,
	})
}

// handleSessionFileChangeContent serves one retained side of an image diff or
// Markdown source. Retained turn content comes from session-scoped blob history;
// Git-scoped Markdown comes only from a changed path in the bound repository.
func (s *serveServer) handleSessionFileChangeContent(w http.ResponseWriter, r *http.Request, sessionID string) {
	store := s.fileTrackStore()
	if store == nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "file tracking is not enabled")
		return
	}
	if !s.fileChangeSessionExists(r.Context(), sessionID) {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "session not found")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "path query parameter is required")
		return
	}
	side := r.URL.Query().Get("side")
	if side != "before" && side != "after" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "side query parameter must be before or after")
		return
	}

	scope, ok := requestedFileChangeScope(r)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "unsupported file change scope")
		return
	}
	snapshotSeq, ok := requestedFileChangeSnapshot(r)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "snapshot_seq must be a positive integer")
		return
	}

	if isMarkdownPath(path) {
		var data []byte
		available := false
		if runs, turnScope := fileChangeScopeRunWindow(scope); turnScope {
			content, err := store.GetRecentRunFileDiffTextSide(r.Context(), sessionID, path, side, runs, snapshotSeq)
			if errors.Is(err, filetrack.ErrInvalidDiffSide) {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "requested side is not available for this change")
				return
			}
			if err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load file diff content")
				return
			}
			if content != nil {
				data = content.Data
				available = true
			}
		} else if repo, isGit := s.sessionGitRepo(r.Context(), sessionID); !isGit {
			writeOpenAIError(w, http.StatusConflict, "invalid_request_error", "git file change scopes require a git repository")
			return
		} else {
			content, err := repo.File(r.Context(), gitdiff.Scope(scope), path)
			if errors.Is(err, gitdiff.ErrPathOutsideRepository) {
				writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "Markdown source is not available")
				return
			}
			if err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load file diff content")
				return
			}
			if content != nil {
				needBefore, needAfter := content.Kind != filetrack.KindCreate, content.Kind != filetrack.KindDelete
				if (side == "before" && !needBefore) || (side == "after" && !needAfter) {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "requested side is not available for this change")
					return
				}
				if !content.Truncated {
					data = content.Before
					if side == "after" {
						data = content.After
					}
					available = filetrack.IsRenderableText(data)
				}
			}
		}
		if !available {
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "Markdown source is not available")
			return
		}
		writeFileChangeContent(w, "text/plain; charset=utf-8", data)
		return
	}

	runs, turnScope := fileChangeScopeRunWindow(scope)
	if !turnScope {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "image diff content is not available for git scopes")
		return
	}
	content, err := store.GetRecentRunFileDiffSide(r.Context(), sessionID, path, side, runs, snapshotSeq)
	if errors.Is(err, filetrack.ErrInvalidDiffSide) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "requested side is not available for this change")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load file diff content")
		return
	}
	if content == nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "image diff content is not available")
		return
	}
	writeFileChangeContent(w, content.MediaType, content.Data)
}

func writeFileChangeContent(w http.ResponseWriter, mediaType string, data []byte) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	uiAddVary(w.Header(), "Authorization")
	uiAddVary(w.Header(), "Cookie")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
