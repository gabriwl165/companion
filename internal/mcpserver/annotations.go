package mcpserver

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/repocompanion/companion/internal/usage"
)

func boolPtr(v bool) *bool { return &v }

// addTool centralizes tool annotations so clients can distinguish read-only,
// destructive and idempotent operations.
func addTool[In, Out any](s *mcp.Server, p *provider, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	tool.Annotations = annotationsFor(tool.Name)
	mcp.AddTool(s, tool, instrumentTool(p, tool.Name, handler))
}

func instrumentTool[In, Out any](p *provider, name string, handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	if p.opts.Usage == nil {
		return handler
	}
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		event := usage.Event{Time: time.Now().UTC(), Tool: name}
		if req != nil {
			if info := req.ClientInfo(); info != nil {
				event.Client = info.Name
			}
		}
		result, out, err := handler(ctx, req, in)
		event.Failed = err != nil || (result != nil && result.IsError)
		if p.opts.Usage.Append(event) != nil {
			p.usageWarning.Do(func() { fmt.Fprintln(os.Stderr, "warning: could not record companion usage") })
		}
		return result, out, err
	}
}

func annotationsFor(name string) *mcp.ToolAnnotations {
	switch name {
	case "docs_get_context":
		return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(true)}
	case "docs_get_guidance", "docs_list", "docs_search", "docs_read", "docs_history", "docs_diff", "docs_validate_code_refs", "docs_stale", "docs_code_map", "docs_code_find":
		return &mcp.ToolAnnotations{ReadOnlyHint: true}
	case "docs_pull":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(true)}
	case "docs_review_comments":
		return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(true)}
	case "docs_change_start":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false}
	case "docs_change_join":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(true)}
	case "docs_write":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false}
	case "docs_patch":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false}
	case "docs_move":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false}
	case "docs_delete":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false}
	case "docs_record_decision", "docs_propose_pattern", "docs_scaffold_project":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(true)}
	case "docs_set_context":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(true)}
	case "docs_change_sync":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false, OpenWorldHint: boolPtr(true)}
	case "docs_change_submit":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false, OpenWorldHint: boolPtr(true)}
	case "docs_change_reopen":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(true)}
	case "docs_change_status":
		return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(true)}
	default:
		return nil
	}
}
