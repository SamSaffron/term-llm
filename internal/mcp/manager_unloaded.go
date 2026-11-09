package mcp

// NewUnloadedManager binds identity without filesystem I/O. Call LoadConfig
// before inspecting connections or enabling servers; access remains fail-closed
// until policy has loaded successfully.
func NewUnloadedManager(options ...ManagerOption) *Manager {
	manager := &Manager{
		clients:    make(map[string]*Client),
		statuses:   make(map[string]*ServerState),
		startups:   make(map[string]*serverStartup),
		catalogues: make(map[string]*ToolSnapshot),
		stopping:   make(map[*Client]chan struct{}),
	}
	for _, option := range options {
		manager.policy.identity = option.policyIdentity
	}
	manager.aggregate.Store(&CatalogueSnapshot{})
	return manager
}
