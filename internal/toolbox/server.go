// Package toolbox serves agentcore's coding tools over MCP stdio from inside
// the sandbox container. It is the only code that runs on the agent's
// behalf inside the container; the daemon dials it with podman exec.
package toolbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	ac "github.com/voocel/agentcore"
	"github.com/voocel/agentcore/tools"
)

// Options tunes the served tools.
type Options struct {
	WorkDir     string
	BashTimeout time.Duration
	// Extra registers additional tools alongside the seven built-in ones.
	// Test-only: cmd/autophage-toolbox never sets it, there is no flag for
	// it, and it exists so server_test.go can register a fake tool (for
	// example one that panics) without a real subprocess.
	Extra []ac.Tool
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
	// bash inherits the toolbox process's own environment (it never sets
	// cmd.Env); the sandbox adapter, not the toolbox, is responsible for
	// keeping that environment clean.
	all := []ac.Tool{
		tools.NewRead(opts.WorkDir, state),
		tools.NewWrite(opts.WorkDir, state),
		tools.NewEdit(opts.WorkDir, state),
		tools.NewGrep(opts.WorkDir),
		tools.NewGlob(opts.WorkDir),
		tools.NewLs(opts.WorkDir),
		bash,
	}
	all = append(all, opts.Extra...)
	srv := mcp.NewServer(&mcp.Implementation{Name: "autophage-toolbox", Version: "v1"}, nil)
	for _, tool := range all {
		schema := tool.Schema()
		if schema["type"] == nil {
			schema["type"] = "object"
		}
		t := &mcp.Tool{Name: tool.Name(), Description: tool.Description(), InputSchema: schema}
		if ro, ok := tool.(interface{ ReadOnly(json.RawMessage) bool }); ok && ro.ReadOnly(nil) {
			t.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
		}
		srv.AddTool(t, handler(tool))
	}
	return srv, nil
}

// handler adapts one agentcore tool to an MCP tool handler: raw arguments in,
// the tool's JSON result out as text, errors as IsError results so the model
// reads them and continues. A panic inside the tool (agentcore's edit does
// block matching over attacker-influenced strings, the most exposed case) is
// recovered here rather than left to crash the toolbox process and the
// attempt with it: neither the MCP SDK's dispatch nor its jsonrpc2 transport
// recovers on our behalf, so this handler is the last line of defense.
func handler(tool ac.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("toolbox: tool %q panicked: %v\n%s", tool.Name(), r, debug.Stack())
				res = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("error: tool panicked: %v", r)}}}
				err = nil
			}
		}()
		args := req.Params.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out, execErr := tool.Execute(ctx, args)
		if execErr != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: execErr.Error()}}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
	}
}
