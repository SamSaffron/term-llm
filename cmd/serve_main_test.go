package cmd

import (
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
	"github.com/samsaffron/term-llm/internal/testutil/procenv"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	mcptest.RunIfChild()
	cleanup, err := procenv.Setup()
	if err != nil {
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
