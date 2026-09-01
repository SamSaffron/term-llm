package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/llm"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// ErrNotFound is returned when a lookup or update targets a row that does not
// exist (e.g., UpdateMessage against a deleted/never-persisted message ID).
var ErrNotFound = errors.New("session: not found")

// ErrTranscriptRevisionUnsupported indicates that a store does not support a
// mutation method that reports the exact transcript revision it commits.
var ErrTranscriptRevisionUnsupported = errors.New("session: transcript revision unsupported")

var (
	// ErrTranscriptConflict means a transcript mutation was based on a stale
	// revision or head row and must be retried after refreshing the transcript.
	ErrTranscriptConflict = errors.New("session: transcript changed")
	// Branch errors describe unavailable schema support and stale optimistic state.
	ErrBranchingUnsupported = errors.New("session: conversation branching unsupported")
	// ErrBranchConflict is branch-specific while matching the shared transcript
	// conflict sentinel for callers that handle all optimistic transcript races.
	ErrBranchConflict = fmt.Errorf("session: branch conflict: %w", ErrTranscriptConflict)
	// ErrBranchIdempotencyConflict means a key was reused for a different source prefix.
	ErrBranchIdempotencyConflict = errors.New("session: branch idempotency key reused with different parameters")
	// ErrNothingToUndo means there is no real post-compaction user turn to undo.
	ErrNothingToUndo = errors.New("session: nothing to undo")
	// ErrNothingToRedo means no durable undo suffix remains for this session.
	ErrNothingToRedo = errors.New("session: nothing to redo")
)

// Store is the interface for session persistence.
type Store interface {
	// Session CRUD
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	GetByNumber(ctx context.Context, number int64) (*Session, error)
	GetByPrefix(ctx context.Context, prefix string) (*Session, error)
	Update(ctx context.Context, s *Session) error
	MarkTitleSkipped(ctx context.Context, id string, t time.Time) error
	Delete(ctx context.Context, id string) error

	// Listing and search
	List(ctx context.Context, opts ListOptions) ([]SessionSummary, error)
	Search(ctx context.Context, opts SearchOptions) ([]SearchResult, error)

	// Message operations - stores full llm.Message with Parts
	AddMessage(ctx context.Context, sessionID string, msg *Message) error
	// UpdateMessage replaces the content of an existing message (by msg.ID) with
	// the supplied msg (role, parts, text, duration, sequence are updated in
	// place). Used for "persist as we go" upserts of an in-progress assistant
	// message during streaming. Returns ErrNotFound if the row does not exist.
	UpdateMessage(ctx context.Context, sessionID string, msg *Message) error
	GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error)
	// GetMessagesFrom returns rows at/after fromSeq in sequence order. When limit
	// <= 0, all remaining rows are returned.
	GetMessagesFrom(ctx context.Context, sessionID string, fromSeq, limit int) ([]Message, error)
	// GetMessageByID retrieves a single message by its global message id.
	GetMessageByID(ctx context.Context, msgID int64) (*Message, error)
	ReplaceMessages(ctx context.Context, sessionID string, messages []Message) error
	CompactMessages(ctx context.Context, sessionID string, messages []Message) error

	// Metrics operations (for incremental session saving)
	UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int) error
	UpdateContextEstimate(ctx context.Context, id string, lastTotalTokens, lastMessageCount int) error
	UpdateStatus(ctx context.Context, id string, status SessionStatus) error
	IncrementUserTurns(ctx context.Context, id string) error

	// Current session tracking (for auto-resume)
	SetCurrent(ctx context.Context, sessionID string) error
	GetCurrent(ctx context.Context) (*Session, error)
	ClearCurrent(ctx context.Context) error

	// Push subscription management (for web push notifications)
	SavePushSubscription(ctx context.Context, sub *PushSubscription) error
	DeletePushSubscription(ctx context.Context, endpoint string) error
	ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error)

	// Lifecycle
	Close() error
}

// StoreChange is one durable, monotonically ordered coarse mutation emitted by
// SQLite triggers. It lets other processes observe shared-store changes without
// rescanning the session catalog.
type StoreChange struct {
	Sequence      int64
	Kind          string
	SessionID     string
	ProjectID     string
	TranscriptRev int64
	Status        SessionStatus
}

const (
	StoreChangeSessionCreated           = "session.created"
	StoreChangeSessionDeleted           = "session.deleted"
	StoreChangeSessionMetadataChanged   = "session.metadata_changed"
	StoreChangeSessionTranscriptChanged = "session.transcript_changed"
	StoreChangeSessionStatusChanged     = "session.status_changed"
	StoreChangeSessionLifecycleChanged  = "session.lifecycle_changed"
	StoreChangeSessionAttentionChanged  = "session.attention_changed"
	StoreChangeProjectMembershipChanged = "project.membership_changed"
	StoreChangeProjectCreated           = "project.created"
	StoreChangeProjectUpdated           = "project.updated"
	StoreChangeProjectDeleted           = "project.deleted"
)

// StoreChangeStore is an optional capability implemented by stores with a
// durable indexed mutation cursor. The serve event watcher uses it for
// cross-process/TUI observation; stores without it rely on explicit events.
type StoreChangeStore interface {
	StoreChangeCursor(ctx context.Context) (int64, error)
	ListStoreChanges(ctx context.Context, after int64, limit int) ([]StoreChange, error)
}

// AsStoreChangeStore resolves the optional capability through decorators
// without making unsupported stores appear observable.
func AsStoreChangeStore(store Store) (StoreChangeStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		return AsStoreChangeStore(logging.Store)
	}
	changeStore, ok := store.(StoreChangeStore)
	return changeStore, ok
}

// ResponseRunState is the durable lifecycle state of a serve-origin web run.
type ResponseRunState string

const (
	ResponseRunRunning   ResponseRunState = "running"
	ResponseRunCompleted ResponseRunState = "completed"
	ResponseRunFailed    ResponseRunState = "failed"
	ResponseRunCancelled ResponseRunState = "cancelled"
	ResponseRunOrphaned  ResponseRunState = "orphaned"
)

var (
	ErrResponseRunLeaseLost = errors.New("session: response run lease lost")
	ErrAttentionConflict    = errors.New("session: attention generation conflict")
)

