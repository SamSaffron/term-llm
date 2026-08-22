package passkeyauth

import "testing"

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name, url, base string
		explicit        bool
		wantBase        string
		wantErr         bool
	}{
		{"https root", "https://Hub.Example.com/", "", false, "", false},
		{"derive", "https://hub.example.com/hub/", "", false, "/hub", false},
		{"match", "https://hub.example.com/hub/", "/hub", true, "/hub", false},
		{"mismatch", "https://hub.example.com/hub/", "/other", true, "", true},
		{"localhost", "http://localhost:8090/hub/", "", false, "/hub", false},
		{"http domain", "http://hub.example.com/", "", false, "", true},
		{"ipv4", "https://127.0.0.1/", "", false, "", true},
		{"ipv6", "https://[::1]/", "", false, "", true},
		{"invalid port", "https://hub.example.com:99999/", "", false, "", true},
		{"empty port", "https://hub.example.com:/", "", false, "", true},
		{"invalid domain", "https://bad..example/", "", false, "", true},
		{"single label", "https://intranet/", "", false, "", true},
		{"userinfo", "https://u@hub.example.com/", "", false, "", true},
		{"query", "https://hub.example.com/?x=1", "", false, "", true},
		{"fragment", "https://hub.example.com/#x", "", false, "", true},
		{"dotdot", "https://hub.example.com/a/../b", "", false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseEndpoint(EndpointOptions{PublicURL: tt.url, BasePath: tt.base, BasePathExplicit: tt.explicit})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v", err)
			}
			if err == nil && got.BasePath != tt.wantBase {
				t.Fatalf("base=%q", got.BasePath)
			}
		})
	}
}
func TestSafeReturnPath(t *testing.T) {
	e, _ := ParseEndpoint(EndpointOptions{PublicURL: "https://hub.example.com/hub/"})
	for _, bad := range []string{"https://evil.test/", "//evil.test/", "/other", "/hub/../other", "/hub\\evil"} {
		if got := e.SafeReturnPath(bad); got != "/hub/" {
			t.Errorf("%q => %q", bad, got)
		}
	}
	if got := e.SafeReturnPath("/hub/node/a/?token=secret&x=1"); got != "/hub/node/a/" {
		t.Fatal(got)
	}
}

func FuzzParseEndpoint(f *testing.F) {
	for _, seed := range []string{"https://hub.example.com/hub/", "http://localhost:8090/", "https://127.0.0.1/", "%"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) { _, _ = ParseEndpoint(EndpointOptions{PublicURL: raw}) })
}
