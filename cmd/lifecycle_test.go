package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/termhost"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestLifecycleStatusJSONExplainsDetectionWithoutPublishing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "pane-1")
	t.Setenv("HERDR_BIN_PATH", "/path/that/need/not/run")
	viper.Reset()
	t.Cleanup(viper.Reset)
	previous := lifecycleStatusJSON
	lifecycleStatusJSON = true
	t.Cleanup(func() { lifecycleStatusJSON = previous })
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := runLifecycleStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	var report termhost.StatusReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode status: %v\n%s", err, output.String())
	}
	if !report.Enabled || len(report.Adapters) < 2 {
		t.Fatalf("report = %#v", report)
	}
	found := false
	for _, status := range report.Adapters {
		if status.Name == "herdr" {
			found = status.Detected && status.Enabled
		}
	}
	if !found {
		t.Fatalf("Herdr status not enabled: %#v", report.Adapters)
	}
}

func TestLifecycleStatusTextIncludesReasons(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TERM_LLM_LIFECYCLE", "0")
	viper.Reset()
	t.Cleanup(viper.Reset)
	previous := lifecycleStatusJSON
	lifecycleStatusJSON = false
	t.Cleanup(func() { lifecycleStatusJSON = previous })
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := runLifecycleStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "TERM_LLM_LIFECYCLE=0") || !strings.Contains(got, "osc [terminal]") {
		t.Fatalf("status output = %q", got)
	}
}