// ResponseRunAdmission durably accounts for a run before provider work starts.
type ResponseRunAdmission struct {
	ResponseID      string
	SessionID       string
	RunEpoch        int64
	OwnerInstanceID string
	StartedRev      int64
	StartedAt       time.Time
	LeaseDuration   time.Duration
}

type ResponseRunLease struct {
	ResponseID     string
	FencingToken   int64
	LeaseExpiresAt time.Time
}

type ResponseRunCheckpoint struct {
	ResponseID         string
	OwnerInstanceID    string
	FencingToken       int64
	FinalRev           int64
	DurableOutputCount int
}

// ResponseRunFence travels with response-scoped transcript writes. SQLite
// validates and checkpoints it in the same transaction as the transcript
// mutation so a recovered/stale owner cannot commit after losing ownership.
type ResponseRunFence struct {
	ResponseID         string
	OwnerInstanceID    string
	FencingToken       int64
	DurableOutputCount int
}

type responseRunFenceContextKey struct{}

func WithResponseRunFence(ctx context.Context, fence ResponseRunFence) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, responseRunFenceContextKey{}, fence)
}

func ResponseRunFenceFromContext(ctx context.Context) (ResponseRunFence, bool) {
	if ctx == nil {
		return ResponseRunFence{}, false
	}
	fence, ok := ctx.Value(responseRunFenceContextKey{}).(ResponseRunFence)
	return fence, ok && fence.ResponseID != "" && fence.OwnerInstanceID != "" && fence.FencingToken > 0
}

type ResponseRunTerminal struct {
	ResponseID         string
	OwnerInstanceID    string
	FencingToken       int64
	Outcome            ResponseRunState
	FinalRev           int64
	DurableOutputCount int
	EndedAt            time.Time
}

// AttentionState is the authoritative per-session terminal-attention watermark.
type AttentionState struct {
	StoreInstanceID    string           `json:"store_instance_id"`
	SessionID          string           `json:"session_id"`
	LatestAttentionSeq int64            `json:"latest_attention_seq"`
	ResponseID         string           `json:"response_id,omitempty"`
	RunEpoch           int64            `json:"run_epoch,omitempty"`
	Outcome            ResponseRunState `json:"outcome,omitempty"`
	StartedRev         int64            `json:"started_rev,omitempty"`
	FinalRev           int64            `json:"final_rev,omitempty"`
	TerminalAt         time.Time        `json:"terminal_at,omitempty"`
	SeenThroughSeq     int64            `json:"seen_through_seq"`
	SeenAt             time.Time        `json:"seen_at,omitempty"`
	Unseen             bool             `json:"attention_unseen"`
	Changed            bool             `json:"-"`
}

type AttentionKind string

const (
	AttentionKindUnseen        AttentionKind = "unseen"
	AttentionKindRunning       AttentionKind = "running"
	AttentionKindInputRequired AttentionKind = "input_required"
)

type AttentionListOptions struct {
	Kind            AttentionKind
	Limit           int
	Cursor          string
	SnapshotVersion int64
}

type AttentionItem struct {
	SessionID                string           `json:"session_id"`
	SessionNumber            int64            `json:"session_number,omitempty"`
	ResponseID               string           `json:"response_id"`
	Kind                     AttentionKind    `json:"kind"`
	LifecycleState           ResponseRunState `json:"lifecycle_state"`
	AttentionSeq             int64            `json:"attention_seq,omitempty"`
	StartedRev               int64            `json:"started_rev,omitempty"`
	FinalRev                 int64            `json:"final_rev,omitempty"`
	ShortTitle               string           `json:"short_title,omitempty"`
	LongTitle                string           `json:"long_title,omitempty"`
	ProjectID                string           `json:"project_id,omitempty"`
	Outcome                  ResponseRunState `json:"outcome,omitempty"`
	StartedAt                time.Time        `json:"started_at,omitempty"`
	TerminalAt               time.Time        `json:"terminal_at,omitempty"`
	LeaseExpiresAt           time.Time        `json:"lease_expires_at,omitempty"`
	InteractionRequired      bool             `json:"interaction_required,omitempty"`
	InteractionStateRev      int64            `json:"interaction_state_rev,omitempty"`
	PendingInteractionCount  int              `json:"pending_interaction_count,omitempty"`
	PendingInteractionKinds  []string         `json:"pending_interaction_kinds,omitempty"`
	InteractionRequiredSince time.Time        `json:"interaction_required_since,omitempty"`
}

type AttentionPage struct {
	ProtocolVersion int             `json:"protocol_version"`
	StoreInstanceID string          `json:"store_instance_id"`
	SnapshotVersion int64           `json:"snapshot_version"`
	Items           []AttentionItem `json:"items"`
	NextCursor      string          `json:"next_cursor,omitempty"`
	HasMore         bool            `json:"has_more"`
}

// ServeResponseLifecycleStore is optional so custom/read-only stores remain usable.
type ServeResponseLifecycleStore interface {
	AdmitResponseRun(context.Context, ResponseRunAdmission) (ResponseRunLease, error)
	RenewResponseRunLease(context.Context, string, string, int64) (ResponseRunLease, error)
	ValidateResponseRunLease(context.Context, string, string, int64) error
	CheckpointResponseRun(context.Context, ResponseRunCheckpoint) error
	FinalizeResponseRun(context.Context, ResponseRunTerminal) (AttentionState, error)
	RecoverExpiredResponseRuns(context.Context, int) ([]AttentionState, error)
}

// ResponseRunInteractionState is the level-triggered, payload-free projection of
// actionable interactions currently blocking one response run.
type ResponseRunInteractionState struct {
	ResponseID      string
	OwnerInstanceID string
	FencingToken    int64
	Revision        int64
	Count           int
	Kinds           []string
	RequiredSince   time.Time
}

// ResponseRunInteractionStore is optional so older/custom lifecycle stores keep
// their existing running and terminal-attention capabilities.
type ResponseRunInteractionStore interface {
	SetResponseRunInteractionState(context.Context, ResponseRunInteractionState) error
}

