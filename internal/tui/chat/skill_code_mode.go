package chat

// skillActivationScope keeps turn-scoped executable admission next to the
// corresponding temporary allowed-tools restriction.
type skillActivationScope struct {
	skillFilterRestoreTools   []string
	skillFilterRestorePresent bool
	skillFilterPending        bool
	skillCodeModuleRestore    func()
}
