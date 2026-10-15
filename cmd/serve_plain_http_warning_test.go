package cmd

import "testing"

func TestPlainHTTPConnectionWarning(t *testing.T) {
	tests := []struct {
		name      string
		ui        bool
		host      string
		publicURL string
		want      bool
	}{
		{name: "lan bind over plain http", ui: true, host: "0.0.0.0", want: true},
		{name: "plain http public url", ui: true, host: "0.0.0.0", publicURL: "http://box:8080", want: true},
		{name: "https proxy", ui: true, host: "0.0.0.0", publicURL: "HTTPS://chat.example.com"},
		{name: "loopback", ui: true, host: "127.0.0.1"},
		{name: "api only", ui: false, host: "0.0.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plainHTTPConnectionWarning(tt.ui, tt.host, tt.publicURL)
			if (got != "") != tt.want {
				t.Fatalf("plainHTTPConnectionWarning(%v, %q, %q) = %q, want warning %v", tt.ui, tt.host, tt.publicURL, got, tt.want)
			}
		})
	}
}