// AttentionStore owns durable terminal markers and exact-sequence acknowledgements.
type AttentionStore interface {
	MarkAttentionSeen(context.Context, string, string, int64) (AttentionState, error)
	GetAttention(context.Context, string) (AttentionState, error)
	ListAttention(context.Context, AttentionListOptions) (AttentionPage, error)
	StoreInstanceID(context.Context) (string, error)
}

// AttentionBatchStore avoids per-session queries on bounded sidebar/status projections.
type AttentionBatchStore interface {
	GetAttentionBatch(context.Context, []string) (map[string]AttentionState, error)
}

func AsServeResponseLifecycleStore(store Store) (ServeResponseLifecycleStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		underlying, supported := AsServeResponseLifecycleStore(logging.Store)
		if !supported {
			return nil, false
		}
		return &loggingServeResponseLifecycleStore{logger: logging, store: underlying}, true
	}
	if sqlite, ok := store.(*SQLiteStore); ok && sqlite.cfg.ReadOnly {
		return nil, false
	}
	result, ok := store.(ServeResponseLifecycleStore)
	return result, ok
}

func AsResponseRunInteractionStore(store Store) (ResponseRunInteractionStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		underlying, supported := AsResponseRunInteractionStore(logging.Store)
		if !supported {
			return nil, false
		}
		return &loggingResponseRunInteractionStore{logger: logging, store: underlying}, true
	}
	if sqlite, ok := store.(*SQLiteStore); ok && sqlite.cfg.ReadOnly {
		return nil, false
	}
	result, ok := store.(ResponseRunInteractionStore)
	return result, ok
}

func AsAttentionStore(store Store) (AttentionStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		underlying, supported := AsAttentionStore(logging.Store)
		if !supported {
			return nil, false
		}
		return &loggingAttentionStore{logger: logging, store: underlying}, true
	}
	if sqlite, ok := store.(*SQLiteStore); ok && sqlite.cfg.ReadOnly {
		return nil, false
	}
	result, ok := store.(AttentionStore)
	return result, ok
}

func AsAttentionBatchStore(store Store) (AttentionBatchStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		underlying, supported := AsAttentionBatchStore(logging.Store)
		if !supported {
			return nil, false
		}
		return &loggingAttentionBatchStore{logger: logging, store: underlying}, true
	}
	if sqlite, ok := store.(*SQLiteStore); ok && sqlite.cfg.ReadOnly {
		return nil, false
	}
	result, ok := store.(AttentionBatchStore)
	return result, ok
}

// SupportsAtomicResponseRunTranscriptFencing reports whether response-scoped
// transcript writes validate and checkpoint their fence in the write transaction.
func SupportsAtomicResponseRunTranscriptFencing(store Store) bool {
	if logging, ok := store.(*LoggingStore); ok {
		return SupportsAtomicResponseRunTranscriptFencing(logging.Store)
	}
	sqlite, ok := store.(*SQLiteStore)
	return ok && !sqlite.cfg.ReadOnly
}

type loggingAttentionBatchStore struct {
	logger *LoggingStore
	store  AttentionBatchStore
}

func (s *loggingAttentionBatchStore) GetAttentionBatch(ctx context.Context, ids []string) (map[string]AttentionState, error) {
	result, err := s.store.GetAttentionBatch(ctx, ids)
	if err != nil {
		s.logger.logOnce("GetAttentionBatch", err)
	}
	return result, err
}

type loggingResponseRunInteractionStore struct {
	logger *LoggingStore
	store  ResponseRunInteractionStore
}

func (s *loggingResponseRunInteractionStore) SetResponseRunInteractionState(ctx context.Context, value ResponseRunInteractionState) error {
	err := s.store.SetResponseRunInteractionState(ctx, value)
	if err != nil && !errors.Is(err, ErrResponseRunLeaseLost) {
		s.logger.logOnce("SetResponseRunInteractionState", err)
	}
	return err
}

type loggingServeResponseLifecycleStore struct {
	logger *LoggingStore
	store  ServeResponseLifecycleStore
}

func (s *loggingServeResponseLifecycleStore) AdmitResponseRun(ctx context.Context, value ResponseRunAdmission) (ResponseRunLease, error) {
	result, err := s.store.AdmitResponseRun(ctx, value)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logger.logOnce("AdmitResponseRun", err)
	}
	return result, err
}
func (s *loggingServeResponseLifecycleStore) RenewResponseRunLease(ctx context.Context, responseID, ownerID string, token int64) (ResponseRunLease, error) {
	result, err := s.store.RenewResponseRunLease(ctx, responseID, ownerID, token)
	if err != nil && !errors.Is(err, ErrResponseRunLeaseLost) {
		s.logger.logOnce("RenewResponseRunLease", err)
	}
	return result, err
}
func (s *loggingServeResponseLifecycleStore) ValidateResponseRunLease(ctx context.Context, responseID, ownerID string, token int64) error {
	err := s.store.ValidateResponseRunLease(ctx, responseID, ownerID, token)
	if err != nil && !errors.Is(err, ErrResponseRunLeaseLost) {
		s.logger.logOnce("ValidateResponseRunLease", err)
	}
	return err
}
func (s *loggingServeResponseLifecycleStore) CheckpointResponseRun(ctx context.Context, value ResponseRunCheckpoint) error {
	err := s.store.CheckpointResponseRun(ctx, value)
	if err != nil && !errors.Is(err, ErrResponseRunLeaseLost) {
		s.logger.logOnce("CheckpointResponseRun", err)
	}
	return err
}
func (s *loggingServeResponseLifecycleStore) FinalizeResponseRun(ctx context.Context, value ResponseRunTerminal) (AttentionState, error) {
	result, err := s.store.FinalizeResponseRun(ctx, value)
	if err != nil && !errors.Is(err, ErrResponseRunLeaseLost) {
		s.logger.logOnce("FinalizeResponseRun", err)
	}
	return result, err
}
func (s *loggingServeResponseLifecycleStore) RecoverExpiredResponseRuns(ctx context.Context, limit int) ([]AttentionState, error) {
	result, err := s.store.RecoverExpiredResponseRuns(ctx, limit)
	if err != nil {
		s.logger.logOnce("RecoverExpiredResponseRuns", err)
	}
	return result, err
}

