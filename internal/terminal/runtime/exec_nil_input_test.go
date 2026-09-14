package tea

import (
	"bytes"
	"os/exec"
	"runtime"
	"testing"
)

type testExecNoInputModel struct{ testExecModel }

func (m *testExecNoInputModel) Init() Cmd {
	return ExecProcess(successExecCommand(), func(err error) Msg {
		return execFinishedMsg{err}
	})
}

func successExecCommand() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit 0") //nolint:gosec
	}
	return exec.Command("true") //nolint:gosec
}

func TestTeaExecWithNilInput(t *testing.T) {
	var buf bytes.Buffer

	m := &testExecNoInputModel{}
	p := NewProgram(m,
		WithInput(nil),
		WithOutput(&buf),
	)

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if m.err != nil {
		t.Fatalf("expected no error, got %v", m.err)
	}
}
