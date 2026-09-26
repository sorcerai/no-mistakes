package axiapi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEnsureDaemonContextHonorsCallerDeadline(t *testing.T) {
	original := ensureDaemon
	defer func() { ensureDaemon = original }()
	ensureDaemon = func(*paths.Paths) error {
		time.Sleep(time.Second)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := ensureDaemonContext(ctx, &paths.Paths{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want caller deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("setup took %s, exceeded caller deadline", elapsed)
	}
}

func TestReadLogTailBoundsLinesAndTrimsTrailingBlankRows(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "step.log")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(strings.Repeat("x", 128*1024) + "\n\n\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	lines, total, truncated, err := readLogTail(context.Background(), file, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(lines) != 1 || !truncated {
		t.Fatalf("lines=%d total=%d truncated=%v, want one bounded truncated line", len(lines), total, truncated)
	}
	if len(lines[0]) > 64*1024 {
		t.Fatalf("retained line length=%d, want bounded", len(lines[0]))
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture builds an initialized repository with its own NM_HOME so the service
// can be exercised without a daemon.
type fixture struct {
	svc      *LocalService
	repoPath string
	repo     *db.Repo
	paths    *paths.Paths
	db       *db.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoPath, "init", "-b", "main")
	gitRun(t, repoPath, "commit", "--allow-empty", "-m", "initial")
	gitRun(t, repoPath, "checkout", "-b", "feature/x")
	gitRun(t, repoPath, "commit", "--allow-empty", "-m", "work")

	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	resolved, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(resolved, "https://example.test/o/r.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{svc: &LocalService{Root: root}, repoPath: resolved, repo: repo, paths: p, db: database}
}

// TestStatusResolvesCurrentBranchRunWithFullHead pins the two facts a machine
// receipt depends on: the run is resolved from the repository directory the
// caller named (never the server's working directory), and the head SHA is
// carried at full length.
func TestStatusResolvesCurrentBranchRunWithFullHead(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(f.repo.ID, "feature/x", head, head)
	if err != nil {
		t.Fatal(err)
	}

	state, err := f.svc.Status(context.Background(), f.repoPath, "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state.RunID != run.ID {
		t.Errorf("RunID = %q, want %q", state.RunID, run.ID)
	}
	if state.HeadSHA != head {
		t.Errorf("HeadSHA = %q, want the full head %q", state.HeadSHA, head)
	}
	if len(state.HeadSHA) != 40 {
		t.Errorf("HeadSHA %q is not a full SHA", state.HeadSHA)
	}
	if state.Branch != "feature/x" {
		t.Errorf("Branch = %q", state.Branch)
	}
}

// TestStatusIsBranchScoped pins that a run on another branch is never
// substituted for the caller's own, matching axi status.
func TestStatusIsBranchScoped(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	if _, err := f.db.InsertRun(f.repo.ID, "other/branch", head, head); err != nil {
		t.Fatal(err)
	}
	state, err := f.svc.Status(context.Background(), f.repoPath, "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state.RunID != "" {
		t.Errorf("RunID = %q, want no run for this branch", state.RunID)
	}
	if state.Branch != "feature/x" {
		t.Errorf("Branch = %q, want the caller's branch", state.Branch)
	}
}

func TestExplicitRunIDCannotCrossRepositoryBoundary(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, other, "init", "-b", "main")
	gitRun(t, other, "commit", "--allow-empty", "-m", "initial")
	resolvedOther, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	otherRepo, err := f.db.InsertRepo(resolvedOther, "https://example.test/o/other.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := gitRun(t, resolvedOther, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(otherRepo.ID, "main-work", head, head)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.svc.Status(context.Background(), f.repoPath, run.ID)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Status cross-repository run error = %v, want not found", err)
	}
}

// TestStatusSurfacesGateFindings proves the gate, its findings, and their
// action classification survive the typed boundary.
func TestStatusSurfacesGateFindings(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(f.repo.ID, "feature/x", head, head)
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.db.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.MarshalFindingsJSON(types.Findings{
		Items: []types.Finding{
			{ID: "r1", Severity: types.FindingSeverityError, Action: types.ActionAskUser, File: "a.go", Description: "needs a decision"},
			{ID: "r2", Severity: types.FindingSeverityWarning, Action: types.ActionAutoFix, File: "b.go", Description: "mechanical"},
		},
		Summary: "two findings",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}

	state, err := f.svc.Status(context.Background(), f.repoPath, "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state.Gate == nil {
		t.Fatal("expected a gate")
	}
	if state.Gate.Step != string(types.StepReview) {
		t.Errorf("gate step = %q", state.Gate.Step)
	}
	if len(state.Gate.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(state.Gate.Findings))
	}
	if state.Gate.Findings[0].Action != types.ActionAskUser {
		t.Errorf("first finding action = %q, want ask-user", state.Gate.Findings[0].Action)
	}
}

func TestStatusPreservesApprovedTestException(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(f.repo.ID, "feature/x", head, head)
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.db.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"verdict":"inconclusive"}`
	if err := f.db.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	const reason = "operator accepts the unverified live scenario"
	if err := f.db.SetTestApprovalReason(step.ID, reason); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CompleteStep(step.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	state, err := f.svc.Status(context.Background(), f.repoPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if state.Outcome != "passed-with-override" || state.CIReady {
		t.Fatalf("approved Test exception reported as clean: %+v", state)
	}
	if !strings.Contains(state.TestOverrideReason, reason) || state.CIOverrideReason != "" {
		t.Fatalf("Test exception was lost or attributed to CI: %+v", state)
	}
}

// TestStatusTerminalOutcomeUsesSharedVocabulary proves the service reports the
// same outcome word AXI's CLI reports, including the skipped qualification.
func TestStatusTerminalOutcomeUsesSharedVocabulary(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(f.repo.ID, "feature/x", head, head)
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.db.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CompleteSkippedStep(step.ID, 0, 1, "", "repository declares no CI"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	state, err := f.svc.Status(context.Background(), f.repoPath, "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !state.Terminal {
		t.Error("run should be terminal")
	}
	if state.Outcome != "passed-with-skips" {
		t.Errorf("Outcome = %q, want passed-with-skips", state.Outcome)
	}
	if len(state.AutomaticSkips) != 1 || state.AutomaticSkips[0].Step != string(types.StepCI) {
		t.Errorf("AutomaticSkips = %#v", state.AutomaticSkips)
	}
	if state.CIReady {
		t.Error("a skipped CI step must never read as checks-passed")
	}
}

func TestStatusUsesPersistedCIReadiness(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(f.repo.ID, "feature/x", head, head)
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.db.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetRunCIReady(run.ID, true); err != nil {
		t.Fatal(err)
	}

	state, err := f.svc.Status(context.Background(), f.repoPath, "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !state.CIReady {
		t.Fatal("Status dropped persisted CI readiness")
	}
}

func TestStatusRejectsUninitializedRepository(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, other, "init", "-b", "main")
	gitRun(t, other, "commit", "--allow-empty", "-m", "initial")
	if _, err := f.svc.Status(context.Background(), other, ""); !errors.Is(err, ErrRepoNotInitialized) {
		t.Fatalf("err = %v, want ErrRepoNotInitialized", err)
	}
}

func TestLogsReturnsBoundedTail(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(f.repo.ID, "feature/x", head, head)
	if err != nil {
		t.Fatal(err)
	}
	dir := f.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "review.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	logs, err := f.svc.Logs(context.Background(), LogsRequest{RepoPath: f.repoPath, Step: "review", TailLines: 10})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if logs.TotalLines != 100 {
		t.Errorf("TotalLines = %d, want 100", logs.TotalLines)
	}
	if len(logs.Lines) != 10 {
		t.Errorf("returned %d lines, want the 10-line tail", len(logs.Lines))
	}
	if !logs.Truncated {
		t.Error("Truncated should be set when the tail elides lines")
	}
}

func TestLogsReadsCustomGateAndRejectsMalformedGateNames(t *testing.T) {
	f := newFixture(t)
	head := gitRun(t, f.repoPath, "rev-parse", "HEAD")
	run, err := f.db.InsertRun(f.repo.ID, "feature/x", head, head)
	if err != nil {
		t.Fatal(err)
	}
	dir := f.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	step := types.CustomGateStepName(types.StepTest, "mutation-budget")
	if err := os.WriteFile(filepath.Join(dir, string(step)+".log"), []byte("gate output\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logs, err := f.svc.Logs(context.Background(), LogsRequest{RepoPath: f.repoPath, Step: string(step)})
	if err != nil {
		t.Fatalf("Logs custom gate: %v", err)
	}
	if logs.TotalLines != 1 || len(logs.Lines) != 1 || logs.Lines[0] != "gate output" {
		t.Fatalf("custom gate logs = %+v, want one owned log line", logs)
	}

	for _, malformed := range []string{
		"gate.test.a/b",
		"gate.test.../../../etc/passwd",
		"gate.nope.mutation-budget",
	} {
		if _, err := f.svc.Logs(context.Background(), LogsRequest{RepoPath: f.repoPath, Step: malformed}); err == nil {
			t.Errorf("Logs(%q) succeeded, want malformed gate rejection", malformed)
		}
	}
}

func TestLogsRejectsUnknownStep(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Logs(context.Background(), LogsRequest{RepoPath: f.repoPath, Step: "nonsense"}); err == nil {
		t.Fatal("expected an error for an unknown step")
	}
}

func TestDoctorReportsRepositoryAndDaemonFacts(t *testing.T) {
	f := newFixture(t)
	report, err := f.svc.Doctor(context.Background(), f.repoPath)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !report.RepoInitialized {
		t.Error("repository should read as initialized")
	}
	if report.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q", report.DefaultBranch)
	}
	if report.DaemonRunning {
		t.Error("no daemon was started for this fixture")
	}
}