type loggingAttentionStore struct {
	logger *LoggingStore
	store  AttentionStore
}

func (s *loggingAttentionStore) MarkAttentionSeen(ctx context.Context, sessionID, storeID string, seq int64) (AttentionState, error) {
	result, err := s.store.MarkAttentionSeen(ctx, sessionID, storeID, seq)
	if err != nil && !errors.Is(err, ErrAttentionConflict) && !errors.Is(err, ErrNotFound) {
		s.logger.logOnce("MarkAttentionSeen", err)
	}
	return result, err
}
func (s *loggingAttentionStore) GetAttention(ctx context.Context, sessionID string) (AttentionState, error) {
	result, err := s.store.GetAttention(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logger.logOnce("GetAttention", err)
	}
	return result, err
}
func (s *loggingAttentionStore) ListAttention(ctx context.Context, value AttentionListOptions) (AttentionPage, error) {
	result, err := s.store.ListAttention(ctx, value)
	if err != nil && !errors.Is(err, ErrAttentionConflict) {
		s.logger.logOnce("ListAttention", err)
	}
	return result, err
}
func (s *loggingAttentionStore) StoreInstanceID(ctx context.Context) (string, error) {
	result, err := s.store.StoreInstanceID(ctx)
	if err != nil {
		s.logger.logOnce("StoreInstanceID", err)
	}
	return result, err
}

// WorkspaceAccess is the level of local filesystem authority granted to a
// session workspace. Write access always implies read access.
type WorkspaceAccess string

const (
	WorkspaceAccessRead  WorkspaceAccess = "read"
	WorkspaceAccessWrite WorkspaceAccess = "write"
)

// WorkspaceGrant is a durable, session-scoped filesystem capability record.
// Migration 46 stores additional grants plus the reserved primary decision row;
// Session.CWD/WorktreeDir remains the separate primary proposal binding.
type WorkspaceGrant struct {
	ID         string
	Path       string
	Access     WorkspaceAccess
	Provenance string
	Rationale  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// WorkspaceGrantStore is an optional Store capability. Custom and mock stores
// that do not implement it remain usable; primary confirmations and dynamic
// grants then live only for the current runtime.
type WorkspaceGrantStore interface {
	ListWorkspaceGrants(ctx context.Context, sessionID string) ([]WorkspaceGrant, error)
	SaveWorkspaceGrant(ctx context.Context, sessionID string, grant WorkspaceGrant) error
	DeleteWorkspaceGrant(ctx context.Context, sessionID, grantID string) error
}

// AsWorkspaceGrantStore resolves the optional capability through term-llm's
// logging decorator without making an unsupported wrapped store appear durable.
func AsWorkspaceGrantStore(store Store) (WorkspaceGrantStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		underlying, supported := AsWorkspaceGrantStore(logging.Store)
		if !supported {
			return nil, false
		}
		return &loggingWorkspaceGrantStore{logger: logging, store: underlying}, true
	}
	workspaceStore, ok := store.(WorkspaceGrantStore)
	return workspaceStore, ok
}

type loggingWorkspaceGrantStore struct {
	logger *LoggingStore
	store  WorkspaceGrantStore
}

func (s *loggingWorkspaceGrantStore) ListWorkspaceGrants(ctx context.Context, sessionID string) ([]WorkspaceGrant, error) {
	grants, err := s.store.ListWorkspaceGrants(ctx, sessionID)
	if err != nil {
		s.logger.logOnce("ListWorkspaceGrants", err)
	}
	return grants, err
}

func (s *loggingWorkspaceGrantStore) SaveWorkspaceGrant(ctx context.Context, sessionID string, grant WorkspaceGrant) error {
	err := s.store.SaveWorkspaceGrant(ctx, sessionID, grant)
	if err != nil {
		s.logger.logOnce("SaveWorkspaceGrant", err)
	}
	return err
}

func (s *loggingWorkspaceGrantStore) DeleteWorkspaceGrant(ctx context.Context, sessionID, grantID string) error {
	err := s.store.DeleteWorkspaceGrant(ctx, sessionID, grantID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.logger.logOnce("DeleteWorkspaceGrant", err)
	}
	return err
}

// PendingInterjection is a durable, not-yet-committed steering intent. It lives
// outside the transcript until the engine consumes it, so restoring a session
// cannot accidentally send the same user message to a provider early.
type PendingInterjection struct {
	SessionID         string
	ID                string
	Message           llm.Message
	DisplayText       string
	AttachmentSummary string
	CreatedAt         time.Time
}

// PendingInterjectionStore persists queued steering intents across tabs and
// runtime loss. Committed interjections are still written to messages through
// the normal turn-completion path.
type PendingInterjectionStore interface {
	SavePendingInterjection(ctx context.Context, entry PendingInterjection) error
	DeletePendingInterjection(ctx context.Context, sessionID, id string) error
	ListPendingInterjections(ctx context.Context, sessionID string) ([]PendingInterjection, error)
}

// AsPendingInterjectionStore resolves the optional capability through the
// logging decorator without making unsupported custom stores appear durable.
func AsPendingInterjectionStore(store Store) (PendingInterjectionStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		if _, supported := AsPendingInterjectionStore(logging.Store); !supported {
			return nil, false
		}
	}
	pending, ok := store.(PendingInterjectionStore)
	return pending, ok
}

// TranscriptRevisionWriter reports the exact revision committed by a message
// mutation. Serve response handoff uses this optional capability instead of a
// session-wide post-write revision sample.
type TranscriptRevisionWriter interface {
	AddMessageWithTranscriptRev(ctx context.Context, sessionID string, msg *Message) (int64, error)
	UpdateStreamingMessageWithTranscriptRev(ctx context.Context, sessionID string, msg *Message, finalizeText bool) (int64, error)
	ReplaceMessagesWithTranscriptRev(ctx context.Context, sessionID string, messages []Message) (int64, error)
}

// ClientMessageBatchLookup retrieves durable first-party intents by identity.
type ClientMessageBatchLookup interface {
	GetMessagesByClientMessageIDs(ctx context.Context, sessionID string, clientMessageIDs []string) (map[string]*Message, error)
}

