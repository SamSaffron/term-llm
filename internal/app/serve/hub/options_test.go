package hub

import "testing"

func TestValidateBindPreservesPortAndAuthLimits(t *testing.T) {
	for _, tc := range []struct {
		host        string
		port        int
		auth, valid bool
	}{
		{"127.0.0.1", 1, false, true}, {"::1", 65535, false, true}, {"LOCALHOST", 8090, false, true},
		{"0.0.0.0", 8090, true, true}, {"0.0.0.0", 8090, false, false}, {"", 8090, false, false},
		{"127.0.0.2", 8090, false, false}, {"127.0.0.1", 0, true, false}, {"127.0.0.1", -1, true, false}, {"127.0.0.1", 65536, true, false},
	} {
		if err := ValidateBind(tc.host, tc.port, tc.auth); (err == nil) != tc.valid {
			t.Errorf("ValidateBind(%q,%d,%t) = %v", tc.host, tc.port, tc.auth, err)
		}
	}
}

func TestNormalizeBasePathPreservesHubRootAndRejections(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		valid     bool
	}{
		{"", "", true}, {"/", "", true}, {" hub/ ", "/hub", true}, {"/hub//child/", "/hub/child", true},
		{"https://example.com/hub", "", false}, {"/hub?x=1", "", false}, {"/hub#fragment", "", false}, {"/hub/../other", "", false}, {"../hub", "", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := NormalizeBasePath(tc.raw)
			if (err == nil) != tc.valid || (tc.valid && got != tc.want) {
				t.Fatalf("NormalizeBasePath(%q) = %q, %v", tc.raw, got, err)
			}
		})
	}
}
