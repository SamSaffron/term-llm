//go:build windows

package pathutil

import "testing"

func TestParseLocalFileURLWindowsForms(t *testing.T) {
	tests := []struct {
		in      string
		path    string
		frac    string
		wantErr bool
	}{
		{in: `file:///C:/Users/me/config.yaml#model`, path: `C:/Users/me/config.yaml`, frac: "model"},
		{in: `file:///C:/Users/A%20B/token.txt`, path: `C:/Users/A B/token.txt`},
		{in: `file:///C:/Users/%E2%9C%93/token.txt`, path: `C:/Users/✓/token.txt`},
		{in: `file:///C:/A%23B/token.txt`, path: `C:/A#B/token.txt`},
		{in: `file:///C:/A%25B/token.txt`, path: `C:/A%B/token.txt`},
		{in: `file:///C:/config.yaml#frag%20ment`, path: `C:/config.yaml`, frac: "frag ment"},
		{in: `file://server/share/A%20B.yaml#model`, path: `\\server\share\A B.yaml`, frac: "model"},
		{in: `file://C:\Users\me\config.yaml`, path: `C:\Users\me\config.yaml`},
		{in: `file://\\server\share\config.yaml`, path: `\\server\share\config.yaml`},
		{in: `file:////server/share/config.yaml`, path: `\\server\share\config.yaml`},
		{in: `file://server/share/config.yaml#model`, path: `\\server\share\config.yaml`, frac: "model"},
		{in: `file:///C:/bad%2`, wantErr: true},
		{in: `file:///C:/bad%zz`, wantErr: true},
		{in: `file:///C:/ok.yaml#bad%zz`, wantErr: true},
	}
	for _, tt := range tests {
		path, fragment, err := ParseLocalFileURL(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error, got path=%q fragment=%q", tt.in, path, fragment)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if path != tt.path || fragment != tt.frac {
			t.Fatalf("%s: path=%q fragment=%q, want %q/%q", tt.in, path, fragment, tt.path, tt.frac)
		}
	}
}
