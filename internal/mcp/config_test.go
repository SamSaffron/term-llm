package mcp

import (
	"reflect"
	"testing"
)

func TestConfigServerNamesSortedAlphabetically(t *testing.T) {
	cfg := &Config{Servers: map[string]ServerConfig{
		"zeta":  {Command: "zeta"},
		"alpha": {Command: "alpha"},
		"Beta":  {Command: "beta"},
		"gamma": {Command: "gamma"},
	}}

	got := cfg.ServerNames()
	want := []string{"Beta", "alpha", "gamma", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ServerNames() = %v, want %v", got, want)
	}
}

func TestManagerAvailableServersSortedAlphabetically(t *testing.T) {
	mgr := NewManager()
	mgr.config = &Config{Servers: map[string]ServerConfig{
		"zeta":  {Command: "zeta"},
		"alpha": {Command: "alpha"},
		"Beta":  {Command: "beta"},
		"gamma": {Command: "gamma"},
	}}

	got := mgr.AvailableServers()
	want := []string{"Beta", "alpha", "gamma", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableServers() = %v, want %v", got, want)
	}
}
