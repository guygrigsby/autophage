# autophage sandbox implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The sandbox: a toolbox binary that serves agentcore's coding tools over MCP stdio inside a container, the `autophage-sandbox` image with the toolchains and a dependency warmer, and the podman adapter that prepares a workspace, runs the prep and agent containers, dials the toolbox, measures the diff, commits and pushes, and tears down.

**Architecture:** `cmd/autophage-toolbox` is a tiny MCP server over agentcore's `read`, `write`, `edit`, `grep`, `glob`, `ls` and `bash`, rooted at `/work`. `deploy/Containerfile` builds one image with Go, Node, Python with uv, git, make and the toolbox, plus `warm-deps` for the networked prep phase. `internal/sandbox` is the only package that shells out to `podman` and `git`; it exposes a `Sandbox` interface in its own types (a workspace, a container, tools) that the runner in Plan E consumes. The container is the security boundary: no secrets inside, `--network=none` for the agent phase, all tools executed inside via `podman exec -i`.

**Tech Stack:** Go 1.26, `github.com/modelcontextprotocol/go-sdk v1.6.1` (server side, in the toolbox only), `github.com/voocel/agentcore v1.6.9` tools, `github.com/guygrigsby/jess/mcp` (client side, in the adapter), podman 5.x rootless on the deploy host, git.

**Spec:** `docs/specs/2026-09-04-autophage-design.md` (Sandbox section), `docs/adr/0002-sandbox-posture.md`, `docs/specs/2026-09-04-autophage-contracts.md` (the Sandbox port rows). Deviation recorded here: the Sandbox port lives in `internal/sandbox`, not `internal/resolution`, because its signatures carry tools and containers, which are not domain types; the contracts document is updated in Task 3.

## Global Constraints

- Repo `~/projects/autophage`, module `github.com/guygrigsby/autophage`, Go 1.26, commits straight to `main`. Plan C's Tasks 1 to 3 (the `internal/resolution` package) must exist; nothing else from Plan C is needed.
- Only `internal/sandbox` runs `podman` or `git`. Only `cmd/autophage-toolbox` imports the MCP SDK server. Only `internal/sandbox` imports `jess/mcp`.
- The container never receives a secret. Tokens are used by host-side git only, as a per-command header, never written to `.git/config`.
- Agent phase containers: `--network=none`, `--userns=keep-id`, `--cap-drop=all`, `--security-opt=no-new-privileges`, `--read-only` root with `--tmpfs /tmp`, memory, cpu and pids limits, the workspace at `/work`, cache volumes at `/cache`.
- Until jess is tagged with `mcp/`, `go.mod` carries `replace github.com/guygrigsby/jess => ../jess`; the same for llm in Plan E. Drop both when tags exist.
- Tests that need podman or the image skip with a clear message when they are absent (a workstation without podman has neither); they run on the deploy host. Every skip prints why.
- No em or en dashes and no Oxford commas anywhere. Commit messages terse, verb-first, prefixed `toolbox:`, `image:`, `sandbox:`. No Claude or Anthropic attribution, no `Co-Authored-By` or `Claude-Session` trailers of any kind. `make check` green before every commit. `git add <paths>`, never `git add -A`. `for i := range n` for counts. Do not push.

---

### Task 1: The toolbox binary

**Files:**
- Create: `cmd/autophage-toolbox/main.go`
- Create: `internal/toolbox/server.go`
- Test: `internal/toolbox/server_test.go`
- Modify: `go.mod` (MCP SDK, agentcore, jess replace)
- Modify: `Makefile` (`toolbox-build` target folded into `build`)

**Interfaces:**
- Produces: `toolbox.New(workDir string) (*mcp.Server, error)` that registers the seven tools rooted at `workDir`; `cmd/autophage-toolbox` runs it over stdio with `-workdir` (default `/work`) and `-bash-timeout` (default `15m`). Tool names are agentcore's own (`read`, `write`, `edit`, `grep`, `glob`, `ls`, `bash`); each call's result is the tool's JSON result as one text content block; a tool error is a result with `IsError: true` and the error text, so the model sees it and continues.

- [ ] **Step 1: Write the failing test**

`internal/toolbox/server_test.go` proves the real path: build the binary, dial it over stdio through `jess/mcp` exactly as the sandbox adapter will, list the tools, and call three of them against a temp workdir.

```go
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

	out, err := tools["write"].Execute(ctx, json.RawMessage(`{"path":"hello.txt","content":"hi there\n"}`))
	if err != nil {
		t.Fatalf("write: %v %s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "hello.txt")); string(got) != "hi there\n" {
		t.Errorf("file = %q", got)
	}
	out, err = tools["read"].Execute(ctx, json.RawMessage(`{"path":"hello.txt"}`))
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
	out, err := tools["read"].Execute(t.Context(), json.RawMessage(`{"path":"missing.txt"}`))
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
```

