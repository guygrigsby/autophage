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
