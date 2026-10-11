package main

import "testing"

func TestDaemonAddrFollowsTheListenAddress(t *testing.T) {
	if got := daemonAddr("127.0.0.1:8091"); got != "http://127.0.0.1:8091" {
		t.Fatalf("daemonAddr = %q", got)
	}
	if got := daemonAddr(""); got != fallbackAddr {
		t.Fatalf("empty listen: %q", got)
	}
}

func TestAddrEnvOverridesConfig(t *testing.T) {
	for env, want := range map[string]string{
		"https://aph.guy.ts.net":  "https://aph.guy.ts.net",
		"http://10.0.0.5:8091":    "http://10.0.0.5:8091",
		"aph.guy.ts.net:8091":     "http://aph.guy.ts.net:8091",
		"https://aph.guy.ts.net/": "https://aph.guy.ts.net",
	} {
		t.Setenv("AUTOPHAGE_ADDR", env)
		if got := configuredAddr(); got != want {
			t.Errorf("AUTOPHAGE_ADDR=%q: got %q, want %q", env, got, want)
		}
	}
	t.Setenv("AUTOPHAGE_ADDR", "")
	if got := configuredAddr(); got == "" {
		t.Error("empty env must fall through to the config or fallback")
	}
}
