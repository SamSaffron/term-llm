package serve

import servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"

func newJobsV2Manager(dbPath string, workers int, llmExec serveJobsExecutor) (*jobsV2Manager, error) {
	return newJobsV2ManagerWithNotifier(dbPath, workers, llmExec, nil)
}

func newJobsV2ManagerWithNotifier(dbPath string, workers int, llmExec serveJobsExecutor, notifyDone jobsV2RunDoneNotifier) (*jobsV2Manager, error) {
	return servejobs.NewManager(dbPath, workers, map[jobsV2RunnerType]jobsV2Runner{
		jobsV2RunnerProgram: &servejobs.ProgramRunner{},
		jobsV2RunnerLLM:     &jobsV2LLMRunner{exec: llmExec},
	}, notifyDone)
}
