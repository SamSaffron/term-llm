package server

import "testing"

func TestIsLoopbackHostUsesExactBindAllowlist(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true}, {" localhost ", true}, {"LOCALHOST", true}, {"::1", true}, {" ::1 ", true},
		{"", false}, {"0.0.0.0", false}, {"127.0.0.2", false}, {"::ffff:127.0.0.1", false}, {"127.0.0.1:80", false}, {"example.com", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := IsLoopbackHost(tc.host); got != tc.want {
				t.Fatalf("IsLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}
