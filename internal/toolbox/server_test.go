package toolbox_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jessmcp "github.com/guygrigsby/jess/mcp"
	ac "github.com/voocel/agentcore"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "toolbox-bin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "autophage-toolbox")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/autophage-toolbox")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("build toolbox: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func dial(t *testing.T, workDir string) map[string]ac.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	tools, closer, err := jessmcp.Tools(ctx, []jessmcp.Server{{Name: "toolbox", Command: binary, Args: []string{"-workdir", workDir}, Bare: true}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	byName := map[string]ac.Tool{}
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

func TestToolboxServesTheSevenToolsBareNamed(t *testing.T) {
	tools := dial(t, t.TempDir())
	for _, name := range []string{"read", "write", "edit", "grep", "glob", "ls", "bash"} {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %q missing; have %v", name, keys(tools))
		}
	}
	if len(tools) != 7 {
		t.Errorf("tools = %v, want exactly seven", keys(tools))
	}
}

func TestToolboxWriteReadBashRoundTrip(t *testing.T) {
	work := t.TempDir()
	tools := dial(t, work)
	ctx := t.Context()

	// agentcore's write/read tools take "file_path", not "path" (verified
	// against tools/write.go and tools/read.go in the installed agentcore
	// v1.6.9).
	out, err := tools["write"].Execute(ctx, json.RawMessage(`{"file_path":"hello.txt","content":"hi there\n"}`))
	if err != nil {
		t.Fatalf("write: %v %s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "hello.txt")); string(got) != "hi there\n" {
		t.Errorf("file = %q", got)
	}
	out, err = tools["read"].Execute(ctx, json.RawMessage(`{"file_path":"hello.txt"}`))
	if err != nil || !strings.Contains(string(out), "hi there") {
		t.Errorf("read = %s %v", out, err)
	}
	out, err = tools["bash"].Execute(ctx, json.RawMessage(`{"command":"pwd && wc -l hello.txt"}`))
	if err != nil || !strings.Contains(string(out), work) || !strings.Contains(string(out), "1 hello.txt") {
		t.Errorf("bash = %s %v", out, err)
	}
}

func TestToolboxErrorsReachTheModelAsResults(t *testing.T) {
	tools := dial(t, t.TempDir())
	out, err := tools["read"].Execute(t.Context(), json.RawMessage(`{"file_path":"missing.txt"}`))
	if err != nil {
		t.Fatalf("a tool error must be a result the model reads, not a Go error (which trips agentcore's failure breaker): %v", err)
	}
	if !strings.Contains(string(out), "missing.txt") {
		t.Errorf("result should name the missing file, got %s", out)
	}
}

func TestToolboxMarksReadOnlyTools(t *testing.T) {
	tools := dial(t, t.TempDir())
	type readOnlyer interface{ ReadOnly(json.RawMessage) bool }
	for name, want := range map[string]bool{"read": true, "grep": true, "glob": true, "ls": true, "bash": false, "write": false, "edit": false} {
		ro, ok := tools[name].(readOnlyer)
		if !ok {
			t.Errorf("%s: adapted tool does not expose ReadOnly", name)
			continue
		}
		if got := ro.ReadOnly(nil); got != want {
			t.Errorf("%s: ReadOnly = %v, want %v", name, got, want)
		}
	}
}

func keys(m map[string]ac.Tool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
