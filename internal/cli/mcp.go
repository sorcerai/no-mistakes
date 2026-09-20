package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/axiapi"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/mcp"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// newMCPCmd builds the `no-mistakes mcp` command tree: the MCP gateway that
// lets an external agent surface hand mutation work to no-mistakes instead of
// writing to a forge directly.
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol gateway for agent surfaces",
		Long: "Serve no-mistakes as an MCP server so ChatGPT, Hermes, Claude, Codex, or another\n" +
			"agent surface can drive the delivery pipeline without direct forge write access.\n\n" +
			"The gateway wraps AXI: no-mistakes still owns review, tests, docs, lint, push, PR,\n" +
			"and CI. It publishes nothing itself, cannot merge a pull request, and returns any\n" +
			"gate the pipeline referred to a human rather than answering it.\n\n" +
			"Repositories must be allowlisted in the global config under mcp.allowed_repo_roots;\n" +
			"a path outside those roots is refused.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newMCPServeCmd())
	return cmd
}

func newMCPServeCmd() *cobra.Command {
	var stdio bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP gateway on stdio",
		Long: "Run the MCP gateway. stdout carries MCP protocol messages only; diagnostics go\n" +
			"to stderr, so this command is meant to be launched by an MCP client rather than\n" +
			"read in a terminal.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !stdio {
				return fmt.Errorf("--stdio is required: stdio is the only transport in v1")
			}
			return runMCPServe(cmd)
		},
	}
	cmd.Flags().BoolVar(&stdio, "stdio", false, "serve over stdio (required; the only v1 transport)")
	return cmd
}

func runMCPServe(cmd *cobra.Command) error {
	service, err := mcpService()
	if err != nil {
		return err
	}
	return mcp.Serve(cmd.Context(), service, buildinfo.CurrentVersion())
}

// mcpService wires the gateway to this machine's no-mistakes installation.
//
// The repository allowlist comes from the operator's own global config only: it
// decides which repositories this machine will mutate on an external agent's
// behalf, so no repository may contribute to it.
func mcpService() (*mcp.Service, error) {
	p, err := paths.New()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	policy, err := mcp.NewRepositoryPolicy(cfg.MCP.AllowedRepoRoots)
	if err != nil {
		return nil, err
	}
	return &mcp.Service{AXI: &axiapi.LocalService{}, Policy: policy}, nil
}
