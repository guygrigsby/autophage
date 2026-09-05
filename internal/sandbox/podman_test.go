package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func realPodman(t *testing.T) *Manager {
	t.Helper()
	bin, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not on PATH; sandbox container tests run on trig")
	}
	image := "localhost/autophage-sandbox:latest"
	if err := exec.Command(bin, "image", "exists", image).Run(); err != nil {
		t.Skipf("image %s not built; run make image", image)
	}
	return &Manager{Podman: bin, Image: image, WorkspacesDir: t.TempDir(), Memory: "1g", CPUs: "1", Pids: 256, BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf}
}

func TestContainerLifecycleToolsAndDiff(t *testing.T) {
	m := realPodman(t)
	bare := origin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Start(ctx, ws, "test-attempt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Teardown(context.Background(), c) })

	tools, closer, err := m.Tools(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	var bash, write interface {
		Execute(context.Context, json.RawMessage) (json.RawMessage, error)
	}
	for _, tool := range tools {
		switch tool.Name() {
		case "bash":
			bash = tool
		case "write":
			write = tool
		}
	}
	if bash == nil || write == nil {
		t.Fatal("bash or write tool missing from the toolbox")
	}
	out, err := bash.Execute(ctx, json.RawMessage(`{"command":"id -u && ls /work && (curl -sS -m 2 https://example.com >/dev/null 2>&1 && echo NET || echo NONET)"}`))
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(string(out), "1000") || !strings.Contains(string(out), "README.md") || !strings.Contains(string(out), "NONET") {
		t.Errorf("bash out = %s", out)
	}
	if _, err := write.Execute(ctx, json.RawMessage(`{"file_path":"new.txt","content":"a\nb\nc\n"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := bash.Execute(ctx, json.RawMessage(`{"command":"printf 'x\\n' >> README.md"}`)); err != nil {
		t.Fatal(err)
	}
	n, err := m.DiffLines(ctx, c, ws.BaseSha)
	if err != nil || n != 4 {
		t.Errorf("diff lines = %d %v, want 4 (three new, one appended)", n, err)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "new.txt")); err != nil {
		t.Error("write inside the container did not land in the host workspace")
	}
	if err := m.Teardown(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(m.Podman, "container", "exists", c.Name).Run(); err == nil {
		t.Error("container still exists after teardown")
	}
}