`jess/mcp` returns an `IsError` result's text as a successful result (agentcore would otherwise count it toward its consecutive-failure breaker and disable the tool), and carries the server's `readOnlyHint` through as `ReadOnly` so agentcore runs read-only tools concurrently. The third and fourth tests pin both.

- [ ] **Step 2: Wire the dependencies and run the test to verify it fails**

Run: `go mod edit -replace github.com/guygrigsby/jess=../jess && go get github.com/guygrigsby/jess@v0.0.0 github.com/modelcontextprotocol/go-sdk@v1.6.1 github.com/voocel/agentcore@v1.6.9; go mod tidy; go test ./internal/toolbox/ 2>&1 | head -5`
Expected: build failure of the toolbox command (no such package yet). If `go get` rejects `v0.0.0`, add the require line by hand as `github.com/guygrigsby/jess v0.0.0-00010101000000-000000000000` and let `go mod tidy` settle it under the replace.

- [ ] **Step 3: Write the server**

`internal/toolbox/server.go`:

```go
// Package toolbox serves agentcore's coding tools over MCP stdio from inside
// the sandbox container. It is the only code that runs on the agent's
// behalf inside the container; the daemon dials it with podman exec.
package toolbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	ac "github.com/voocel/agentcore"
	"github.com/voocel/agentcore/tools"
)

// Options tunes the served tools.
type Options struct {
	WorkDir     string
	BashTimeout time.Duration
}

// New builds the MCP server with the seven tools rooted at WorkDir. The
// read, write and edit tools share one FileReadState so edit-before-read
// validation works as it does in agentcore itself.
func New(opts Options) (*mcp.Server, error) {
	if opts.WorkDir == "" {
		return nil, fmt.Errorf("toolbox: work dir is required")
	}
	if opts.BashTimeout <= 0 {
		opts.BashTimeout = 15 * time.Minute
	}
	state := tools.NewFileReadState()
	bash := tools.NewBash(opts.WorkDir)
	bash.Timeout = opts.BashTimeout
	all := []ac.Tool{
		tools.NewRead(opts.WorkDir, state),
		tools.NewWrite(opts.WorkDir, state),
		tools.NewEdit(opts.WorkDir, state),
		tools.NewGrep(opts.WorkDir),
		tools.NewGlob(opts.WorkDir),
		tools.NewLs(opts.WorkDir),
		bash,
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "autophage-toolbox", Version: "v1"}, nil)
	for _, tool := range all {
		t := &mcp.Tool{Name: tool.Name(), Description: tool.Description(), InputSchema: tool.Schema()}
		if ro, ok := tool.(interface{ ReadOnly(json.RawMessage) bool }); ok && ro.ReadOnly(nil) {
			t.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
		}
		srv.AddTool(t, handler(tool))
	}
	return srv, nil
}

// handler adapts one agentcore tool to an MCP tool handler: raw arguments in,
// the tool's JSON result out as text, errors as IsError results so the model
// reads them and continues.
func handler(tool ac.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.Params.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out, err := tool.Execute(ctx, args)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
	}
}
```

`cmd/autophage-toolbox/main.go`:

```go
// Command autophage-toolbox serves the coding tools over MCP stdio inside the
// sandbox. It has no network, no secrets and no opinion; it executes what
// the daemon's agent asks, rooted at the work dir.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guygrigsby/autophage/internal/toolbox"
)

func main() {
	workDir := flag.String("workdir", "/work", "directory every tool is rooted at")
	bashTimeout := flag.Duration("bash-timeout", 15*time.Minute, "per-command limit for the bash tool")
	flag.Parse()
	log.SetOutput(os.Stderr)

	srv, err := toolbox.New(toolbox.Options{WorkDir: *workDir, BashTimeout: *bashTimeout})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
```

`AddTool` on the server panics when the schema is not an object; agentcore's tools all declare `"type": "object"`. If one does not, wrap it: `schema := tool.Schema(); if schema["type"] == nil { schema["type"] = "object" }`.

