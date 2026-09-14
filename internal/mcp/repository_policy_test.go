package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func mkGitRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// realPath is the spelling the policy compares in, so a test on a platform
// whose temp directory is itself a symlink (macOS /var -> /private/var) asserts
// against the same canonical form.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestPolicyAllowsRepositoryUnderConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	repo := mkGitRepo(t, filepath.Join(root, "project"))
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	got, policyErr := policy.Resolve(repo)
	if policyErr != nil {
		t.Fatalf("Resolve: %+v", policyErr)
	}
	if got != realPath(t, repo) {
		t.Errorf("resolved = %q, want %q", got, realPath(t, repo))
	}
}

// TestPolicyAllowsTheRootItself covers a root that is itself the repository.
func TestPolicyAllowsTheRootItself(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, policyErr := policy.Resolve(root); policyErr != nil {
		t.Fatalf("Resolve: %+v", policyErr)
	}
}

func TestPolicyRejectsAllowlistedSubdirectoryOfRepository(t *testing.T) {
	repo := mkGitRepo(t, filepath.Join(t.TempDir(), "project"))
	allowed := filepath.Join(repo, "allowed")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	policy, err := NewRepositoryPolicy([]string{allowed})
	if err != nil {
		t.Fatal(err)
	}
	_, policyErr := policy.Resolve(allowed)
	if policyErr == nil {
		t.Fatal("a subdirectory allowlist must not grant access to its parent repository")
	}
	if policyErr.Code != CodeRepoNotAllowed {
		t.Errorf("code = %q, want %q", policyErr.Code, CodeRepoNotAllowed)
	}
}

func TestPolicyRejectsSiblingOutsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "allowed")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := mkGitRepo(t, filepath.Join(base, "elsewhere"))
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	_, policyErr := policy.Resolve(outside)
	if policyErr == nil {
		t.Fatal("expected a refusal for a repository outside every root")
	}
	if policyErr.Code != CodeRepoNotAllowed {
		t.Errorf("code = %q, want %q", policyErr.Code, CodeRepoNotAllowed)
	}
	if policyErr.Remediation == "" {
		t.Error("a refusal must carry remediation text")
	}
}

// TestPolicyRejectsPrefixSiblingNotUnderRoot pins that containment is by path
// segment, not string prefix: /allowed-elsewhere is not inside /allowed.
func TestPolicyRejectsPrefixSiblingNotUnderRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "allowed")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	sneaky := mkGitRepo(t, filepath.Join(base, "allowed-elsewhere"))
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, policyErr := policy.Resolve(sneaky); policyErr == nil {
		t.Fatal("a sibling sharing the root's name prefix must not be allowed")
	}
}

// TestPolicyTraversalCannotEscape pins that a path escaping its allowed root is
// refused outright. It is never normalized back into the root.
func TestPolicyTraversalCannotEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "allowed")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mkGitRepo(t, filepath.Join(base, "secret"))
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	escaped := filepath.Join(root, "..", "secret")
	resolved, policyErr := policy.Resolve(escaped)
	if policyErr == nil {
		t.Fatalf("traversal was accepted and resolved to %q", resolved)
	}
	if policyErr.Code != CodeRepoNotAllowed {
		t.Errorf("code = %q, want %q", policyErr.Code, CodeRepoNotAllowed)
	}
}

// TestPolicySymlinkEscapeIsRefused pins the security boundary the design calls
// out by name: a link inside an allowed root whose target lives outside it is a
// refusal, never a normalized fallback to the link's own location.
func TestPolicySymlinkEscapeIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "allowed")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := mkGitRepo(t, filepath.Join(base, "outside"))
	link := filepath.Join(root, "looks-inside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	resolved, policyErr := policy.Resolve(link)
	if policyErr == nil {
		t.Fatalf("symlink escape was accepted and resolved to %q", resolved)
	}
	if policyErr.Code != CodeRepoNotAllowed {
		t.Errorf("code = %q, want %q", policyErr.Code, CodeRepoNotAllowed)
	}
}

func TestPolicyRejectsNonGitDirectory(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "not-a-repo")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	_, policyErr := policy.Resolve(plain)
	if policyErr == nil {
		t.Fatal("expected a refusal for a non-git directory")
	}
	if policyErr.Code != CodeNotAGitRepository {
		t.Errorf("code = %q, want %q", policyErr.Code, CodeNotAGitRepository)
	}
}

func TestPolicyRejectsMissingPath(t *testing.T) {
	root := t.TempDir()
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, policyErr := policy.Resolve(filepath.Join(root, "nope")); policyErr == nil {
		t.Fatal("expected a refusal for a path that does not exist")
	}
	if _, policyErr := policy.Resolve(""); policyErr == nil {
		t.Fatal("expected a refusal for an empty path")
	}
	if _, policyErr := policy.Resolve("relative/path"); policyErr == nil {
		t.Fatal("expected a refusal for a relative path")
	}
}

// TestPolicyWithNoRootsRefusesEverything pins that an unconfigured gateway is
// closed, not open: the server must not accept arbitrary filesystem paths.
func TestPolicyWithNoRootsRefusesEverything(t *testing.T) {
	repo := mkGitRepo(t, filepath.Join(t.TempDir(), "project"))
	policy, err := NewRepositoryPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, policyErr := policy.Resolve(repo)
	if policyErr == nil {
		t.Fatal("a policy with no configured roots must refuse every repository")
	}
	if policyErr.Code != CodeNoAllowedRoots {
		t.Errorf("code = %q, want %q", policyErr.Code, CodeNoAllowedRoots)
	}
}

func TestNewRepositoryPolicyRejectsRelativeRoot(t *testing.T) {
	if _, err := NewRepositoryPolicy([]string{"relative"}); err == nil {
		t.Fatal("expected an error for a relative allowed root")
	}
}
