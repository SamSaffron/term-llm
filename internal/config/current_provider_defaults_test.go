package config

import "testing"

func TestCurrentProviderDefaultsLoadWithoutAnExplicitModel(t *testing.T) {
	for _, tc := range []struct{ name, model string }{
		{"openai", "gpt-6.1-sol"},
		{"chatgpt", "gpt-6.1-sol-medium"},
	} {
		for _, block := range []string{"", "providers:\n  " + tc.name + ": {}\n"} {
			t.Run(tc.name+"/"+block, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", home)
				writeTestConfig(t, home, "default_provider: "+tc.name+"\n"+block)
				cfg, err := loadUnresolved()
				if err != nil {
					t.Fatal(err)
				}
				pc := cfg.Providers[tc.name]
				if pc.Model != tc.model || pc.FastModel != "gpt-6-luna" {
					t.Fatalf("loaded %s defaults: model=%q fast=%q", tc.name, pc.Model, pc.FastModel)
				}
				if DefaultProviderModel(tc.name) != pc.Model || DefaultProviderFastModel(tc.name) != pc.FastModel {
					t.Fatal("schema defaults and loaded defaults drifted")
				}
			})
		}
	}
}

func TestCurrentProviderDefaultsPreserveExplicitSelections(t *testing.T) {
	for _, provider := range []string{"openai", "chatgpt"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			// Deliberately retained older selections must not be silently upgraded.
			writeTestConfig(t, home, "default_provider: "+provider+"\nproviders:\n  "+provider+":\n    model: gpt-5.4-high\n    fast_model: gpt-5.4-mini\n")
			cfg, err := loadUnresolved()
			if err != nil {
				t.Fatal(err)
			}
			pc := cfg.Providers[provider]
			if pc.Model != "gpt-5.4-high" || pc.FastModel != "gpt-5.4-mini" {
				t.Fatalf("operator selection overwritten: model=%q fast=%q", pc.Model, pc.FastModel)
			}
		})
	}
}
