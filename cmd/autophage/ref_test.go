package main

import "testing"

func TestParseRef(t *testing.T) {
	repo, n, err := parseRef("guy/repo#7")
	if err != nil || repo != "guy/repo" || n != 7 {
		t.Errorf("got %q %d %v", repo, n, err)
	}
	if _, _, err := parseRef("repo#7"); err == nil {
		t.Error("missing owner accepted")
	}
	if _, _, err := parseRef("guy/repo#x"); err == nil {
		t.Error("non-numeric issue number accepted")
	}
}
