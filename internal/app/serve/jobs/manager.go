package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/session"
	_ "github.com/samsaffron/term-llm/internal/sqlitedriver"
)

// NewManager opens the jobs database and starts its scheduler and workers.
func NewManager(dbPath string, workers int, runners map[RunnerType]Runner, notifyDone RunDoneNotifier) (*Manager, error) {
	if workers < 0 {
		workers = 1
	}
	if strings.TrimSpace(dbPath) == "" {
		dataDir, err := session.GetDataDir()
		if err != nil {
			return nil, fmt.Errorf("resolve jobs data dir: %w", err)
		}
		dbPath = filepath.Join(dataDir, "jobs_v2.db")
	}
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			return nil, fmt.Errorf("create jobs db dir: %w", err)
		}
	}

	// One live owner per jobs database. Recovery must not mark another process's
	// running jobs worker-lost. The close-on-exec lock survives until replacement.
	unlock := func() error { return nil }
	if dbPath != ":memory:" {
		var lockErr error
		unlock, lockErr = filelock.TryLock(dbPath + ".owner.lock")
		if lockErr != nil {
			return nil, fmt.Errorf("jobs database already owned: %w", lockErr)
		}
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = unlock()
		}
	}()

	dsn := dbPath
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open jobs db: %w", err)
	}
	// The jobs scheduler, workers, and admin paths all share one SQLite file.
	// Keep the database/sql pool to one connection so concurrent writes are
	// serialized before they reach SQLite's single-writer lock.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := InitSchema(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init jobs schema: %w", err)
	}

	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	mgr := &Manager{
		releaseOwner: unlock, db: db,
		workers:            workers,
		workerID:           "worker_" + wire.RandomSuffix(),
		notifyDone:         notifyDone,
		schedulerIdleDelay: jobsV2SchedulerIdleDelay,
		workerIdleDelay:    jobsV2WorkerIdleDelay,
		// Conservative defaults: enough history for debugging without unbounded growth.
		retentionRunDays:    30,
		retentionEventDays:  30,
		retentionMaxRunsJob: 1000,
		cleanupInterval:     time.Hour,
		runners:             runners,
		done:                make(chan struct{}),
		schedulerWake:       make(chan struct{}, 1),
		workerWake:          make(chan struct{}, max(1, workers)),
		cancels:             make(map[string]context.CancelFunc),
		notifyCtx:           notifyCtx,
		notifyCancel:        notifyCancel,
	}

	if err := mgr.recoverRuns(); err != nil {
		notifyCancel()
		_ = db.Close()
		return nil, err
	}
	if err := mgr.reconcileMisfiredSchedules(time.Now().UTC()); err != nil {
		notifyCancel()
		_ = db.Close()
		return nil, err
	}

	mgr.wg.Add(1)
	go mgr.schedulerLoop()
	for i := 0; i < mgr.workers; i++ {
		mgr.wg.Add(1)
		go mgr.workerLoop()
	}

	adopted = true
	return mgr, nil
}

// Manager persists jobs and coordinates scheduling, execution, and completion notifications.
type Manager struct {
	releaseOwner func() error
	db           *sql.DB
	workers      int
	workerID     string
	runners      map[RunnerType]Runner
	notifyDone   RunDoneNotifier
	// Idle timers are only fallbacks; job/run mutations wake the loops immediately.
	schedulerIdleDelay time.Duration
	workerIdleDelay    time.Duration
	// Retention settings for protecting disk usage.
	retentionRunDays     int
	retentionEventDays   int
	retentionMaxRunsJob  int
	cleanupInterval      time.Duration
	lastCleanupCompleted time.Time
	cleanupRunning       bool

	enqueueMu     sync.Mutex
	mu            sync.Mutex
	closed        bool
	done          chan struct{}
	schedulerWake chan struct{}
	workerWake    chan struct{}
	wg            sync.WaitGroup
	cancels       map[string]context.CancelFunc
	notifyCtx     context.Context
	notifyCancel  context.CancelFunc
}

func (m *Manager) recoverRuns() error {
	rows, err := m.db.Query(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at FROM job_runs_v2 WHERE status IN (?, ?) ORDER BY created_at ASC`, RunClaimed, RunRunning)
	if err != nil {
		return fmt.Errorf("load interrupted runs: %w", err)
	}

	var interrupted []Run
	for rows.Next() {
		run, err := scanRunV2(rows)
		if err != nil {
			return fmt.Errorf("scan interrupted run: %w", err)
		}
		interrupted = append(interrupted, run)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("scan interrupted runs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close interrupted runs query: %w", err)
	}

	if _, err := m.db.Exec(`
		UPDATE job_runs_v2
		SET status = ?,
			finished_at = CURRENT_TIMESTAMP,
			exit_reason = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE status = ?`,
		RunCancelled,
		exitReasonCancelled,
		RunCancelRequested); err != nil {
		return fmt.Errorf("recover cancel requested runs: %w", err)
	}

	for _, run := range interrupted {
		exitCode := 0
		if run.ExitCode != nil {
			exitCode = *run.ExitCode
		}
		result := RunResult{
			ExitCode:     exitCode,
			Stdout:       run.Stdout,
			Stderr:       run.Stderr,
			Thinking:     run.Thinking,
			Response:     run.Response,
			SessionID:    run.SessionID,
			ExitReason:   exitReasonWorkerLost,
			Truncated:    run.Truncated,
			TurnCount:    run.TurnCount,
			InputTokens:  run.InputTokens,
			OutputTokens: run.OutputTokens,
		}
		if err := m.finishRun(run.ID, RunFailed, result, errors.New("worker lost before run could finish"), run.Attempt); err != nil {
			return fmt.Errorf("recover interrupted run %s: %w", run.ID, err)
		}
	}

	return nil
}

func (m *Manager) Close() error {
	return m.CloseContext(context.Background())
}

func (m *Manager) CloseContext(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	if m.done != nil {
		close(m.done)
	}
	if m.notifyCancel != nil {
		m.notifyCancel()
	}
	cancels := make([]context.CancelFunc, 0, len(m.cancels))
	for _, cancel := range m.cancels {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	waitDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		err := m.db.Close()
		if m.releaseOwner != nil {
			err = errors.Join(err, m.releaseOwner())
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) doneChan() <-chan struct{} {
	if m.done == nil {
		return nil
	}
	return m.done
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}
