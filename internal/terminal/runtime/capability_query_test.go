package tea

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

type capabilityQueryModel struct{}

func (capabilityQueryModel) Init() Cmd {
	return Sequence(RequestBackgroundColor, Raw("explicit raw output"), Quit)
}
func (m capabilityQueryModel) Update(Msg) (Model, Cmd) { return m, nil }
func (capabilityQueryModel) View() View                { return NewView("query test") }

func TestProgramCapabilityQueriesRequireInput(t *testing.T) {
	for _, inputEnabled := range []bool{false, true} {
		name := "disabled"
		input := WithInput(nil)
		if inputEnabled {
			name = "enabled"
			input = WithInput(bytes.NewReader(nil))
		}
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			p := NewProgram(capabilityQueryModel{}, input, WithOutput(&output),
				WithEnvironment([]string{"TERM=xterm-kitty", "TERM_PROGRAM=kitty"}),
				WithoutSignalHandler())
			if _, err := p.Run(); err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, query := range []string{ansi.RequestBackgroundColor, ansi.RequestModeSynchronizedOutput, ansi.RequestModeUnicodeCore} {
				if got := strings.Contains(output.String(), query); got != inputEnabled {
					t.Errorf("query %q present = %v, want %v: %q", query, got, inputEnabled, output.String())
				}
			}
			if !strings.Contains(output.String(), "explicit raw output") {
				t.Fatal("explicit raw output was suppressed")
			}
		})
	}
}
