package version

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
)

func TestCapabilitiesJSONContract(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := RunCapabilities(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output %q is not a JSON object: %v", out.String(), err)
	}
	want := map[string]int{"session_scope_lock": 1, "custom_tool_session_context": 1}
	if len(got) != len(want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
	for key, version := range want {
		if got[key] != version {
			t.Fatalf("capabilities = %v, want %v", got, want)
		}
	}
	if _, ok := got["private_upload_context"]; ok {
		t.Fatal("upload context must not be advertised")
	}
}
