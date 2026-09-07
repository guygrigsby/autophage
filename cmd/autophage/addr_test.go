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
