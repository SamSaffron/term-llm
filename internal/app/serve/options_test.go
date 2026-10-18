package serve

import (
	"strings"
	"testing"
	"time"
)

func TestOptionsValidate(t *testing.T) {
	t.Parallel()
	base := Options{Port: 8080, SessionTTL: time.Minute, SessionMax: 100, JobsWorkers: 4}
	for _, tc := range []struct {
		name, want string
		changed    bool
		modify     func(*Options)
	}{
		{"valid", "", false, func(*Options) {}},
		{"invalid port", "invalid --port", false, func(o *Options) { o.Port = 65536 }},
		{"invalid session TTL", "invalid --session-ttl", false, func(o *Options) { o.SessionTTL = 0 }},
		{"invalid session max", "invalid --session-max", false, func(o *Options) { o.SessionMax = 0 }},
		{"invalid carryover", "invalid --telegram-carryover-chars", false, func(o *Options) { o.TelegramCarryoverChars = -1 }},
		{"invalid jobs workers", "invalid --jobs-workers", false, func(o *Options) { o.JobsWorkers = 0 }},
		{"response timeout default", "", false, func(o *Options) { o.ResponseTimeout = 0 }},
		{"response timeout flag", "invalid --response-timeout", true, func(o *Options) { o.ResponseTimeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base
			tc.modify(&opts)
			err := opts.Validate(tc.changed)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResolveAuthMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode                string
		authSet, aliasSet, noAuth bool
		want                      string
		invalid                   bool
	}{
		{"default", "", false, false, false, "bearer", false},
		{"explicit none", "none", true, false, false, "none", false},
		{"no-auth alias", "bearer", false, true, true, "none", false},
		{"conflicting flags", "bearer", true, true, true, "", true},
		{"invalid mode", "invalid", true, false, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveAuthMode(tc.authSet, tc.mode, tc.aliasSet, tc.noAuth)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("ResolveAuthMode() = %q, %v; want %q, invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestParseToolMap(t *testing.T) {
	t.Parallel()
	got, err := ParseToolMap([]string{"client:read_file", "other:grep"})
	if err != nil || got["client"] != "read_file" || got["other"] != "grep" {
		t.Fatalf("ParseToolMap() = %v, %v", got, err)
	}
	for _, value := range []string{"missing-separator", ":tool", "name:"} {
		if _, err := ParseToolMap([]string{value}); err == nil {
			t.Errorf("ParseToolMap(%q) accepted malformed mapping", value)
		}
	}
}
