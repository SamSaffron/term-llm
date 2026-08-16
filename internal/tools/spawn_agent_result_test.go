package tools

import "testing"

func TestParseSpawnAgentResult(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    SpawnAgentResult
		wantErr bool
	}{
		{name: "success", content: `{"agent_name":"reviewer","output":"done","duration_ms":12,"session_id":"child"}`, want: SpawnAgentResult{AgentName: "reviewer", Output: "done", Duration: 12, SessionID: "child"}},
		{name: "partial error", content: `{"output":"partial","error":"timed out","type":"timeout","session_id":"child"}`, want: SpawnAgentResult{Output: "partial", Error: "timed out", Type: "timeout", SessionID: "child"}},
		{name: "unknown fields tolerated", content: `{"output":"done","future":true}`, want: SpawnAgentResult{Output: "done"}},
		{name: "plain text", content: "done", wantErr: true},
		{name: "empty object", content: `{}`, wantErr: true},
		{name: "empty", content: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSpawnAgentResult(tt.content)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %#v want %#v", got, tt.want)
			}
		})
	}
}
