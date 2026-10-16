package config

import (
	"strings"
	"testing"
)

func TestRerankSchemaAndValidation(t *testing.T) {
	for key, want := range map[string]any{"rerank.pool": 64, "rerank.timeout": "20s", "rerank.llamacpp.startup_timeout": "10m", "rerank.llamacpp.idle_timeout": "10m"} {
		got, ok := DefaultForKey(key)
		if !ok || got != want {
			t.Errorf("DefaultForKey(%q) = %v, %v, want %v", key, got, ok, want)
		}
	}
	if !IsKnownKey("rerank.api_key") || !IsKnownKey("rerank.llamacpp.args") {
		t.Fatal("rerank keys absent from schema")
	}
	cases := []struct {
		cfg  RerankConfig
		want string
	}{
		{RerankConfig{}, ""},
		{RerankConfig{Provider: "none"}, ""},
		{RerankConfig{Provider: "http", BaseURL: "http://127.0.0.1"}, ""},
		{RerankConfig{Provider: "other"}, "rerank.provider"},
		{RerankConfig{Provider: "http"}, "rerank.base_url"},
		{RerankConfig{Pool: -1}, "rerank.pool"},
		{RerankConfig{Timeout: "bad"}, "rerank.timeout"},
		{RerankConfig{Llamacpp: RerankLlamacppConfig{IdleTimeout: "-1s"}}, "rerank.llamacpp.idle_timeout"},
	}
	for _, tc := range cases {
		err := (&Config{Rerank: tc.cfg}).ValidateRerank()
		if tc.want == "" && err != nil {
			t.Errorf("%+v: %v", tc.cfg, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%+v: %v, want %q", tc.cfg, err, tc.want)
		}
	}
	t.Setenv("RERANK_TEST_SECRET", "example")
	key, err := (RerankConfig{APIKey: "${RERANK_TEST_SECRET}"}).Key().Resolve()
	if err != nil || key != "example" {
		t.Fatalf("credential = %q, %v", key, err)
	}
}
