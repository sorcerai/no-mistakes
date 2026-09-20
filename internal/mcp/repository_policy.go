package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// RepositoryPolicy decides whether the gateway will touch a repository at all.
//
// It is a security boundary, so it fails closed at every step. A path it cannot
// canonicalize, a path that leaves its allowed root through `..` or a symlink,
// a path that is not a git repository, and a gateway with no configured roots
// are all refusals with a stable machine code - never a normalized fallback to
// some nearby path the caller did not name. Resolving before comparing is the
// whole point: the comparison has to happen in the one spelling the filesystem
// agrees on, or a link inside an allowed root would launder access to anything
// it points at.
type RepositoryPolicy struct {
	// roots are canonical absolute directories. A repository is allowed when
	// its canonical path is one of them or lies beneath one.
	roots []string
}

// Error codes. They are part of the gateway's machine contract: a caller
// branches on the code, and the remediation text tells an operator what to
// change.
const (
	CodeRepoPathRequired  = "repo_path_required"
	CodeRepoNotAllowed    = "repo_not_allowed"
	CodeRepoUnresolvable  = "repo_path_unresolvable"
	CodeNotAGitRepository = "not_a_git_repository"
	CodeNoAllowedRoots    = "no_allowed_roots"
)

// PolicyError is a typed refusal: what went wrong, and what to do about it.
type PolicyError struct {
	Code        string
	Message     string
	Remediation string
}

func (e *PolicyError) Error() string { return e.Message }

// NewRepositoryPolicy canonicalizes the configured roots once. A root that
// cannot be resolved is dropped rather than silently widening the allowlist to
// its unresolved spelling; a relative root is a configuration error, because
// there is no working directory to resolve it against that would stay true for
// the life of a long-running server.
func NewRepositoryPolicy(roots []string) (*RepositoryPolicy, error) {
	policy := &RepositoryPolicy{}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("mcp.allowed_repo_roots: %q is not an absolute path", root)
		}
		resolved, err := canonical(root)
		if err != nil {
			// A root that does not exist yet cannot contain a repository, so
			// dropping it changes nothing a caller could have reached.
			continue
		}
		policy.roots = append(policy.roots, resolved)
	}
	return policy, nil
}

// Roots returns the canonical allowed roots, for diagnostics.
func (p *RepositoryPolicy) Roots() []string {
	out := make([]string, len(p.roots))
	copy(out, p.roots)
	return out
}

// Resolve returns the canonical repository path, or the refusal that stops the
// call before it reaches AXI.
func (p *RepositoryPolicy) Resolve(repoPath string) (string, *PolicyError) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return "", &PolicyError{
			Code:        CodeRepoPathRequired,
			Message:     "repo_path is required.",
			Remediation: "Pass the absolute path of the repository to work in.",
		}
	}
	if !filepath.IsAbs(repoPath) {
		return "", &PolicyError{
			Code:        CodeRepoPathRequired,
			Message:     fmt.Sprintf("repo_path %q is not an absolute path.", repoPath),
			Remediation: "Pass an absolute repository path; the server has no working directory to resolve a relative one against.",
		}
	}
	if len(p.roots) == 0 {
		return "", &PolicyError{
			Code:        CodeNoAllowedRoots,
			Message:     "No repository roots are configured, so no repository may be used.",
			Remediation: "Add the canonical repository root to mcp.allowed_repo_roots in the no-mistakes global config.",
		}
	}
	resolved, err := canonical(repoPath)
	if err != nil {
		return "", &PolicyError{
			Code:        CodeRepoUnresolvable,
			Message:     fmt.Sprintf("repo_path %q could not be resolved.", repoPath),
			Remediation: "Check that the path exists and is readable.",
		}
	}
	if !p.contains(resolved) {
		// The refusal names only the path the caller already knows. The
		// configured roots are the operator's business, not the caller's.
		return "", &PolicyError{
			Code:        CodeRepoNotAllowed,
			Message:     fmt.Sprintf("repo_path %q is outside the configured repository roots.", repoPath),
			Remediation: "Add the canonical repository root to mcp.allowed_repo_roots in the no-mistakes global config.",
		}
	}
	repositoryRoot, err := git.FindGitRoot(resolved)
	if err != nil {
		return "", &PolicyError{
			Code:        CodeNotAGitRepository,
			Message:     fmt.Sprintf("repo_path %q is not inside a git repository.", repoPath),
			Remediation: "Point repo_path at a git working tree.",
		}
	}
	repositoryRoot, err = canonical(repositoryRoot)
	if err != nil || !p.contains(repositoryRoot) {
		return "", &PolicyError{
			Code:        CodeRepoNotAllowed,
			Message:     fmt.Sprintf("repo_path %q belongs to a repository outside the configured repository roots.", repoPath),
			Remediation: "Add the canonical repository root to mcp.allowed_repo_roots in the no-mistakes global config.",
		}
	}
	return repositoryRoot, nil
}

// canonical resolves a path to its real absolute spelling. It requires the path
// to exist: an unresolvable path is refused, so there is deliberately no lenient
// fallback here.
func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

// contains reports whether a canonical path is a configured root or lies
// beneath one. Containment is by path segment, so "/srv/repos-archive" is not
// inside "/srv/repos".
func (p *RepositoryPolicy) contains(resolved string) bool {
	for _, root := range p.roots {
		if resolved == root {
			return true
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil {
			continue
		}
		if rel == "." {
			return true
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}
