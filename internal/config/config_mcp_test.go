package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadGlobal_MCPAllowedRepoRoots(t *testing.T) {
	first := filepath.Join(t.TempDir(), "repos")
	second := filepath.Join(t.TempDir(), "src")
	config := fmt.Sprintf("mcp:\n  allowed_repo_roots:\n    - '%s'\n    - '%s'\n", first, second)
	cfg, err := LoadGlobalFromBytes([]byte(config))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.MCP.AllowedRepoRoots) != 2 || cfg.MCP.AllowedRepoRoots[0] != first || cfg.MCP.AllowedRepoRoots[1] != second {
		t.Fatalf("AllowedRepoRoots = %#v", cfg.MCP.AllowedRepoRoots)
	}
}

func TestLoadGlobal_MCPDefaultsToNoRoots(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.MCP.AllowedRepoRoots) != 0 {
		t.Fatalf("AllowedRepoRoots = %#v, want empty by default", cfg.MCP.AllowedRepoRoots)
	}
}

func TestLoadGlobal_MCPRejectsRelativeRoot(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("mcp:\n  allowed_repo_roots:\n    - ../elsewhere\n"))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want a not-absolute rejection", err)
	}
}

// TestRepoConfigCannotCarryMCPSettings pins the trust boundary: the MCP
// allowlist decides which repositories this machine's no-mistakes will mutate
// on an external agent's behalf, so it is an operator setting only. A pushed
// branch writing an mcp block reaches nothing - the repository configuration
// has no such surface to widen.
func TestRepoConfigCannotCarryMCPSettings(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("mcp:\n  allowed_repo_roots:\n    - /\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(cfg, &RepoConfig{}) {
		t.Fatalf("an mcp block reached the repository configuration: %#v", cfg)
	}
}
