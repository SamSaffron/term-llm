package hub

import "testing"

func TestResolveDelegationCwd(t *testing.T) {
	for _, tc := range []struct {
		workdir, req, want string
		err                bool
	}{
		{"/work/", "", "/work", false},
		{"/work", "sub/../x", "/work/x", false},
		{"/work", "/work/a", "/work/a", false},
		{"/work", "../etc", "", true},
		{"/work", "/other", "", true},
		{"rel", "", "", true},
		{"/", "", "", true},
		{"", "", "", true},
		{"/work", "a\x01b", "", true},
	} {
		got, err := ResolveDelegationCwd(tc.workdir, tc.req)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("ResolveDelegationCwd(%q,%q) = %q, %v; want %q err=%v", tc.workdir, tc.req, got, err, tc.want, tc.err)
		}
	}
}

func TestDelegationStatusFromRun(t *testing.T) {
	if DelegationStatusFromRun("timed_out") == DelegationStatusFromRun("succeeded") || DelegationStatusFromRun("weird") != DelegationStatusFromRun("running") {
		t.Fatal("unexpected status mapping")
	}
}
