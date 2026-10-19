package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/session"
)

func newJobsV2Manager(dbPath string, workers int, llmExec serveJobsExecutor) (*jobsV2Manager, error) {
	return newJobsV2ManagerWithNotifier(dbPath, workers, llmExec, nil)
}

func newJobsV2ManagerWithNotifier(dbPath string, workers int, llmExec serveJobsExecutor, notifyDone jobsV2RunDoneNotifier) (*jobsV2Manager, error) {
	return newJobsV2ManagerWithRunners(dbPath, workers, map[jobsV2RunnerType]jobsV2Runner{
		jobsV2RunnerProgram: &jobsV2ProgramRunner{},
		jobsV2RunnerLLM:     &jobsV2LLMRunner{exec: llmExec},
	}, notifyDone)
}

func newJobsV2ManagerWithRunners(dbPath string, workers int, runners map[jobsV2RunnerType]jobsV2Runner, notifyDone jobsV2RunDoneNotifier) (*jobsV2Manager, error) {
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
	if err := servejobs.InitSchema(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init jobs schema: %w", err)
	}

	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	mgr := &jobsV2Manager{
		releaseOwner: unlock, db: db,
		workers:            workers,
		workerID:           "worker_" + randomSuffix(),
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
