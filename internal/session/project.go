package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrProjectsUnsupported indicates that the store predates or does not expose
	// durable project support.
	ErrProjectsUnsupported = errors.New("session: projects unsupported")
	// ErrProjectDuplicate indicates that a canonical project directory already
	// has a stable identity.
	ErrProjectDuplicate = errors.New("session: project already exists")
	// ErrWorkspaceConflict indicates that an immutable session workspace was
	// already bound to different values.
	ErrWorkspaceConflict = errors.New("session: workspace conflict")
)

func NewProjectID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate project id: %w", err)
	}
	return "prj_" + hex.EncodeToString(raw[:]), nil
}

// Project is durable operator-managed metadata. CanonicalDir is immutable;
// Session.CWD and Session.WorktreeDir remain the execution snapshot.
type Project struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	CanonicalDir      string     `json:"canonical_dir"`
	IsBootstrap       bool       `json:"is_bootstrap,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	LastUsedAt        time.Time  `json:"last_used_at"`
	ArchivedAt        *time.Time `json:"archived_at,omitempty"`
	ConversationCount int        `json:"conversation_count"`
	Available         bool       `json:"available"`
	Git               bool       `json:"git"`
	UnavailableReason string     `json:"unavailable_reason,omitempty"`
}

func (p Project) Archived() bool { return p.ArchivedAt != nil }

type ProjectListOptions struct {
	IncludeArchived bool
}

// ProjectUpdate deliberately excludes CanonicalDir; project roots are immutable.
type ProjectUpdate struct {
	Name     *string
	Archived *bool
}

type ProjectSessionCursor struct {
	ProjectID  string    `json:"g"`
	Pinned     bool      `json:"p"`
	ActivityAt time.Time `json:"a"`
	Number     int64     `json:"n"`
}

func EncodeProjectSessionCursor(summary SessionSummary) string {
	activity := summary.LastMessageAt
	if activity.IsZero() {
		activity = summary.CreatedAt
	}
	data, _ := json.Marshal(ProjectSessionCursor{ProjectID: summary.ProjectID, Pinned: summary.Pinned, ActivityAt: activity, Number: summary.Number})
	return base64.RawURLEncoding.EncodeToString(data)
}

func DecodeProjectSessionCursor(value string) (ProjectSessionCursor, error) {
	var cursor ProjectSessionCursor
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, fmt.Errorf("decode project cursor: %w", err)
	}
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Number <= 0 || cursor.ActivityAt.IsZero() {
		return ProjectSessionCursor{}, fmt.Errorf("invalid project cursor")
	}
	return cursor, nil
}

// SidebarGroup is the bounded server-side projection used by the Web UI.
type SidebarGroup struct {
	Project      *Project         `json:"project,omitempty"`
	SessionCount int              `json:"session_count"`
	LastActivity time.Time        `json:"last_activity_at,omitempty"`
	Sessions     []SessionSummary `json:"sessions"`
	NextCursor   string           `json:"next_cursor,omitempty"`
	NoProject    bool             `json:"no_project,omitempty"`
}

type SidebarOptions struct {
	PerProject              int
	IncludeArchivedProjects bool
	IncludeArchivedSessions bool
}

// ProjectReader is the read-only capability used to restore immutable project
// sessions even when project mutation mode is unavailable (for example, a
// read-only SQLite deployment that auto-disabled the project UI).
type ProjectReader interface {
	GetProject(ctx context.Context, id string) (*Project, error)
}

func AsProjectReader(store Store) (ProjectReader, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		return AsProjectReader(logging.Store)
	}
	if sqlite, ok := store.(*SQLiteStore); ok {
		if !sqlite.hasProjectID || !sqlite.hasProjectsTable {
			return nil, false
		}
		return sqlite, true
	}
	reader, ok := store.(ProjectReader)
	return reader, ok
}

// ProjectStore is optional so custom and read-only pre-migration stores remain
// usable without falsely advertising project support.
type ProjectStore interface {
	ListProjects(ctx context.Context, opts ProjectListOptions) ([]Project, error)
	GetProject(ctx context.Context, id string) (*Project, error)
	GetProjectByCanonicalDir(ctx context.Context, canonicalDir string) (*Project, error)
	CreateProject(ctx context.Context, project *Project) error
	UpdateProject(ctx context.Context, id string, update ProjectUpdate) (*Project, error)
	BootstrapProject(ctx context.Context, project *Project, matchingSessionIDs []string) error
	Sidebar(ctx context.Context, opts SidebarOptions) ([]SidebarGroup, error)
	AssignSessionProject(ctx context.Context, sessionID, projectID, expectedCWD, expectedWorktreeDir string) error
}

func AsProjectStore(store Store) (ProjectStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		underlying, supported := AsProjectStore(logging.Store)
		if !supported {
			return nil, false
		}
		return &loggingProjectStore{logger: logging, store: underlying}, true
	}
	if sqlite, ok := store.(*SQLiteStore); ok && !sqlite.projectsAvailable() {
		return nil, false
	}
	projects, ok := store.(ProjectStore)
	return projects, ok
}

type loggingProjectStore struct {
	logger *LoggingStore
	store  ProjectStore
}

func (s *loggingProjectStore) log(op string, err error) {
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrProjectDuplicate) {
		s.logger.logOnce(op, err)
	}
}

func (s *loggingProjectStore) ListProjects(ctx context.Context, opts ProjectListOptions) ([]Project, error) {
	v, err := s.store.ListProjects(ctx, opts)
	s.log("ListProjects", err)
	return v, err
}
func (s *loggingProjectStore) GetProject(ctx context.Context, id string) (*Project, error) {
	v, err := s.store.GetProject(ctx, id)
	s.log("GetProject", err)
	return v, err
}
func (s *loggingProjectStore) GetProjectByCanonicalDir(ctx context.Context, dir string) (*Project, error) {
	v, err := s.store.GetProjectByCanonicalDir(ctx, dir)
	s.log("GetProjectByCanonicalDir", err)
	return v, err
}
func (s *loggingProjectStore) CreateProject(ctx context.Context, p *Project) error {
	err := s.store.CreateProject(ctx, p)
	s.log("CreateProject", err)
	return err
}
func (s *loggingProjectStore) UpdateProject(ctx context.Context, id string, update ProjectUpdate) (*Project, error) {
	v, err := s.store.UpdateProject(ctx, id, update)
	s.log("UpdateProject", err)
	return v, err
}
func (s *loggingProjectStore) BootstrapProject(ctx context.Context, p *Project, ids []string) error {
	err := s.store.BootstrapProject(ctx, p, ids)
	s.log("BootstrapProject", err)
	return err
}
func (s *loggingProjectStore) Sidebar(ctx context.Context, opts SidebarOptions) ([]SidebarGroup, error) {
	v, err := s.store.Sidebar(ctx, opts)
	s.log("Sidebar", err)
	return v, err
}
func (s *loggingProjectStore) AssignSessionProject(ctx context.Context, sid, pid, cwd, worktreeDir string) error {
	err := s.store.AssignSessionProject(ctx, sid, pid, cwd, worktreeDir)
	s.log("AssignSessionProject", err)
	return err
}

// SessionWorkspaceBinding is committed atomically after request validation.
type SessionWorkspaceBinding struct {
	ProjectID   string
	CWD         string
	WorktreeDir string
}

// SessionWorkspaceBinder provides first-writer-wins immutable binding.
type SessionWorkspaceBinder interface {
	BindSessionWorkspace(ctx context.Context, sessionID string, binding SessionWorkspaceBinding) (*Session, error)
}

func AsSessionWorkspaceBinder(store Store) (SessionWorkspaceBinder, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		binder, supported := AsSessionWorkspaceBinder(logging.Store)
		if !supported {
			return nil, false
		}
		return &loggingWorkspaceBinder{logger: logging, binder: binder}, true
	}
	if sqlite, ok := store.(*SQLiteStore); ok && (!sqlite.hasProjectID || sqlite.cfg.ReadOnly) {
		return nil, false
	}
	binder, ok := store.(SessionWorkspaceBinder)
	return binder, ok
}

type loggingWorkspaceBinder struct {
	logger *LoggingStore
	binder SessionWorkspaceBinder
}

func (s *loggingWorkspaceBinder) BindSessionWorkspace(ctx context.Context, id string, binding SessionWorkspaceBinding) (*Session, error) {
	v, err := s.binder.BindSessionWorkspace(ctx, id, binding)
	if err != nil && !errors.Is(err, ErrWorkspaceConflict) && !errors.Is(err, ErrNotFound) {
		s.logger.logOnce("BindSessionWorkspace", err)
	}
	return v, err
}