// FindMessagesByClientMessageIDs uses a batch capability when available and
// otherwise performs at most one full transcript scan.
func FindMessagesByClientMessageIDs(ctx context.Context, store Store, sessionID string, clientMessageIDs []string) (map[string]*Message, error) {
	found := make(map[string]*Message)
	wanted := make(map[string]struct{}, len(clientMessageIDs))
	for _, rawID := range clientMessageIDs {
		if id := strings.TrimSpace(rawID); id != "" {
			wanted[id] = struct{}{}
		}
	}
	if store == nil || strings.TrimSpace(sessionID) == "" || len(wanted) == 0 {
		return found, nil
	}
	if lookup, ok := store.(ClientMessageBatchLookup); ok {
		return lookup.GetMessagesByClientMessageIDs(ctx, sessionID, clientMessageIDs)
	}
	if lookup, ok := store.(ClientMessageLookup); ok {
		for id := range wanted {
			message, err := lookup.GetMessageByClientMessageID(ctx, sessionID, id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			found[id] = message
		}
		return found, nil
	}
	messages, err := store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	for i := range messages {
		id := strings.TrimSpace(messages[i].ClientMessageID)
		if _, ok := wanted[id]; !ok {
			continue
		}
		message := messages[i]
		found[id] = &message
	}
	return found, nil
}

// ClientMessageLookup retrieves the durable owner of a first-party user intent.
// Implementations should return ErrNotFound when the identity is absent.
type ClientMessageLookup interface {
	GetMessageByClientMessageID(ctx context.Context, sessionID, clientMessageID string) (*Message, error)
}

// FindMessageByClientMessageID uses an indexed lookup when available and falls
// back to scanning stores that do not implement the optional capability.
func FindMessageByClientMessageID(ctx context.Context, store Store, sessionID, clientMessageID string) (*Message, error) {
	clientMessageID = strings.TrimSpace(clientMessageID)
	if store == nil || strings.TrimSpace(sessionID) == "" || clientMessageID == "" {
		return nil, ErrNotFound
	}
	if lookup, ok := store.(ClientMessageLookup); ok {
		return lookup.GetMessageByClientMessageID(ctx, sessionID, clientMessageID)
	}
	messages, err := store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	for i := range messages {
		if strings.TrimSpace(messages[i].ClientMessageID) == clientMessageID {
			message := messages[i]
			return &message, nil
		}
	}
	return nil, ErrNotFound
}

// CompactedTranscriptRevisionWriter reports the exact revision committed by an
// atomic compaction replacement.
type CompactedTranscriptRevisionWriter interface {
	ReplaceCompactedMessagesWithTranscriptRev(ctx context.Context, sessionID string, messages []Message) (int64, error)
}

// GeneratedTitleUpdater is an optional Store capability for updating only the
// generated title fields. It avoids full-session Update writes from async title
// generation paths, where a stale in-memory Session snapshot could clobber
// concurrently updated metadata such as status or pinned state.
type GeneratedTitleUpdater interface {
	UpdateGeneratedTitle(ctx context.Context, id, shortTitle, longTitle string, generatedAt time.Time, basisMsgSeq int) error
}

// UpdateGeneratedTitle persists generated title fields using a title-only fast
// path when available, and falls back to Store.Update for test/custom stores.
func UpdateGeneratedTitle(ctx context.Context, store Store, sess *Session, shortTitle, longTitle string, generatedAt time.Time, basisMsgSeq int) error {
	if store == nil || sess == nil {
		return nil
	}
	if updater, ok := store.(GeneratedTitleUpdater); ok {
		return updater.UpdateGeneratedTitle(ctx, sess.ID, shortTitle, longTitle, generatedAt, basisMsgSeq)
	}
	updated := *sess
	updated.GeneratedShortTitle = shortTitle
	updated.GeneratedLongTitle = longTitle
	if updated.TitleSource != TitleSourceUser && strings.TrimSpace(updated.Name) == "" {
		updated.TitleSource = TitleSourceGenerated
	}
	updated.TitleGeneratedAt = generatedAt
	updated.TitleBasisMsgSeq = basisMsgSeq
	return store.Update(ctx, &updated)
}

// GoalUpdater is an optional Store capability for updating only the persisted
// session goal. It avoids full-session Update writes from runner callbacks where
// a stale Session snapshot could clobber concurrently updated metadata.
type GoalUpdater interface {
	UpdateGoal(ctx context.Context, id string, goal *Goal) error
}

// UpdateGoal persists a session goal using a goal-only fast path when available,
// and falls back to Store.Get + Store.Update for custom stores.
func UpdateGoal(ctx context.Context, store Store, sessionID string, goal *Goal) error {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	goal = goal.Clone()
	if goal != nil {
		goal.Normalize(time.Now())
	}
	if updater, ok := store.(GoalUpdater); ok {
		return updater.UpdateGoal(ctx, sessionID, goal)
	}
	sess, err := store.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return ErrNotFound
	}
	sess.Goal = goal
	return store.Update(ctx, sess)
}

// ShareUpdater is an optional Store capability for updating only share metadata.
type ShareUpdater interface {
	UpdateShare(ctx context.Context, id string, share *ShareState) error
}

// UpdateShare persists share metadata using a narrow update when available.
func UpdateShare(ctx context.Context, store Store, sessionID string, share *ShareState) error {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	share = share.Clone()
	if updater, ok := store.(ShareUpdater); ok {
		return updater.UpdateShare(ctx, sessionID, share)
	}
	sess, err := store.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return ErrNotFound
	}
	sess.Share = share
	return store.Update(ctx, sess)
}

// StreamingMessageUpdater is an optional Store capability for the hot streaming
// assistant upsert path. Implementations may update role/parts/duration without
// rewriting the FTS-backed text_content column until finalizeText is true.
type StreamingMessageUpdater interface {
	UpdateStreamingMessage(ctx context.Context, sessionID string, msg *Message, finalizeText bool) error
}

