package runtime

import (
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestApplySkillsFlagPreservesSelectionSemantics(t *testing.T) {
	original := config.SkillsConfig{Enabled: true, AutoInvoke: true, AlwaysEnabled: []string{"original"}}
	for _, tc := range []struct {
		flag          string
		enabled, auto bool
		names         []string
	}{
		{"", true, true, []string{"original"}},
		{"none", false, true, []string{"original"}},
		{"all", true, true, []string{"original"}},
		{"*", true, true, []string{"original"}},
		{"one,two", true, false, []string{"one", "two"}},
		{" one, +,two ", true, true, []string{"one", "two"}},
		{"+", true, true, []string{"original"}},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got := ApplySkillsFlag(&original, tc.flag)
			if got.Enabled != tc.enabled || got.AutoInvoke != tc.auto || !reflect.DeepEqual(got.AlwaysEnabled, tc.names) {
				t.Fatalf("flag %q => %#v", tc.flag, got)
			}
			if (got == &original) != (tc.flag == "") {
				t.Fatal("empty must return original, explicit flags must copy")
			}
			if !original.Enabled || !original.AutoInvoke || !reflect.DeepEqual(original.AlwaysEnabled, []string{"original"}) {
				t.Fatal("mutated original")
			}
		})
	}
	for _, flag := range []string{"all", "*"} {
		disabled := config.SkillsConfig{}
		got := ApplySkillsFlag(&disabled, flag)
		if !got.Enabled || !got.AutoInvoke || disabled.Enabled || disabled.AutoInvoke {
			t.Fatalf("%q did not enable discovery on a copy: %#v", flag, got)
		}
	}

}
