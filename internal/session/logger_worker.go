package session

import "context"

func (s *LoggingStore) workerStore() (WorkerStore, error) {
	store, ok := s.Store.(WorkerStore)
	if !ok {
		return nil, ErrWorkersUnsupported
	}
	return store, nil
}

func (s *LoggingStore) CreateWorker(ctx context.Context, coordinatorSessionID, task string) (WorkerEdge, error) {
	store, err := s.workerStore()
	if err != nil {
		return WorkerEdge{}, err
	}
	edge, err := store.CreateWorker(ctx, coordinatorSessionID, task)
	if err != nil {
		s.logOnce("CreateWorker", err)
	}
	return edge, err
}

func (s *LoggingStore) CreateWorkerOwned(ctx context.Context, coordinatorSessionID, ownerID, task string) (WorkerEdge, error) {
	store, err := s.workerStore()
	if err != nil {
		return WorkerEdge{}, err
	}
	edge, err := store.CreateWorkerOwned(ctx, coordinatorSessionID, ownerID, task)
	if err != nil {
		s.logOnce("CreateWorkerOwned", err)
	}
	return edge, err
}

func (s *LoggingStore) SetWorkerExecution(ctx context.Context, childSessionID, jobID, runID string) error {
	store, err := s.workerStore()
	if err == nil {
		err = store.SetWorkerExecution(ctx, childSessionID, jobID, runID)
	}
	if err != nil {
		s.logOnce("SetWorkerExecution", err)
	}
	return err
}

func (s *LoggingStore) SetWorkerOwner(ctx context.Context, childSessionID, oldOwnerID, newOwnerID string) (bool, error) {
	store, err := s.workerStore()
	if err != nil {
		return false, err
	}
	changed, err := store.SetWorkerOwner(ctx, childSessionID, oldOwnerID, newOwnerID)
	if err != nil {
		s.logOnce("SetWorkerOwner", err)
	}
	return changed, err
}

func (s *LoggingStore) UpdateWorkerStatus(ctx context.Context, childSessionID string, status WorkerStatus) error {
	store, err := s.workerStore()
	if err == nil {
		err = store.UpdateWorkerStatus(ctx, childSessionID, status)
	}
	if err != nil {
		s.logOnce("UpdateWorkerStatus", err)
	}
	return err
}

func (s *LoggingStore) FinishWorker(ctx context.Context, childSessionID string, status WorkerStatus, report WorkerReport) error {
	store, err := s.workerStore()
	if err == nil {
		err = store.FinishWorker(ctx, childSessionID, status, report)
	}
	if err != nil {
		s.logOnce("FinishWorker", err)
	}
	return err
}

func (s *LoggingStore) GetWorker(ctx context.Context, childSessionID string) (WorkerEdge, error) {
	store, err := s.workerStore()
	if err != nil {
		return WorkerEdge{}, err
	}
	return store.GetWorker(ctx, childSessionID)
}

func (s *LoggingStore) ListWorkers(ctx context.Context, coordinatorSessionID string) ([]WorkerEdge, error) {
	store, err := s.workerStore()
	if err != nil {
		return nil, err
	}
	return store.ListWorkers(ctx, coordinatorSessionID)
}

func (s *LoggingStore) AddWorkerReport(ctx context.Context, report WorkerReport) (WorkerReport, error) {
	store, err := s.workerStore()
	if err != nil {
		return WorkerReport{}, err
	}
	created, err := store.AddWorkerReport(ctx, report)
	if err != nil {
		s.logOnce("AddWorkerReport", err)
	}
	return created, err
}

func (s *LoggingStore) ListWorkerReports(ctx context.Context, childSessionID string) ([]WorkerReport, error) {
	store, err := s.workerStore()
	if err != nil {
		return nil, err
	}
	return store.ListWorkerReports(ctx, childSessionID)
}

func (s *LoggingStore) MarkWorkerReportRead(ctx context.Context, reportID int64) error {
	store, err := s.workerStore()
	if err == nil {
		err = store.MarkWorkerReportRead(ctx, reportID)
	}
	if err != nil {
		s.logOnce("MarkWorkerReportRead", err)
	}
	return err
}

func (s *LoggingStore) ImportWorkerReport(ctx context.Context, reportID int64) (*Message, error) {
	store, err := s.workerStore()
	if err != nil {
		return nil, err
	}
	message, err := store.ImportWorkerReport(ctx, reportID)
	if err != nil {
		s.logOnce("ImportWorkerReport", err)
	}
	return message, err
}

func (s *LoggingStore) CountUnreadWorkerReports(ctx context.Context, coordinatorSessionID string) (int, error) {
	store, err := s.workerStore()
	if err != nil {
		return 0, err
	}
	return store.CountUnreadWorkerReports(ctx, coordinatorSessionID)
}
