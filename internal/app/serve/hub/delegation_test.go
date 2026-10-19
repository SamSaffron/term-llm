package hub

import "testing"

func TestResolveDelegationCwd(t *testing.T) {
	cases := []struct {
		workdir, requested, want string
		wantErr                  bool
	}{
		{"/work", "", "/work", false},
		{"/work/", "", "/work", false},
		{"/work", "/work", "/work", false},
		{"/work", "/work/sub", "/work/sub", false},
		{"/work", "sub/dir", "/work/sub/dir", false},
		{"/work", "/worker", "", true},
		{"/work", "/etc", "", true},
		{"/work", "/work/../etc", "", true},
		{"/work", "../outside", "", true},
		{"", "", "", true},
		// Workdir canonicalization: relative or root workdirs are refused,
		// un-normalized ones are cleaned before containment checks.
		{"relative", "", "", true},
		{"/", "", "", true},
		{"/work/..", "", "", true},
		{"/work/.", "", "/work", false},
		{"/work/sub/..", "sub", "/work/sub", false},
		{"/wo\nrk", "", "", true},
		{"/work", "sub\x00dir", "", true},
		{"/work", "sub/../x", "/work/x", false},
		{"/work", "a\x01b", "", true},
	}
	for _, tc := range cases {
		got, err := ResolveDelegationCwd(tc.workdir, tc.requested)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ResolveDelegationCwd(%q, %q) = %q, want error", tc.workdir, tc.requested, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ResolveDelegationCwd(%q, %q) = %q, %v; want %q", tc.workdir, tc.requested, got, err, tc.want)
		}
	}
}

func TestDelegationStatusFromRun(t *testing.T) {
	if DelegationStatusFromRun("timed_out") == DelegationStatusFromRun("succeeded") || DelegationStatusFromRun("weird") != DelegationStatusFromRun("running") {
		t.Fatal("unexpected status mapping")
	}
}
