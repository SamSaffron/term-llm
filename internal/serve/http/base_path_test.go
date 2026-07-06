package servehttp

import "testing"

func TestNormalizeBasePath(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "already normalized", raw: "/chat", want: "/chat"},
		{name: "trims spaces", raw: "  /chat  ", want: "/chat"},
		{name: "adds slash", raw: "chat", want: "/chat"},
		{name: "multi segment", raw: "/a/b", want: "/a/b"},
		{name: "trims trailing", raw: "/chat/", want: "/chat"},
		{name: "trims multiple trailing", raw: "/chat//", want: "/chat"},
		{name: "empty", raw: "  ", wantErr: true},
		{name: "root", raw: "/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeBasePath(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeBasePath: %v", err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeBasePath() = %q, want %q", got, tt.want)
			}
		})
	}
}
