package version

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// Capabilities reports additive native interfaces that external integrations
// may require before enabling dependent behaviour. Keys are stable; a value is
// the interface version (incremented only for incompatible changes). Absent
// keys, or a binary without this command, mean the interface is unsupported.
func Capabilities() map[string]int {
	return map[string]int{
		// agent.yaml lock_session: durable policy and fixed agent/workspace.
		"session_scope_lock": 1,
		// Host-reserved TERM_LLM_SESSION_ID/TERM_LLM_PRIVATE_SESSION for
		// admitted locked sessions; reserved names cannot be inherited/forged.
		"custom_tool_session_context": 1,
	}
}

// NewCapabilitiesCommand builds `term-llm capabilities [--json]`.
func NewCapabilitiesCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "capabilities",
		Short: "Print native interface capability versions as JSON",
		Args:  cobra.NoArgs,
		RunE:  RunCapabilities,
	}
	command.Flags().Bool("json", true, "Print machine-readable JSON (the only format)")
	return command
}

// RunCapabilities prints Capabilities as one JSON object.
func RunCapabilities(cmd *cobra.Command, _ []string) error {
	encoded, err := json.Marshal(Capabilities())
	if err != nil {
		return fmt.Errorf("encode capabilities: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
	return err
}
