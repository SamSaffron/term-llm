package cmd

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway"
	"github.com/spf13/viper"
)

func TestGatewayEnrollWritesParseableConfigAndSecureTokenByDefault(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	stateDir := t.TempDir()
	store, err := gateway.OpenClientStore(filepath.Join(stateDir, "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, bootstrap, err := store.CreateEnrollment("satellite-test", gateway.Policy{AllowProviders: []string{"debug"}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := gateway.OpenStateSealer(filepath.Join(stateDir, "state.key"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := gateway.NewServer(gateway.ServerConfig{Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: store, Sealer: sealer})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	oldName, oldWrite, oldTokenFile, oldPrint := gatewayEnrollName, gatewayEnrollWrite, gatewayEnrollTokenFile, gatewayEnrollPrintOnly
	t.Cleanup(func() {
		gatewayEnrollName, gatewayEnrollWrite, gatewayEnrollTokenFile, gatewayEnrollPrintOnly = oldName, oldWrite, oldTokenFile, oldPrint
	})
	gatewayEnrollName = "satellite-test"
	gatewayEnrollWrite = true
	gatewayEnrollTokenFile = ""
	gatewayEnrollPrintOnly = false
	var output bytes.Buffer
	gatewayEnrollCmd.SetOut(&output)
	gatewayEnrollCmd.SetContext(t.Context())
	t.Cleanup(func() {
		gatewayEnrollCmd.SetOut(nil)
		gatewayEnrollCmd.SetContext(context.Background())
	})
	if err := runGatewayEnroll(gatewayEnrollCmd, []string{ts.URL, bootstrap}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "tlg1_") {
		t.Fatalf("default enrollment printed client token: %q", output.String())
	}

	tokenPath := filepath.Join(configHome, "term-llm", "gateway-token")
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o, want 600", info.Mode().Perm())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.URL != ts.URL || cfg.Gateway.TokenFile != tokenPath || cfg.Gateway.Token != "" {
		t.Fatalf("enrolled gateway config = %+v", cfg.Gateway)
	}
	token, err := cfg.Gateway.ResolveToken()
	if err != nil || !strings.HasPrefix(token, "tlg1_") {
		t.Fatalf("resolved enrolled token = %q, %v", token, err)
	}
}

func TestGatewayEnrollPrintOnlyIsExplicitAndDoesNotWrite(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	stateDir := t.TempDir()
	store, _ := gateway.OpenClientStore(filepath.Join(stateDir, "clients.json"))
	_, bootstrap, _ := store.CreateEnrollment("print-test", gateway.Policy{AllowProviders: []string{"debug"}}, time.Minute)
	sealer, _ := gateway.OpenStateSealer(filepath.Join(stateDir, "state.key"))
	server, _ := gateway.NewServer(gateway.ServerConfig{Config: &config.Config{Providers: map[string]config.ProviderConfig{}}, Clients: store, Sealer: sealer})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	oldName, oldWrite, oldTokenFile, oldPrint := gatewayEnrollName, gatewayEnrollWrite, gatewayEnrollTokenFile, gatewayEnrollPrintOnly
	t.Cleanup(func() {
		gatewayEnrollName, gatewayEnrollWrite, gatewayEnrollTokenFile, gatewayEnrollPrintOnly = oldName, oldWrite, oldTokenFile, oldPrint
	})
	gatewayEnrollName, gatewayEnrollWrite, gatewayEnrollTokenFile, gatewayEnrollPrintOnly = "print-test", true, "", true
	var output bytes.Buffer
	gatewayEnrollCmd.SetOut(&output)
	gatewayEnrollCmd.SetContext(t.Context())
	t.Cleanup(func() {
		gatewayEnrollCmd.SetOut(nil)
		gatewayEnrollCmd.SetContext(context.Background())
	})
	if err := runGatewayEnroll(gatewayEnrollCmd, []string{ts.URL, bootstrap}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "token: tlg1_") {
		t.Fatalf("print-only output omitted token: %q", output.String())
	}
	if _, err := os.Stat(filepath.Join(configHome, "term-llm", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("print-only wrote config: %v", err)
	}
}
