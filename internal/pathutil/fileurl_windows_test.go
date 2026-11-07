//go:build windows

package pathutil

import "testing"

func TestParseLocalFileURLWindowsForms(t *testing.T) {
	tests := []struct {
		in   string
		path string
		frac string
	}{
		{`file:///C:/Users/me/config.yaml#model`, `C:/Users/me/config.yaml`, "model"},
		{`file://C:\Users\me\config.yaml`, `C:\Users\me\config.yaml`, ""},
		{`file://\\server\share\config.yaml`, `\\server\share\config.yaml`, ""},
		{`file:////server/share/config.yaml`, `\\server\share\config.yaml`, ""},
		{`file://server/share/config.yaml#model`, `\\server\share\config.yaml`, "model"},
	}
	for _, tt := range tests {
		path, fragment, err := ParseLocalFileURL(tt.in)
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if path != tt.path || fragment != tt.frac {
			t.Fatalf("%s: path=%q fragment=%q, want %q/%q", tt.in, path, fragment, tt.path, tt.frac)
		}
	}
}
