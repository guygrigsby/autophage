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
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	ac "github.com/voocel/agentcore"

	"github.com/guygrigsby/autophage/internal/toolbox"
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

// panicTool is a test-only ac.Tool whose Execute panics, standing in for a
// real tool (agentcore's edit, doing block matching over attacker-influenced
// strings, is the most exposed) misbehaving on bad input.
type panicTool struct{}

func (panicTool) Name() string           { return "panic_test_tool" }
func (panicTool) Description() string    { return "test-only tool that always panics" }
func (panicTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (panicTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	panic("boom")
}

func TestToolboxRecoversToolPanics(t *testing.T) {
	srv, err := toolbox.New(toolbox.Options{WorkDir: t.TempDir(), Extra: []ac.Tool{panicTool{}}})
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "panic_test_tool"})
	if err != nil {
		t.Fatalf("a panic must come back as a result, not a Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("want IsError, got %+v", res)
	}
	if text := textOf(res); !strings.Contains(text, "tool panicked") {
		t.Errorf("result = %q, want it to contain %q", text, "tool panicked")
	}

	// The panic must not have taken the server down: a following call on
	// another tool still works.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "ls", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("ls after panic: %v", err)
	}
	if res.IsError {
		t.Errorf("ls after panic errored: %s", textOf(res))
	}
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func keys(m map[string]ac.Tool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