// UpdateStreamingMessage updates an in-progress assistant message using the
// store's streaming-aware fast path when available, otherwise it falls back to
// Store.UpdateMessage.
func UpdateStreamingMessage(ctx context.Context, store Store, sessionID string, msg *Message, finalizeText bool) error {
	if updater, ok := store.(StreamingMessageUpdater); ok {
		return updater.UpdateStreamingMessage(ctx, sessionID, msg, finalizeText)
	}
	return store.UpdateMessage(ctx, sessionID, msg)
}

// PlanSnapshotStore is an optional Store capability for the authoritative latest
// update_plan snapshot. Transcript tool-call/result parts remain the durable
// replay record; this narrow store supports efficient resume restoration.
type PlanSnapshotStore interface {
	LoadPlanSnapshot(ctx context.Context, sessionID string) (planpkg.Snapshot, int64, error)
	SavePlanSnapshot(ctx context.Context, sessionID string, snapshot planpkg.Snapshot) (int64, error)
	DeletePlanSnapshot(ctx context.Context, sessionID string) error
}

// ProviderStateStore is an optional Store capability for provider-specific
// resume state. It stores opaque JSON/blob payloads keyed by term-llm session
// and provider key, allowing stateful CLI providers to survive runtime
// eviction without leaking that state into the user-visible transcript.
type ProviderStateStore interface {
	SaveProviderState(ctx context.Context, sessionID, providerKey string, state []byte) error
	LoadProviderState(ctx context.Context, sessionID, providerKey string) ([]byte, error)
	DeleteProviderState(ctx context.Context, sessionID, providerKey string) error
}

// MessageSequenceStore is an optional Store capability for fetching the latest
// message sequence for many sessions without issuing one query per session.
// Callers must preserve behavior when a store does not implement this fast path.
type MessageSequenceStore interface {
	MaxMessageSequences(ctx context.Context, sessionIDs []string) (map[string]int, error)
}

// Transcript index flags describe durable rows without materializing bodies.
const (
	TranscriptFlagCompactionTail uint8 = 1 << iota
	TranscriptFlagEmptyBody
)

// TranscriptIndexItem is the compact durable identity and ordering metadata for
// one UI-visible transcript row. IDs are stable identities; Seq is only the
// current ordering key.
type TranscriptIndexItem struct {
	Seq                     int
	ID                      int64
	Role                    string
	Flags                   uint8
	ClientMessageID         string
	ResponseID              string
	AssistantSegmentOrdinal int
}

// TranscriptSnapshot is the complete compact identity stream and its session
// envelope read from one database snapshot.
type TranscriptSnapshot struct {
	Rev             int64
	CompactionSeq   int
	CompactionCount int
	Items           []TranscriptIndexItem
}

// ResponseRunStartState is the compact transcript envelope needed when a
// stateful response run starts. DurableBoundaryID is the latest non-compaction
// user, assistant, or tool row, or zero when no such row exists.
type ResponseRunStartState struct {
	Rev               int64
	CompactionSeq     int
	CompactionCount   int
	DurableBoundaryID int64
}

// ResponseRunStartStateReader is an optional Store capability for reading the
// response-run transcript envelope without materializing historical bodies.
type ResponseRunStartStateReader interface {
	GetResponseRunStartState(ctx context.Context, sessionID string) (ResponseRunStartState, error)
}

// TranscriptRange identifies one complete, contiguous UI transcript segment by
// its inclusive durable ordering bounds. Sequence alone is not assumed unique,
// so IDs disambiguate both endpoints.
type TranscriptRange struct {
	StartSeq int
	StartID  int64
	EndSeq   int
	EndID    int64
}

const TranscriptMaterializationMaxRanges = 32

// TranscriptIndexer is an optional Store capability for coherent revisioned
// transcript reads. Implementations return each revision and its rows from one
// read transaction.
type TranscriptIndexer interface {
	GetTranscriptIndex(ctx context.Context, sessionID string) (rev int64, items []TranscriptIndexItem, err error)
	GetTranscriptSnapshot(ctx context.Context, sessionID string) (TranscriptSnapshot, error)
	GetMessagesByTranscriptRanges(ctx context.Context, sessionID string, ranges []TranscriptRange) (rev int64, messages []Message, err error)
	TranscriptRev(ctx context.Context, sessionID string) (int64, error)
}

// TranscriptVersionReporter distinguishes a current revisioned schema from an
// older read-only database where TranscriptIndexer exposes revision zero only.
type TranscriptVersionReporter interface {
	TranscriptVersioned() bool
}

// TranscriptMutationState is the optimistic concurrency token for undo/redo.
// HeadID is the final non-internal transcript row (the same row stream exposed
// by TranscriptIndexer); zero represents an empty transcript.
type TranscriptMutationState struct {
	Rev    int64 `json:"rev"`
	HeadID int64 `json:"head_id"`
}

// TranscriptMutationResult describes the transcript after undo or redo.
// UserText is populated for undo so clients can restore the removed prompt to
// their composer.
type TranscriptMutationResult struct {
	TranscriptMutationState
	UserText           string `json:"user_text,omitempty"`
	AttachmentsOmitted bool   `json:"attachments_omitted,omitempty"`
}

// TranscriptUndoRedoStore owns a durable per-session redo stack. Ordinary
// transcript writes invalidate the entire stack in the same storage transaction.
type TranscriptUndoRedoStore interface {
	TranscriptMutationState(ctx context.Context, sessionID string) (TranscriptMutationState, error)
	UndoLastUserTurn(ctx context.Context, sessionID string, expected TranscriptMutationState) (TranscriptMutationResult, error)
	RedoLastUserTurn(ctx context.Context, sessionID string, expected TranscriptMutationState) (TranscriptMutationResult, error)
}

// CreateBranchOptions identifies a durable source prefix and optional optimistic
// concurrency/idempotency guards. AnchorMessageID zero means an empty prefix.
type CreateBranchOptions struct {
	AnchorMessageID int64
	ExpectedState   *TranscriptMutationState
	ExpectedRev     *int64
	IdempotencyKey  string
	PathNote        *BranchPathNote
}

// BranchPathNote is optional model-readable context inserted after the copied
// prefix in the same transaction that materializes a conversation branch.
type BranchPathNote struct {
	Text       string
	Provenance llm.PathNoteProvenance
}

