package exitcode

// Exit codes for term-llm commands
const (
	Success      = 0
	Error        = 1
	UserDeclined = 2
	NoChanges    = 3
	Cancelled    = 130 // 128 + SIGINT
)

// ExitError is an error that carries a specific exit code
type ExitError struct {
	Code    int
	Message string
}

func (e ExitError) Error() string {
	return e.Message
}

// Convenience constructors
func Declined(msg string) ExitError { return ExitError{Code: UserDeclined, Message: msg} }
func NoEdits(msg string) ExitError  { return ExitError{Code: NoChanges, Message: msg} }
func Cancel() ExitError             { return ExitError{Code: Cancelled, Message: "cancelled"} }
