package main

import "testing"

func TestBuildConfigAggregateUsesOneServerWithExactlyTwoHundredProfile(t *testing.T) {
	cfg, err := buildConfig("/fixture", "/bin/eval", 200, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("aggregate servers = %d, want 1", len(cfg.Servers))
	}
	server, ok := cfg.Servers["federation"]
	if !ok {
		t.Fatalf("aggregate config = %#v, want federation", cfg.Servers)
	}
	if server.Command != "/bin/eval" || len(server.Args) != 3 || server.Args[0] != "aggregate-server" {
		t.Fatalf("aggregate server config = %#v", server)
	}
	if _, err := buildConfig("/fixture", "/bin/eval", 199, true); err == nil {
		t.Fatal("aggregate config accepted a non-200 profile")
	}
}

func TestBuildConfigRetainsTenServerMode(t *testing.T) {
	cfg, err := buildConfig("/fixture", "/bin/eval", 200, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 10 {
		t.Fatalf("federation servers = %d, want 10", len(cfg.Servers))
	}
}