// BranchResult is the newly materialized (or idempotently reused) linear child.
type BranchResult struct {
	Session            *Session `json:"session"`
	ForkAfterMessageID int64    `json:"fork_after_message_id,omitempty"`
	AnchorMessageID    int64    `json:"anchor_message_id,omitempty"`
	Reused             bool     `json:"reused,omitempty"`
}

// BranchTreeNode is one normal session in a connected conversation tree.
type BranchTreeNode struct {
	SessionID             string    `json:"session_id"`
	SessionNumber         int64     `json:"session_number,omitempty"`
	ParentSessionID       string    `json:"parent_session_id,omitempty"`
	ForkAfterMessageID    int64     `json:"fork_after_message_id,omitempty"`
	ForkAfterSequence     int       `json:"fork_after_sequence"`
	CopiedAnchorMessageID int64     `json:"copied_anchor_message_id,omitempty"`
	Title                 string    `json:"title,omitempty"`
	AnchorRole            string    `json:"anchor_role,omitempty"`
	AnchorPreview         string    `json:"anchor_preview,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

// BranchTree is a connected component rooted at its oldest surviving ancestor.
// PathCount counts independently resumable linear sessions in the component;
// it intentionally equals len(Nodes), including non-leaf ancestor sessions.
type BranchTree struct {
	RootSessionID   string           `json:"root_session_id"`
	ActiveSessionID string           `json:"active_session_id"`
	PathCount       int              `json:"path_count"`
	Nodes           []BranchTreeNode `json:"nodes"`
}

// ConversationBranchStore is an optional capability so old/read-only schemas
// and custom stores can fail explicitly without expanding the core Store API.
type ConversationBranchStore interface {
	CreateBranch(ctx context.Context, sourceSessionID string, opts CreateBranchOptions) (BranchResult, error)
	GetBranchTree(ctx context.Context, sessionID string) (BranchTree, error)
}

// ConversationBranchReplayStore resolves a prior idempotent branch request
// without regenerating optional helper context.
type ConversationBranchReplayStore interface {
	GetBranchByIdempotencyKey(ctx context.Context, sourceSessionID, idempotencyKey string) (BranchResult, bool, error)
}

// SessionSummaryTranscriptRevisionReporter reports whether Store.List populates
// SessionSummary.TranscriptRev directly, allowing callers to avoid one revision
// query per listed session.
type SessionSummaryTranscriptRevisionReporter interface {
	SessionSummariesIncludeTranscriptRev() bool
}

// DiffCommentMessageLister is an optional targeted lookup capability for stores
// that can select only messages carrying typed inline-diff comment metadata.
// Web comment hydration uses it to avoid loading and decoding an entire session.
type DiffCommentMessageLister interface {
	GetDiffCommentMessages(ctx context.Context, sessionID string) ([]Message, error)
}

// MessagesDescendingPager is an optional Store capability for efficient reverse
// pagination over session messages. Implementations return messages ordered by
// descending sequence and, when beforeSeq > 0, only rows with sequence < beforeSeq.
type MessagesDescendingPager interface {
	GetMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]Message, error)
}

// PromptHistoryEntry is a user prompt recalled from composer history.
type PromptHistoryEntry struct {
	ID        int64
	CreatedAt time.Time
	Text      string
}

// PromptHistoryStore is an optional Store capability for shell-style composer
// history recall. Implementations traverse persisted user prompts globally so
// multiple TUI processes share the same prompt history.
type PromptHistoryStore interface {
	PreviousUserPrompt(ctx context.Context, agent string, beforeID int64) (*PromptHistoryEntry, error)
	NextUserPrompt(ctx context.Context, agent string, afterID int64) (*PromptHistoryEntry, error)
}

// PromptHistoryOutsideSessionStore is an optional Store capability for the TUI
// composer history sequence after the current session's in-memory prompts have
// been exhausted. It traverses persisted user prompts from all agents while
// excluding the current session to avoid duplicate recalls.
type PromptHistoryOutsideSessionStore interface {
	PreviousUserPromptOutsideSession(ctx context.Context, excludeSessionID string, beforeID int64, beforeCreatedAt time.Time) (*PromptHistoryEntry, error)
	NextUserPromptOutsideSession(ctx context.Context, excludeSessionID string, afterID int64, afterCreatedAt time.Time) (*PromptHistoryEntry, error)
}

// PushSubscription represents a Web Push subscription stored in the database.
type PushSubscription struct {
	ID              string
	Endpoint        string
	KeyP256DH       string
	KeyAuth         string
	Status          string
	VAPIDKeyID      string
	UpdatedAt       time.Time
	LastUsedAt      time.Time
	LastFailureCode string
	LastFailure     string
	LastFailureAt   time.Time
}

type PushSubscriptionLifecycleStore interface {
	UpsertPushSubscription(ctx context.Context, sub *PushSubscription) (*PushSubscription, error)
	GetPushSubscription(ctx context.Context, id string) (*PushSubscription, error)
	DeletePushSubscriptionByID(ctx context.Context, id string) error
	MarkPushSubscriptionStale(ctx context.Context, id, code, detail string) error
	MarkPushSubscriptionUsed(ctx context.Context, id string) error
}

func AsPushSubscriptionLifecycleStore(store Store) (PushSubscriptionLifecycleStore, bool) {
	if store == nil {
		return nil, false
	}
	if lifecycle, ok := store.(PushSubscriptionLifecycleStore); ok {
		return lifecycle, true
	}
	if logging, ok := store.(*LoggingStore); ok {
		lifecycle, ok := logging.Store.(PushSubscriptionLifecycleStore)
		return lifecycle, ok
	}
	return nil, false
}

type CompletionPushOutboxItem struct {
	ID             int64
	EventID        string
	ResponseID     string
	SubscriptionID string
	Payload        []byte
	AttemptCount   int
}

type CompletionPushOutboxStore interface {
	EnqueueCompletionPush(ctx context.Context, item CompletionPushOutboxItem) (bool, error)
	ListDueCompletionPushes(ctx context.Context, now time.Time, limit int) ([]CompletionPushOutboxItem, error)
	MarkCompletionPushDelivered(ctx context.Context, id int64) error
	RetryCompletionPush(ctx context.Context, id int64, next time.Time, lastError string) error
	MarkCompletionPushDead(ctx context.Context, id int64, lastError string) error
	PruneCompletionPushOutbox(ctx context.Context, before time.Time) error
}

func AsCompletionPushOutboxStore(store Store) (CompletionPushOutboxStore, bool) {
	if store == nil {
		return nil, false
	}
	if outbox, ok := store.(CompletionPushOutboxStore); ok {
		return outbox, true
	}
	if logging, ok := store.(*LoggingStore); ok {
		outbox, ok := logging.Store.(CompletionPushOutboxStore)
		return outbox, ok
	}
	return nil, false
}

// Config holds session storage configuration.
type Config struct {
	Enabled          bool   `mapstructure:"enabled"`            // Master switch
	MaxAgeDays       int    `mapstructure:"max_age_days"`       // Auto-delete after N days (0=never)
	MaxCount         int    `mapstructure:"max_count"`          // Keep at most N sessions (0=unlimited)
	Path             string `mapstructure:"path"`               // Optional DB path override (supports :memory:)
	StripImageBase64 bool   `mapstructure:"strip_image_base64"` // Store path/metadata only for images with ImagePath (smaller DB, less portable)
	ReadOnly         bool   `mapstructure:"-"`                  // Open DB in read-only mode (skip schema init/cleanup)
}

// DefaultConfig returns the default session configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:    true,
		MaxAgeDays: 0, // Never auto-delete
		MaxCount:   0, // Unlimited
		Path:       "",
	}
}

// GetDataDir returns the XDG data directory for term-llm.
// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share
func GetDataDir() (string, error) {
	return appdata.GetDataDir()
}

// GetDBPath returns the path to the sessions database.
func GetDBPath() (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "sessions.db"), nil
}

// GetHandoverDir returns the handover directory for the given working directory.
// The path is XDG_DATA_HOME/term-llm/handover/<basename>-<sha256[:6]>/
// where the hash is computed from the absolute cwd to avoid collisions.
func GetHandoverDir(cwd string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute path: %w", err)
	}
	h := sha256.Sum256([]byte(abs))
	projectID := filepath.Base(abs) + "-" + hex.EncodeToString(h[:3])
	return filepath.Join(dataDir, "handover", projectID), nil
}

// GetHandoverPath returns a full handover file path with a random name like
// "2026-04-03-amber-creek-bloom.md". A fresh slug is generated per call so
// concurrent sessions in the same project get distinct plan files. The
// expanded system prompt is the durable per-session record of the path; use
// ExtractHandoverPath to recover it.
func GetHandoverPath(cwd, date string) (string, error) {
	dir, err := GetHandoverDir(cwd)
	if err != nil {
		return "", err
	}
	slug := RandomHandoverSlug()
	return filepath.Join(dir, date+"-"+slug+".md"), nil
}

// ExtractHandoverPath recovers a handover file path embedded in a system
// prompt via {{handover_path}}. It matches the first path under dir with the
// "<date>-<slug>.md" shape. Returns "" when the prompt names no such file.
func ExtractHandoverPath(prompt, dir string) string {
	if prompt == "" || dir == "" {
		return ""
	}
	re := regexp.MustCompile(regexp.QuoteMeta(dir) + `[\\/]\d{4}-\d{2}-\d{2}-[a-zA-Z0-9-]+\.md`)
	return re.FindString(prompt)
}

// ResolvePinnedHandoverPath recovers the handover path assigned by the system
// prompt. Candidate directories support sessions whose effective directory and
// process working directory differ. The planner's assignment is also recovered
// across directory changes, but only when exactly one assignment points under
// term-llm's global handover root.
//
// pinned is true when an assignment was found even if it was ambiguous. Callers
// must not fall back to scanning another file when pinned is true.
func ResolvePinnedHandoverPath(prompt string, candidateDirs ...string) (path string, pinned bool) {
	if prompt == "" {
		return "", false
	}

	assigned := assignedHandoverPaths(prompt)
	for _, dir := range candidateDirs {
		for _, path := range assigned {
			if handoverPathInDir(path, dir) {
				return path, true
			}
		}
	}
	if len(assigned) == 1 {
		return assigned[0], true
	}
	if len(assigned) > 1 {
		return "", true
	}

	seen := make(map[string]struct{}, len(candidateDirs))
	for _, dir := range candidateDirs {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		if path := ExtractHandoverPath(prompt, clean); path != "" {
			return path, true
		}
	}
	return "", false
}

func assignedHandoverPaths(prompt string) []string {
	// This wording is part of the built-in planner prompt and anchors the path
	// to the actual assignment rather than unrelated handover references that
	// may also appear in resumed or injected context.
	re := regexp.MustCompile(`(?is)your plan lives at exactly this path[^\r\n:]*:\s*` + "`?" + `([^` + "`" + `\r\n]+\.md)`)
	matches := re.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return nil
	}
	dataDir, err := GetDataDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(dataDir, "handover")
	seen := make(map[string]struct{}, len(matches))
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		path := strings.TrimSpace(match[1])
		if !validHandoverPathUnderRoot(path, root) {
			continue
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

func validHandoverPathUnderRoot(path, root string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if filepath.Dir(rel) == "." || strings.Contains(filepath.Dir(rel), string(filepath.Separator)) {
		return false
	}
	matched, _ := regexp.MatchString(`^\d{4}-\d{2}-\d{2}-[a-zA-Z0-9-]+\.md$`, filepath.Base(path))
	return matched
}

func handoverPathInDir(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	return filepath.Clean(filepath.Dir(path)) == filepath.Clean(dir)
}

// ResolveDBPath resolves an optional DB path override.
// Empty path uses the default XDG location.
// Supports :memory: for ephemeral in-memory storage.
func ResolveDBPath(pathOverride string) (string, error) {
	return sqliteutil.ResolveDBPathOverride(pathOverride, GetDBPath)
}

// NewStore creates a new Store based on the configuration.
// If sessions are disabled, returns a no-op store.
func NewStore(cfg Config) (Store, error) {
	if !cfg.Enabled {
		return &NoopStore{}, nil
	}
	return NewSQLiteStore(cfg)
}
