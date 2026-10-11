package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestParseRepo(t *testing.T) {
	if got, err := parseRepo("guy/repo"); err != nil || got != "guy/repo" {
		t.Errorf("got %q %v", got, err)
	}
	for _, bad := range []string{"repo", "guy/", "/repo", "guy/repo/extra", ""} {
		if _, err := parseRepo(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestShortSha(t *testing.T) {
	if got := short("1111111111111111111111111111111111111111"); got != "1111111" {
		t.Errorf("got %q", got)
	}
	// Short input is handed back whole rather than panicking on the slice.
	if got := short("abc"); got != "abc" {
		t.Errorf("got %q", got)
	}
}

// Every bump command has to be reachable from the root, or it exists only in
// the source.
func TestBumpCommandsAreRegistered(t *testing.T) {
	root := newRootCmd()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"bumps", "bump", "retry", "watch", "unwatch", "stop-repair"} {
		if !have[want] {
			t.Errorf("%q is not registered on the root command", want)
		}
	}
}

func TestWatchArgsAreRepoOrAll(t *testing.T) {
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"owner/repo"}, true},
		{[]string{"--all"}, true},
		{nil, false},
		{[]string{"--all", "owner/repo"}, false},
	} {
		cmd := newWatchCmd()
		cmd.RunE = func(*cobra.Command, []string) error { return nil }
		cmd.SetArgs(c.args)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		if err := cmd.Execute(); (err == nil) != c.ok {
			t.Errorf("watch %v: err = %v, want ok=%v", c.args, err, c.ok)
		}
	}
}
