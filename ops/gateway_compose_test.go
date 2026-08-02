package ops

import (
	"bytes"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGatewayComposeExampleParsesAndHasHealthGating(t *testing.T) {
	data, err := os.ReadFile("gateway-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			Image       string `yaml:"image"`
			Healthcheck struct {
				Test     []string `yaml:"test"`
				Interval string   `yaml:"interval"`
				Timeout  string   `yaml:"timeout"`
				Retries  int      `yaml:"retries"`
			} `yaml:"healthcheck"`
			DependsOn map[string]struct {
				Condition string `yaml:"condition"`
			} `yaml:"depends_on"`
		} `yaml:"services"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&document); err != nil {
		t.Fatalf("compose YAML is invalid: %v", err)
	}
	for _, name := range []string{"gateway", "satellite"} {
		service, ok := document.Services[name]
		if !ok || service.Image == "" {
			t.Fatalf("missing %s service/image", name)
		}
		if len(service.Healthcheck.Test) < 4 || service.Healthcheck.Test[0] != "CMD" || service.Healthcheck.Interval == "" || service.Healthcheck.Timeout == "" || service.Healthcheck.Retries <= 0 {
			t.Fatalf("%s healthcheck is incomplete: %+v", name, service.Healthcheck)
		}
	}
	if got := document.Services["satellite"].DependsOn["gateway"].Condition; got != "service_healthy" {
		t.Fatalf("satellite gateway dependency condition = %q", got)
	}
}
