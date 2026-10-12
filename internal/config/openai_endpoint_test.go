package config

import (
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/openaiapi"
)

func TestConfigOpenAIEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		providers map[string]ProviderConfig
		want      openaiapi.Endpoint
	}{
		{
			name: "environment only",
			want: openaiapi.Endpoint{BaseURL: "https://env.example/v1", Headers: map[string]string{openaiapi.HeaderOrganization: "org-env"}},
		},
		{
			name: "native providers.openai resolves base_url and headers",
			providers: map[string]ProviderConfig{"openai": {
				BaseURL: "$(printf https://gateway.example/v1)",
				Headers: map[string]string{"OpenAI-Organization": "$(printf org-config)", "OpenAI-Project": "proj-config"},
			}},
			want: openaiapi.Endpoint{
				BaseURL: "https://gateway.example/v1",
				Headers: map[string]string{"OpenAI-Organization": "org-config", "OpenAI-Project": "proj-config"},
			},
		},
		{
			name: "non-native providers.openai entry is ignored",
			providers: map[string]ProviderConfig{"openai": {
				Type:    ProviderTypeOpenAICompat,
				BaseURL: "https://compat.example/v1",
			}},
			want: openaiapi.Endpoint{BaseURL: "https://env.example/v1", Headers: map[string]string{openaiapi.HeaderOrganization: "org-env"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(openaiapi.EnvBaseURL, "https://env.example/v1")
			t.Setenv(openaiapi.EnvOrganization, "org-env")
			t.Setenv(openaiapi.EnvProject, "")
			got, err := (&Config{Providers: tt.providers}).OpenAIEndpoint()
			if err != nil {
				t.Fatalf("OpenAIEndpoint: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("OpenAIEndpoint() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
