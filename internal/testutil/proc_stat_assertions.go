package testutil

import "testing"

// AssertProcStatState verifies Linux /proc stat parsing edge cases.
func AssertProcStatState(t *testing.T, parse func([]byte) (byte, bool)) {
	t.Helper()
	cases := map[string]struct {
		stat string
		want byte
		ok   bool
	}{
		"running":              {stat: "123 (sleep) S 1 2 3", want: 'S', ok: true},
		"zombie":               {stat: "123 (sleep) Z 1 2 3", want: 'Z', ok: true},
		"parenthesis in name":  {stat: "123 (sleep) helper) Z 1 2 3", want: 'Z', ok: true},
		"embedded parenthesis": {stat: "123 (odd)name) R 1 2 3", want: 'R', ok: true},
		"missing name":         {stat: "123 sleep Z 1 2 3"},
		"missing state":        {stat: "123 (sleep)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := parse([]byte(tc.stat))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parse(%q) = %q, %v; want %q, %v", tc.stat, got, ok, tc.want, tc.ok)
			}
		})
	}
}
