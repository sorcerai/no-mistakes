package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestMCPServeRequiresAnExplicitTransport keeps the only v1 transport explicit,
// so adding another later cannot silently change what `mcp serve` does.
func TestMCPServeRequiresAnExplicitTransport(t *testing.T) {
	out, err := executeCmd("mcp", "serve")
	if err == nil {
		t.Fatalf("expected an error without --stdio; output: %s", out)
	}
	if !strings.Contains(err.Error(), "--stdio") {
		t.Errorf("err = %v, want a --stdio requirement", err)
	}
}

// TestMCPServiceReadsAllowedRootsFromGlobalConfig pins where the gateway's
// repository allowlist comes from: the operator's own global config, which no
// repository can contribute to.
func TestMCPServiceReadsAllowedRootsFromGlobalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	p := paths.WithRoot(home)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("mcp:\n  allowed_repo_roots:\n    - "+allowed+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	service, err := mcpService()
	if err != nil {
		t.Fatal(err)
	}
	roots := service.Policy.Roots()
	if len(roots) != 1 {
		t.Fatalf("roots = %v, want the one configured root", roots)
	}
	resolved, err := filepath.EvalSymlinks(allowed)
	if err != nil {
		t.Fatal(err)
	}
	if roots[0] != resolved {
		t.Errorf("root = %q, want %q", roots[0], resolved)
	}
}

// TestMCPServiceWithNoConfiguredRootsStillStarts pins that an unconfigured
// gateway is closed rather than broken: it builds, and every call is refused by
// the policy with remediation.
func TestMCPServiceWithNoConfiguredRootsStillStarts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	service, err := mcpService()
	if err != nil {
		t.Fatal(err)
	}
	if len(service.Policy.Roots()) != 0 {
		t.Errorf("roots = %v, want none", service.Policy.Roots())
	}
}