Makefile: add `toolbox-build: ## Build the sandbox toolbox` with `go build -o autophage-toolbox ./cmd/autophage-toolbox`, add it to `build`, add `/autophage-toolbox` to `.gitignore` and to `clean`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/toolbox/ -v 2>&1 | tail -15`
Expected: all three PASS. The stdio round trip goes through the real binary.

- [ ] **Step 5: Commit**

```bash
make check && git add go.mod go.sum .gitignore Makefile cmd/autophage-toolbox/ internal/toolbox/ && git commit -m "toolbox: serve agentcore coding tools over MCP stdio"
```

---

### Task 2: The sandbox image and the dependency warmer

**Files:**
- Create: `deploy/Containerfile`
- Create: `deploy/warm-deps`
- Create: `deploy/image_test.sh`
- Modify: `Makefile` (`image`, `image-test`)
- Modify: `.golangci.yml` or none (shell only)

**Interfaces:**
- Produces: the image `localhost/autophage-sandbox:latest` with: user `agent` (uid 1000), `/work` and `/cache/{go,npm,uv}` owned by it, Go 1.26 at `/usr/local/go`, Node 22 LTS, Python 3.12 with `uv`, git, make, gcc, `autophage-toolbox` at `/usr/local/bin/autophage-toolbox`, `warm-deps` at `/usr/local/bin/warm-deps`. Environment: `GOMODCACHE=/cache/go/mod`, `GOCACHE=/cache/go/build`, `GOFLAGS=-mod=mod`, `GOTOOLCHAIN=local`, `npm_config_cache=/cache/npm`, `UV_CACHE_DIR=/cache/uv`, `HOME=/home/agent`, `PATH` including `/usr/local/go/bin` and `/home/agent/go/bin`.
- `warm-deps` (run in the prep container, with network) detects lockfiles in `/work` and fetches: `go.mod` present: `go mod download all`; `package-lock.json`: `npm ci --ignore-scripts`; `pnpm-lock.yaml`: `corepack enable && pnpm install --frozen-lockfile --ignore-scripts`; `uv.lock`: `uv sync --frozen --no-install-project`; `requirements.txt` without `uv.lock`: `uv pip install --system -r requirements.txt` is not run (no venv target); logged and skipped. Exit 0 even when a fetch fails, printing which failed; the agent will see the missing dependency.

- [ ] **Step 1: Write the image test**

`deploy/image_test.sh` runs after `make image` and proves the image is what the plan says; it is the failing test until the Containerfile exists:

```bash
#!/usr/bin/env bash
# Prove the sandbox image: user, tools, env, no network in the agent shape.
set -euo pipefail
IMG="${1:-localhost/autophage-sandbox:latest}"
run() { podman run --rm --network=none --userns=keep-id "$IMG" "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

[ "$(run id -u)" = "1000" ] || fail "uid is not 1000"
run go version | grep -q 'go1.26' || fail "go 1.26 missing"
run node --version | grep -q '^v22' || fail "node 22 missing"
run python3 --version | grep -q '3.12' || fail "python 3.12 missing"
run uv --version >/dev/null || fail "uv missing"
run git --version >/dev/null || fail "git missing"
run make --version >/dev/null || fail "make missing"
run gcc --version >/dev/null || fail "gcc missing"
run autophage-toolbox -h 2>&1 | grep -q workdir || fail "toolbox missing"
run sh -c 'echo $GOMODCACHE' | grep -q '^/cache/go/mod$' || fail "GOMODCACHE not set"
run sh -c 'test -w /work && test -w /cache/go && test -w /cache/npm && test -w /cache/uv' || fail "/work or /cache not writable by agent"
if run sh -c 'curl -sS -m 3 https://proxy.golang.org >/dev/null 2>&1'; then fail "network reachable with --network=none"; fi
echo "✓ image ok"
```

Run: `chmod +x deploy/image_test.sh && deploy/image_test.sh`
Expected: fails because the image does not exist (podman missing locally also fails; this task's verification is on the deploy host, see Step 4).

- [ ] **Step 2: Write the Containerfile and warm-deps**

`deploy/Containerfile`:

```dockerfile
# autophage sandbox: every attempt runs its tools in a fresh container from
# this image. No secrets, no network in the agent phase. Built on the deploy host by
# `make image`.
FROM docker.io/library/golang:1.26-bookworm AS toolbox
WORKDIR /src
COPY go.mod go.sum ./
COPY ../jess /jess
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/autophage-toolbox ./cmd/autophage-toolbox

FROM docker.io/library/debian:bookworm-slim
ARG GO_VERSION=1.26.5
ARG NODE_MAJOR=22
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git make gcc g++ libc6-dev pkg-config \
      python3 python3-venv python3-pip xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C /usr/local -xz
RUN curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs && rm -rf /var/lib/apt/lists/* \
    && corepack enable
RUN curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh
COPY --from=toolbox /out/autophage-toolbox /usr/local/bin/autophage-toolbox
COPY deploy/warm-deps /usr/local/bin/warm-deps
RUN chmod 0755 /usr/local/bin/warm-deps /usr/local/bin/autophage-toolbox \
    && useradd --uid 1000 --create-home --shell /bin/bash agent \
    && mkdir -p /work /cache/go/mod /cache/go/build /cache/npm /cache/uv \
    && chown -R agent:agent /work /cache
USER agent
ENV HOME=/home/agent \
    PATH=/usr/local/go/bin:/home/agent/go/bin:/usr/local/bin:/usr/bin:/bin \
    GOMODCACHE=/cache/go/mod GOCACHE=/cache/go/build GOFLAGS=-mod=mod GOTOOLCHAIN=local \
    npm_config_cache=/cache/npm UV_CACHE_DIR=/cache/uv
WORKDIR /work
```

The toolbox stage copies `../jess` because of the `replace` directive; podman's build context must therefore be `~/projects` (the deploy host uses the same layout) with `-f autophage/deploy/Containerfile`. Adjust the `COPY` lines to `COPY autophage/go.mod autophage/go.sum ./`, `COPY jess /jess`, `COPY autophage/ .` and `COPY autophage/deploy/warm-deps ...` accordingly, and in the Makefile run `podman build -t localhost/autophage-sandbox:latest -f deploy/Containerfile ..`. When jess is published and the replace is dropped, remove the `COPY jess` line and build with context `.`.

`deploy/warm-deps`:

```bash
#!/usr/bin/env bash
# Fetch a repository's dependencies from its lockfiles into the cache volumes,
# in the networked prep container, so the agent phase can run with no network.
# Never fails the attempt: a fetch that fails is printed and skipped, and the
# agent finds out when it builds.
set -uo pipefail
cd /work || exit 0
status=0
try() {
  local what=$1; shift
  echo "warm-deps: $what"
  if ! "$@"; then echo "warm-deps: $what failed (continuing)"; status=1; fi
}
[ -f go.mod ] && try "go mod download" go mod download all
[ -f package-lock.json ] && try "npm ci" npm ci --ignore-scripts --no-audit --no-fund
[ -f pnpm-lock.yaml ] && try "pnpm install" pnpm install --frozen-lockfile --ignore-scripts
[ -f uv.lock ] && try "uv sync" uv sync --frozen --no-install-project
if [ -f requirements.txt ] && [ ! -f uv.lock ]; then echo "warm-deps: requirements.txt without uv.lock; skipped"; fi
[ $status -eq 0 ] && echo "warm-deps: done" || echo "warm-deps: done with failures"
exit 0
```

Makefile:

```make
IMAGE ?= localhost/autophage-sandbox:latest

image: ## Build the sandbox image (podman, context is the parent dir for the jess replace)
	podman build -t $(IMAGE) -f deploy/Containerfile ..

image-test: ## Prove the sandbox image
	deploy/image_test.sh $(IMAGE)
```

- [ ] **Step 3: Commit (the build is verified on the deploy host in Step 4)**

```bash
chmod +x deploy/warm-deps deploy/image_test.sh && make check && git add deploy/Containerfile deploy/warm-deps deploy/image_test.sh Makefile && git commit -m "image: sandbox image with toolchains, toolbox and the dependency warmer"
```

- [ ] **Step 4: Build and prove the image on the deploy host**

The deploy host is a deploy target: code flows through git, never edited there. Until autophage and jess are pushed, verify by syncing a throwaway copy: `rsync -a --exclude .git --exclude .superpowers ~/projects/autophage/ <host>:/tmp/autophage-image/autophage/ && rsync -a --exclude .git ~/projects/jess/ <host>:/tmp/autophage-image/jess/` then `ssh <host> 'cd /tmp/autophage-image/autophage && podman build -t localhost/autophage-sandbox:latest -f deploy/Containerfile .. 2>&1 | tail -5 && deploy/image_test.sh'`.
Expected: the build succeeds and the test prints `✓ image ok`. Record the exact output in the report. Delete `/tmp/autophage-image` on the deploy host afterwards. If the build fails on a package name, fix the Containerfile here, commit, and re-sync; never edit on the deploy host.

---

### Task 3: The podman adapter

**Files:**
- Create: `internal/sandbox/doc.go`
- Create: `internal/sandbox/sandbox.go` (the interface and types)
- Create: `internal/sandbox/git.go` (host-side git: clone, fetch, branch, rebase or merge, commit, push)
- Create: `internal/sandbox/podman.go` (containers: prep, start, exec, diff, teardown)
- Create: `internal/sandbox/manager.go` (the implementation tying both together)
- Test: `internal/sandbox/git_test.go` (real git, always runs)
- Test: `internal/sandbox/podman_test.go` (real podman and image, skips when absent)
- Modify: `docs/specs/2026-09-04-autophage-contracts.md` (Sandbox port rows: package noted as `internal/sandbox`)

**Interfaces:**
- Produces:

```go
// Workspace is a case's checkout on host disk, on its branch.
type Workspace struct {
	Path          string
	Repository    string
	Branch        string
	DefaultBranch string
	BaseSha       string // origin/<default> the branch was rebased onto
}

// Container is a running agent container.
type Container struct {
	Name string
}

// Sandbox is what the runner needs. The podman Manager implements it.
type Sandbox interface {
	Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error)
	Start(ctx context.Context, ws Workspace, attemptID string) (Container, error)
	Tools(ctx context.Context, c Container) ([]ac.Tool, io.Closer, error)
	DiffLines(ctx context.Context, c Container, baseSha string) (int, error)
	CommitAndPush(ctx context.Context, ws Workspace, token, message string) (headSha string, pushed bool, err error)
	Teardown(ctx context.Context, c Container) error
}

type Manager struct {
	Podman        string // binary, default "podman"
	Image         string
	WorkspacesDir string
	Memory        string // e.g. "4g"
	CPUs          string // e.g. "4"
	Pids          int    // e.g. 512
	BotName       string // git identity for commits
	BotEmail      string
	Logf          func(string, ...any)
}

func (m *Manager) ... // the six methods
```

Behavior:
- `Prepare`: `WorkspacesDir/<owner>/<name>`; first time `git clone --no-tags --branch <default> <cloneURL> <path>` with the token header; afterwards `git fetch --prune origin`. Then `git checkout -B <branch> origin/<branch>` when the branch exists on origin, else from `origin/<default>`. Then `git rebase origin/<default>`; on conflict `git rebase --abort` followed by `git merge --no-commit --no-ff origin/<default> || true`, leaving conflict markers for the agent (the brief tells it so). `BaseSha` is `git rev-parse origin/<default>`. Then the prep container: `podman run --rm --userns=keep-id -v <path>:/work:Z -v autophage-cache-go-<owner>-<name>:/cache/go -v autophage-cache-npm-<owner>-<name>:/cache/npm -v autophage-cache-uv-<owner>-<name>:/cache/uv <image> warm-deps` with a 20 minute timeout; its failure is logged and does not fail Prepare (warm-deps exits 0 by design; a podman failure here is `Failed{Infra}` territory and is returned).
- Every git command runs with `-c http.extraheader=AUTHORIZATION: basic <base64("x-access-token:" + token)>`, `-c user.name=<BotName>`, `-c user.email=<BotEmail>`, `-c core.hooksPath=/dev/null` (a repository's own hooks must not run on the host), and `GIT_TERMINAL_PROMPT=0`. The token never touches disk.
- `Start`: `podman run -d --name autophage-<attemptID> --network=none --userns=keep-id --cap-drop=all --security-opt=no-new-privileges --read-only --tmpfs /tmp:rw,size=1g --memory <Memory> --cpus <CPUs> --pids-limit <Pids> -v <path>:/work:Z -v <the three cache volumes> -w /work <image> sleep infinity`. Also set `GOPROXY=off` and `GOFLAGS=-mod=mod` via `-e` so a missing module fails fast instead of hanging on the network.
- `Tools`: `jessmcp.Tools(ctx, []jessmcp.Server{{Name: "toolbox", Command: m.Podman, Args: []string{"exec", "-i", c.Name, "autophage-toolbox", "-workdir", "/work"}, Bare: true}}, m.Logf)`.
- `DiffLines`: inside the container, `podman exec <name> sh -c 'cd /work && git add -N . && git diff --numstat <base> | awk "{a+=\$1; d+=\$2} END {print a+d}"'` (intent-to-add makes new files count; `git add -N` stores symlinks as links, nothing escapes). Binary files report `-` in numstat and are skipped by awk's numeric coercion.
- `CommitAndPush`: host-side, `git add -A`; if `git diff --cached --quiet` reports changes, `git commit -m <message>`; then `git push --force-with-lease origin HEAD:refs/heads/<branch>` with the header. Returns `HEAD` sha and whether anything was pushed (a push with no new commits is still made once so the branch exists remotely on the first attempt).
- `Teardown`: `podman rm -f <name>`, ignoring "no such container".
- Every external command has a context timeout: git 5 minutes, prep 20, podman run and rm 2, exec per call none beyond ctx.

- [ ] **Step 1: Write the git tests (real git, local remotes)**

`internal/sandbox/git_test.go` sets up a bare "origin" in a temp dir and a seeded default branch, then exercises Prepare's git half and CommitAndPush through `Manager` with `Podman` set to a stub script that exits 0 (so the prep container step is a no-op):

```go
package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin creates a bare remote with one commit on main and returns its path.
func origin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	git(t, root, "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "main")
	return bare
}

func stubPodman(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func manager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{Podman: stubPodman(t), Image: "x", WorkspacesDir: t.TempDir(), BotName: "autophage[bot]", BotEmail: "autophage[bot]@users.noreply.github.com", Logf: t.Logf}
}

func TestPrepareClonesThenFetchesAndBranches(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Path != filepath.Join(m.WorkspacesDir, "guy", "repo") || ws.Branch != "autophage/7" || len(ws.BaseSha) != 40 {
		t.Errorf("ws = %+v", ws)
	}
	if got := git(t, ws.Path, "branch", "--show-current"); got != "autophage/7" {
		t.Errorf("branch = %q", got)
	}
	if cfg := git(t, ws.Path, "config", "--list"); strings.Contains(cfg, "tok") || strings.Contains(cfg, "extraheader") {
		t.Errorf("token or header leaked into config:\n%s", cfg)
	}
	ws2, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil || ws2.BaseSha != ws.BaseSha {
		t.Errorf("second prepare: %+v %v", ws2, err)
	}
}

func TestCommitAndPushThenResumeRebases(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx := t.Context()
	ws, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, pushed, err := m.CommitAndPush(ctx, ws, "tok", "autophage: first pass")
	if err != nil || !pushed || len(sha) != 40 {
		t.Fatalf("push: %s %v %v", sha, pushed, err)
	}
	if got := git(t, bare, "log", "-1", "--format=%s", "autophage/7"); got != "autophage: first pass" {
		t.Errorf("remote branch log = %q", got)
	}
	if author := git(t, bare, "log", "-1", "--format=%an <%ae>", "autophage/7"); author != "autophage[bot] <autophage[bot]@users.noreply.github.com>" {
		t.Errorf("author = %q", author)
	}

	// main moves on; the resumed attempt must rebase onto it.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", bare, other)
	if err := os.WriteFile(filepath.Join(other, "NEWS.md"), []byte("news\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "NEWS.md")
	git(t, other, "commit", "-q", "-m", "news")
	git(t, other, "push", "-q", "origin", "main")

	ws2, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if ws2.BaseSha == ws.BaseSha {
		t.Error("base sha did not advance")
	}
	if _, err := os.Stat(filepath.Join(ws2.Path, "NEWS.md")); err != nil {
		t.Error("rebase did not bring NEWS.md in")
	}
	if _, err := os.Stat(filepath.Join(ws2.Path, "fix.txt")); err != nil {
		t.Error("rebase lost fix.txt")
	}
	_, pushed, err = m.CommitAndPush(ctx, ws2, "tok", "autophage: nothing new")
	if err != nil || !pushed {
		t.Errorf("force-with-lease push after rebase: %v %v", pushed, err)
	}
}

func TestPrepareLeavesConflictMarkersOnRebaseConflict(t *testing.T) {
	m := manager(t)
	bare := origin(t)
	ctx := t.Context()
	ws, _ := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err := os.WriteFile(filepath.Join(ws.Path, "README.md"), []byte("# ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CommitAndPush(ctx, ws, "tok", "ours"); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", bare, other)
	if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("# theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "commit", "-q", "-am", "theirs")
	git(t, other, "push", "-q", "origin", "main")

	ws2, err := m.Prepare(ctx, "guy/repo", bare, "autophage/7", "main", "tok")
	if err != nil {
		t.Fatalf("prepare must not fail on conflict: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(ws2.Path, "README.md"))
	if !strings.Contains(string(b), "<<<<<<<") {
		t.Errorf("no conflict markers left for the agent:\n%s", b)
	}
}
```

- [ ] **Step 2: Write the podman tests (skip without podman or the image)**

`internal/sandbox/podman_test.go`:

```go
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
		t.Skip("podman not on PATH; sandbox container tests run on the deploy host")
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
	if _, err := write.Execute(ctx, json.RawMessage(`{"path":"new.txt","content":"a\nb\nc\n"}`)); err != nil {
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
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/ 2>&1 | head -5`
Expected: build failure.

- [ ] **Step 4: Write the adapter**

`internal/sandbox/doc.go`:

```go
// Package sandbox prepares a case's workspace on host disk and runs the
// agent's tools inside a rootless podman container with no network and no
// secrets. It is the only package that runs git or podman.
package sandbox
```

`internal/sandbox/sandbox.go`: the `Workspace`, `Container`, `Sandbox` and `Manager` declarations from the Interfaces block above, plus:

```go
// run executes name with args under ctx, returning trimmed combined output.
// env is appended to the process environment.
func run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(redact(args), " "), err, s)
	}
	return s, nil
}

// redact hides header values in error messages.
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "http.extraheader=") {
			a = "http.extraheader=<redacted>"
		}
		out[i] = a
	}
	return out
}
```

`internal/sandbox/git.go`:

```go
package sandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const gitTimeout = 5 * time.Minute

// gitArgs prefixes every git invocation with the identity, the hooks
// override and the auth header for this token. Nothing is written to the
// repository's config.
func (m *Manager) gitArgs(token string, args ...string) []string {
	base := []string{
		"-c", "user.name=" + m.BotName,
		"-c", "user.email=" + m.BotEmail,
		"-c", "core.hooksPath=/dev/null",
	}
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		base = append(base, "-c", "http.extraheader=AUTHORIZATION: basic "+auth)
	}
	return append(base, args...)
}

func (m *Manager) git(ctx context.Context, dir, token string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	return run(ctx, dir, []string{"GIT_TERMINAL_PROMPT=0"}, "git", m.gitArgs(token, args...)...)
}

func (m *Manager) workspacePath(repository string) string {
	owner, name, _ := strings.Cut(repository, "/")
	return filepath.Join(m.WorkspacesDir, owner, name)
}

// checkout clones on first use, otherwise fetches, then puts the case branch
// in place on top of the current default branch. A rebase conflict is left
// as merge conflict markers for the agent.
func (m *Manager) checkout(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	path := m.workspacePath(repository)
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Workspace{}, err
		}
		if _, err := m.git(ctx, filepath.Dir(path), token, "clone", "--no-tags", "--branch", defaultBranch, cloneURL, path); err != nil {
			return Workspace{}, fmt.Errorf("clone %s: %w", repository, err)
		}
	} else {
		if _, err := m.git(ctx, path, token, "fetch", "--prune", "origin"); err != nil {
			return Workspace{}, fmt.Errorf("fetch %s: %w", repository, err)
		}
		_, _ = m.git(ctx, path, "", "rebase", "--abort")
		_, _ = m.git(ctx, path, "", "merge", "--abort")
		if _, err := m.git(ctx, path, "", "reset", "--hard"); err != nil {
			return Workspace{}, err
		}
		if _, err := m.git(ctx, path, "", "clean", "-fdx"); err != nil {
			return Workspace{}, err
		}
	}
	base, err := m.git(ctx, path, "", "rev-parse", "origin/"+defaultBranch)
	if err != nil {
		return Workspace{}, fmt.Errorf("default branch %s: %w", defaultBranch, err)
	}
	start := "origin/" + defaultBranch
	if _, err := m.git(ctx, path, "", "rev-parse", "--verify", "--quiet", "origin/"+branch); err == nil {
		start = "origin/" + branch
	}
	if _, err := m.git(ctx, path, "", "checkout", "-q", "-B", branch, start); err != nil {
		return Workspace{}, err
	}
	if start != "origin/"+defaultBranch {
		if _, err := m.git(ctx, path, "", "rebase", "origin/"+defaultBranch); err != nil {
			m.logf("rebase %s onto %s conflicted; leaving markers for the agent", branch, defaultBranch)
			_, _ = m.git(ctx, path, "", "rebase", "--abort")
			_, _ = m.git(ctx, path, "", "merge", "--no-commit", "--no-ff", "origin/"+defaultBranch)
		}
	}
	return Workspace{Path: path, Repository: repository, Branch: branch, DefaultBranch: defaultBranch, BaseSha: base}, nil
}

// CommitAndPush stages everything, commits when there is anything to
// commit, and pushes the branch with force-with-lease. The first push of a
// fresh branch always happens so the branch exists remotely.
func (m *Manager) CommitAndPush(ctx context.Context, ws Workspace, token, message string) (string, bool, error) {
	if _, err := m.git(ctx, ws.Path, "", "add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := m.git(ctx, ws.Path, "", "diff", "--cached", "--quiet"); err != nil {
		if _, err := m.git(ctx, ws.Path, "", "commit", "-q", "-m", message); err != nil {
			return "", false, err
		}
	}
	head, err := m.git(ctx, ws.Path, "", "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	if _, err := m.git(ctx, ws.Path, token, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+ws.Branch); err != nil {
		return head, false, err
	}
	return head, true, nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}
```

`internal/sandbox/podman.go`:

```go
package sandbox

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	jessmcp "github.com/guygrigsby/jess/mcp"
	ac "github.com/voocel/agentcore"
	"io"
)

func (m *Manager) podman() string {
	if m.Podman == "" {
		return "podman"
	}
	return m.Podman
}

func cacheVolumes(repository string) []string {
	slug := strings.ReplaceAll(repository, "/", "-")
	return []string{
		"-v", "autophage-cache-go-" + slug + ":/cache/go",
		"-v", "autophage-cache-npm-" + slug + ":/cache/npm",
		"-v", "autophage-cache-uv-" + slug + ":/cache/uv",
	}
}

// warm runs the dependency warmer in a networked prep container.
func (m *Manager) warm(ctx context.Context, ws Workspace) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	args := append([]string{"run", "--rm", "--userns=keep-id", "-v", ws.Path + ":/work:Z"}, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "warm-deps")
	out, err := run(ctx, "", nil, m.podman(), args...)
	m.logf("warm-deps %s: %s", ws.Repository, lastLine(out))
	return err
}

// Start runs the agent container: no network, hardened, workspace and caches
// mounted, idling until the daemon execs the toolbox into it.
func (m *Manager) Start(ctx context.Context, ws Workspace, attemptID string) (Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	name := "autophage-" + attemptID
	args := []string{"run", "-d", "--name", name, "--network=none", "--userns=keep-id",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,size=1g",
		"-e", "GOPROXY=off", "-e", "GOFLAGS=-mod=mod",
		"-v", ws.Path + ":/work:Z"}
	if m.Memory != "" {
		args = append(args, "--memory", m.Memory)
	}
	if m.CPUs != "" {
		args = append(args, "--cpus", m.CPUs)
	}
	if m.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(m.Pids))
	}
	args = append(args, cacheVolumes(ws.Repository)...)
	args = append(args, "-w", "/work", m.Image, "sleep", "infinity")
	if _, err := run(ctx, "", nil, m.podman(), args...); err != nil {
		return Container{}, fmt.Errorf("start container: %w", err)
	}
	return Container{Name: name}, nil
}

// Tools dials the toolbox inside the container over podman exec stdio.
func (m *Manager) Tools(ctx context.Context, c Container) ([]ac.Tool, io.Closer, error) {
	return jessmcp.Tools(ctx, []jessmcp.Server{{
		Name:    "toolbox",
		Command: m.podman(),
		Args:    []string{"exec", "-i", c.Name, "autophage-toolbox", "-workdir", "/work"},
		Bare:    true,
	}}, m.Logf)
}

// DiffLines counts added plus removed lines against base, inside the
// container so nothing the agent did can reach the host through a path.
func (m *Manager) DiffLines(ctx context.Context, c Container, baseSha string) (int, error) {
	script := "cd /work && git add -N . && git diff --numstat " + baseSha + " | awk '{a+=$1; d+=$2} END {print a+d}'"
	out, err := run(ctx, "", nil, m.podman(), "exec", c.Name, "sh", "-c", script)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(lastLine(out))
	if err != nil {
		return 0, fmt.Errorf("diff lines: %q", out)
	}
	return n, nil
}

func (m *Manager) Teardown(ctx context.Context, c Container) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := run(ctx, "", nil, m.podman(), "rm", "-f", c.Name)
	if err != nil && !strings.Contains(out, "no such container") {
		return err
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
```

`internal/sandbox/manager.go`:

```go
package sandbox

import "context"

var _ Sandbox = (*Manager)(nil)

// Prepare puts the case branch in place on host disk and warms dependencies
// in the networked prep container.
func (m *Manager) Prepare(ctx context.Context, repository, cloneURL, branch, defaultBranch, token string) (Workspace, error) {
	ws, err := m.checkout(ctx, repository, cloneURL, branch, defaultBranch, token)
	if err != nil {
		return Workspace{}, err
	}
	if err := m.warm(ctx, ws); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}
```

Note on `git clean -fdx` in `checkout`: it runs before the branch is (re)checked out and only on a workspace that already exists, so an interrupted previous attempt cannot leave junk that the new attempt commits. The previous attempt's commits are on the branch on origin (pushed on every end), so nothing committed is lost; uncommitted work from a crashed attempt is, by design.

- [ ] **Step 5: Run the tests locally (git half) and record the podman skip**

Run: `go test ./internal/sandbox/ -v 2>&1 | tail -20`
Expected: the three git tests PASS; `TestContainerLifecycleToolsAndDiff` SKIPs with the podman message.

- [ ] **Step 6: Run the container test on the deploy host**

Sync as in Task 2 Step 4, then `ssh <host> 'cd /tmp/autophage-image/autophage && go test ./internal/sandbox/ -run TestContainerLifecycleToolsAndDiff -v 2>&1 | tail -20'`. Expected: PASS, with `NONET` in the bash output proving no network. Record the output in the report. If `--userns=keep-id` and `:Z` labels misbehave on the deploy host (Fedora, SELinux), adjust the flags here, commit, re-sync.

- [ ] **Step 7: Update the contracts document and commit**

In `docs/specs/2026-09-04-autophage-contracts.md`, in the Internal ports table, change the `Sandbox` rows' Adapter column from `internal/sandbox` to `internal/sandbox (port and adapter; the port's types are tools and containers, not domain types)` on the first Sandbox row only, and add a sentence under the table: "The Sandbox and Agent ports are declared beside their adapters rather than in `internal/resolution`: their signatures carry tools and containers, which are not domain types. Resolution never calls them; the runner does."

```bash
make check && git add internal/sandbox/ docs/specs/2026-09-04-autophage-contracts.md && git commit -m "sandbox: podman adapter with hardened containers, toolbox dial, diff and push"
```

---

## Self-review

Spec coverage against ADR 0002 and the design's Sandbox section: tools in-container over MCP via `podman exec` (Tasks 1, 3); no secrets in the container, token only as a per-command header on host git (Task 3, tested for config leakage); `--network=none` for the agent phase with the prep container warming from lockfiles into per-repository cache volumes (Tasks 2, 3, tested with `NONET`); hardening flags (Task 3); fresh container per attempt with a persistent workspace and rebase on resume, conflicts left as markers (Task 3, tested); image contents and env (Task 2, tested by `image_test.sh`). Diff lines measured inside the container (Task 3).

Type consistency: `Manager` field names match between the declaration, the tests and the methods; `jessmcp.Server{Name, Command, Args, Bare}` matches jess/mcp; `Sandbox` interface method signatures match the implementations; `run` signature `(ctx, dir, env, name, args...)` used identically in git.go and podman.go.
