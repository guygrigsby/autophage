package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/perch/client"
)

func TestAuthLoginWritesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/mint" {
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "minted-token"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	cmd := newRootCmd()
	cmd.SetArgs([]string{"auth", "login", "--addr", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	path, err := client.TokenPath(appID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("token not written: %v", err)
	}
	if string(b) != "minted-token" {
		t.Errorf("token file = %q, want minted-token", b)
	}
	if filepath.Base(path) != "cli.token" {
		t.Errorf("unexpected token path %q", path)
	}
}

func TestRootHasVersion(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "autophage version ") {
		t.Errorf("output = %q", out.String())
	}
}
