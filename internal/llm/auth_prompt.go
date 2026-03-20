package llm

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
)

// waitForEnterOrInterrupt waits for the user to press Enter or Ctrl+C.
// Returns nil on Enter, or an error on interrupt.
func waitForEnterOrInterrupt() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	doneCh := make(chan struct{})
	go func() {
		reader := bufio.NewReader(os.Stdin)
		reader.ReadString('\n')
		close(doneCh)
	}()

	select {
	case <-doneCh:
		return nil
	case <-sigCh:
		fmt.Println()
		// Exit the process rather than returning, because the stdin-reading
		// goroutine above has no cancellation path and would leak, potentially
		// consuming later user input unexpectedly.
		os.Exit(1)
		return nil // unreachable, but keeps the compiler happy
	}
}
