package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is how the gateway identifies itself to an MCP client.
const ServerName = "no-mistakes"

// NewServer registers exactly the v1 tools.
//
// What is absent is part of the contract: there is no merge tool, no push tool,
// and no abort tool. Publication stays inside no-mistakes, and abort is a
// between-runs action that is easy to misuse mid-run.
//
// Tool results are returned as a normalized receipt. A refused call is marked
// as a tool error so the calling model sees the refusal and can act on it,
// rather than reading a refusal as a success.
func NewServer(svc *Service, version string) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: ServerName, Version: version}, nil)

	sdk.AddTool(server, &sdk.Tool{
		Name: "nomistakes_status",
		Description: "Read-only. Report the no-mistakes run for a repository's current branch, " +
			"or an explicitly named run: its state, step, gate findings, PR URL, full head SHA, " +
			"skipped validation, and what to do next.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, handler(func(ctx context.Context, in StatusInput) *Receipt {
		return svc.Status(ctx, in)
	}))

	sdk.AddTool(server, &sdk.Tool{
		Name: "nomistakes_run",
		Description: "Start a no-mistakes run for the repository's current branch, or reattach to the one " +
			"already in flight, and return at the first decision gate, the terminal outcome, or the bounded wait. " +
			"no-mistakes owns every mutation: review, tests, docs, lint, push, PR, and CI. " +
			"Gates are never resolved here - they come back for a decision. " +
			"intent is required: pass what the user set out to accomplish, not a description of the diff.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, handler(func(ctx context.Context, in RunInput) *Receipt {
		return svc.Run(ctx, in)
	}))

	sdk.AddTool(server, &sdk.Tool{
		Name: "nomistakes_respond",
		Description: "Answer the decision gate an active run is parked at with approve, fix, or skip. " +
			"A gate holding ask-user findings, or a protected-path refusal, is returned for a human decision " +
			"instead and is only answered once user_decision carries the decision that human actually made.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, handler(func(ctx context.Context, in RespondInput) *Receipt {
		return svc.Respond(ctx, in)
	}))

	sdk.AddTool(server, &sdk.Tool{
		Name:        "nomistakes_logs",
		Description: "Read-only. Return a bounded tail of one pipeline step's log for a run.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, handler(func(ctx context.Context, in LogsInput) *Receipt {
		return svc.Logs(ctx, in)
	}))

	sdk.AddTool(server, &sdk.Tool{
		Name: "nomistakes_sync",
		Description: "Inspect branch synchronization, and apply it or return custody only when no-mistakes' " +
			"own reported next action authorizes exactly that. Call it without apply or recover to read the state.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, handler(func(ctx context.Context, in SyncInput) *Receipt {
		return svc.Sync(ctx, in)
	}))

	sdk.AddTool(server, &sdk.Tool{
		Name: "nomistakes_doctor",
		Description: "Read-only. Report whether a repository is initialized for no-mistakes, whether the daemon " +
			"is running, and which pipeline agents this machine can launch.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, handler(func(ctx context.Context, in DoctorInput) *Receipt {
		return svc.Doctor(ctx, in)
	}))

	return server
}

// handler adapts a service method to the SDK's typed tool handler. The output
// type is `any` on purpose: the receipt is a stable documented shape, and
// generating a schema for its operation-specific data payload would constrain
// it without making any caller better off.
func handler[In any](fn func(context.Context, In) *Receipt) sdk.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		receipt := fn(ctx, in)
		return &sdk.CallToolResult{IsError: !receipt.OK}, receipt, nil
	}
}

// Serve runs the gateway on stdio until the client disconnects or ctx ends.
//
// stdout belongs to the protocol. Everything diagnostic goes to stderr,
// including the structured log below: a single stray byte on stdout corrupts
// the JSON-RPC stream.
func Serve(ctx context.Context, svc *Service, version string) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	slog.Info("no-mistakes mcp gateway starting",
		"version", version, "allowed_repo_roots", len(svc.Policy.Roots()))
	if len(svc.Policy.Roots()) == 0 {
		// Not fatal: the server still starts and answers, and every call
		// returns the configuration remediation rather than a silent nothing.
		slog.Warn("no repository roots are configured; every repository will be refused",
			"remediation", "set mcp.allowed_repo_roots in the no-mistakes global config")
	}
	if err := NewServer(svc, version).Run(ctx, &sdk.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}
