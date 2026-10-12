package openaiapi

import (
	"net/http"
	"reflect"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		configured Endpoint
		want       Endpoint
	}{
		{
			name: "defaults",
			want: Endpoint{BaseURL: DefaultBaseURL},
		},
		{
			name: "environment",
			env:  map[string]string{EnvBaseURL: "https://proxy.example/v1/", EnvOrganization: "org-env", EnvProject: "proj-env"},
			want: Endpoint{BaseURL: "https://proxy.example/v1", Headers: map[string]string{HeaderOrganization: "org-env", HeaderProject: "proj-env"}},
		},
		{
			name: "configured headers win over environment case-insensitively",
			env:  map[string]string{EnvBaseURL: "https://proxy.example/v1", EnvOrganization: "org-env", EnvProject: "proj-env"},
			configured: Endpoint{
				BaseURL: "https://config.example/v1",
				Headers: map[string]string{"openai-organization": " org-config ", "X-Proxy-Key": "secret", "authorization": "Bearer other", "content-type": "text/plain", "Accept": "text/html"},
			},
			want: Endpoint{
				BaseURL: "https://config.example/v1",
				Headers: map[string]string{"openai-organization": "org-config", HeaderProject: "proj-env", "X-Proxy-Key": "secret"},
			},
		},
		{
			name:       "blank configured header falls back to environment",
			env:        map[string]string{EnvProject: "proj-env"},
			configured: Endpoint{Headers: map[string]string{HeaderProject: "  "}},
			want:       Endpoint{BaseURL: DefaultBaseURL, Headers: map[string]string{HeaderProject: "proj-env"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{EnvBaseURL, EnvOrganization, EnvProject} {
				t.Setenv(key, tt.env[key])
			}
			if got := Resolve(tt.configured); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolveDoesNotMutateConfiguredHeaders(t *testing.T) {
	t.Setenv(EnvOrganization, "org-env")
	configured := map[string]string{"x-custom": "1"}
	Resolve(Endpoint{Headers: configured})
	if !reflect.DeepEqual(configured, map[string]string{"x-custom": "1"}) {
		t.Fatalf("configured headers mutated: %#v", configured)
	}
}

func TestPublicIgnoresEnvironment(t *testing.T) {
	t.Setenv(EnvBaseURL, "https://proxy.example/v1")
	t.Setenv(EnvOrganization, "org-env")
	if got := Public(); !reflect.DeepEqual(got, Endpoint{BaseURL: DefaultBaseURL}) {
		t.Fatalf("Public() = %#v", got)
	}
}

func TestEndpointURLAndHeaders(t *testing.T) {
	e := Endpoint{BaseURL: "https://proxy.example/v1/", Headers: map[string]string{HeaderOrganization: "org-1", HeaderProject: "proj-1"}}
	if got := e.URL("/models"); got != "https://proxy.example/v1/models" {
		t.Fatalf("URL = %q", got)
	}
	h := http.Header{}
	e.Apply(h)
	if h.Get(HeaderOrganization) != "org-1" || h.Get(HeaderProject) != "proj-1" {
		t.Fatalf("headers = %#v", h)
	}
	copied := e.HeaderMap()
	copied[HeaderProject] = "changed"
	if e.Headers[HeaderProject] != "proj-1" {
		t.Fatal("HeaderMap must return a copy")
	}
	if got := (Endpoint{}).HeaderMap(); got != nil {
		t.Fatalf("empty headers = %#v", got)
	}
}
