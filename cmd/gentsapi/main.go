// Command gentsapi writes the registered Go serve JSON contracts for the frontend.
package main

import (
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/tsgen"
)

func main() {
	output, err := tsgen.Generate(".")
	if err == nil {
		err = os.WriteFile("frontend/src/api/generated/types.ts", output, 0644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
