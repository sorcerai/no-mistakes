package axiapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Errors the boundary distinguishes. A caller maps these onto its own error
// vocabulary; everything else is an opaque failure.
var (
	ErrRepoNotInitialized = errors.New("repository is not initialized for no-mistakes")
	ErrNotAGitRepository  = errors.New("not a git repository")
	ErrDetachedHEAD       = errors.New("detached HEAD: check out a branch before validating")
	// ErrDefaultBranch refuses to validate - and therefore publish - the
	// repository's own default branch. Changes reach it through a pull request.
	ErrDefaultBranch  = errors.New("refusing to validate the default branch")
	ErrIntentRequired = errors.New("intent is required to start a run")
	ErrNoRunForBranch = errors.New("no run exists for this branch")
	ErrNoGate         = errors.New("the run is not awaiting a decision")
	// ErrUserDecisionRequired reports a gate the pipeline referred to a human.
	// It is raised against the same live snapshot the action would land on, so
	// a run that advanced into an ask-user gate between a caller's read and its
	// response cannot be answered by the response it already had in flight.
	ErrUserDecisionRequired = errors.New("the gate holds findings the pipeline referred to a human")
)

// DirtyWorktreeError reports uncommitted work. The gate validates committed
// history, so a dirty tree would validate the wrong thing rather than the
// caller's change.
type DirtyWorktreeError struct{ Untracked []string }

func (e *DirtyWorktreeError) Error() string { return "uncommitted changes in the working tree" }

// BranchOwnershipError reports that the pipeline currently owns the branch, so
// starting a fresh run would discard commits that live only in the gate.
type BranchOwnershipError struct{ State branchsync.State }

func (e *BranchOwnershipError) Error() string {
	return "branch is owned by the pipeline: " + e.State.Safety
}

// NestedGateError reports a caller running inside an active validation step.
// Such a caller must return its assigned phase, never drive the pipeline.
type NestedGateError struct {
	RunID string
	Phase types.StepName
}

func (e *NestedGateError) Error() string {
	return "refusing pipeline control from an active no-mistakes validation step"
}

// Gate is a step parked on a decision, with the findings that decision is about.
type Gate struct {
	Step    string
	Status  string
	Summary string
	// Findings carry their own action classification (auto-fix, ask-user,
	// no-op) verbatim. Nothing here re-classifies them.
	Findings []types.Finding
	// ProtectedPathRefusal marks a gate the pipeline refused to resolve on its
	// own; it always requires an explicit decision.
	ProtectedPathRefusal bool
}

// RequiresUserDecision reports whether a gate must go back to a human. An
// ask-user finding is the pipeline saying it will not decide; a protected-path
// refusal is the pipeline saying it will not act. Neither is an agent's to
// resolve, so both consumers of this boundary ask the same question here.
func RequiresUserDecision(gate *Gate) bool {
	if gate == nil {
		return false
	}
	if gate.ProtectedPathRefusal {
		return true
	}
	for _, f := range gate.Findings {
		if f.ActionOrDefault() == types.ActionAskUser {
			return true
		}
	}
	return false
}

// RunState is the typed AXI run snapshot.
type RunState struct {
	RepoPath string
	RunID    string
	Branch   string
	// HeadSHA is always the full 40-character commit, never abbreviated: a
	// machine receipt that cannot be compared to a forge SHA is not evidence.
	HeadSHA          string
	Status           string
	Outcome          string
	PRURL            string
	RunError         string
	CIOverrideReason string
	Terminal         bool
	// CIReady is the checks-passed handoff: CI is monitoring and the daemon
	// persisted readiness. It is never inferred from any other state.
	CIReady bool
	// WaitElapsed marks a bounded hold that ended while the run kept going. It
	// is not a failure and not a terminal state.
	WaitElapsed    bool
	Steps          []StepState
	AutomaticSkips []AutomaticSkip
	Gate           *Gate
}

type RunRequest struct {
	RepoPath   string
	Intent     string
	BaseBranch string
	Skip       []types.StepName
	Wait       time.Duration
}

type RespondRequest struct {
	RepoPath     string
	RunID        string
	Action       types.ApprovalAction
	FindingIDs   []string
	Instructions map[string]string
	// UserDecisionGiven records that a human actually decided this gate. It is
	// the only thing that lets a gate holding ask-user findings, or a
	// protected-path refusal, be answered.
	UserDecisionGiven bool
	Wait              time.Duration
}

type LogsRequest struct {
	RepoPath  string
	RunID     string
	Step      string
	TailLines int
}

type StepLog struct {
	RunID      string
	Step       string
	Lines      []string
	TotalLines int
	Truncated  bool
}

type SyncRequest struct {
	RepoPath  string
	Apply     bool
	Recover   bool
	KeepLocal bool
}

type SyncState struct {
	State        string
	Relation     string
	Safety       string
	LocalHead    string
	PipelineHead string
	Changed      bool
	Recovered    bool
	// NextActionCode is what AXI itself says to do next. It is the only thing
	// that authorizes a mutating sync.
	NextActionCode    string
	NextActionCommand string
	SyncError         string
}

// AgentCandidate is one supported pipeline agent and the binaries it can launch
// through.
type AgentCandidate struct {
	Name     string
	Binaries []string
}

type AgentCheck struct {
	Name      string
	Available bool
}

type DoctorReport struct {
	RepoPath        string
	DaemonRunning   bool
	RepoInitialized bool
	DefaultBranch   string
	ConfigError     string
	Agents          []AgentCheck
}

// Service is the typed AXI boundary. Every method is scoped by an explicit
// repository directory and never changes the process working directory.
type Service interface {
	Status(ctx context.Context, repoPath, runID string) (*RunState, error)
	Run(ctx context.Context, req RunRequest) (*RunState, error)
	Respond(ctx context.Context, req RespondRequest) (*RunState, error)
	Logs(ctx context.Context, req LogsRequest) (*StepLog, error)
	Sync(ctx context.Context, req SyncRequest) (*SyncState, error)
	Doctor(ctx context.Context, repoPath string) (*DoctorReport, error)
	// GateContext classifies the calling process. A nested caller - one running
	// inside an active validation step - must not drive the pipeline.
	GateContext(ctx context.Context, repoPath string) (gatecontext.Result, error)
}

// LocalService drives the no-mistakes installation on this machine.
type LocalService struct {
	// Root overrides the no-mistakes home directory. Empty uses the operator's
	// own (NM_HOME or the default), which is what production does; tests pin it.
	Root string
}

var _ Service = (*LocalService)(nil)

// env is one call's resources. Every entry point opens and closes its own, so
// concurrent tool calls never share a database handle or a daemon connection.
type env struct {
	p         *paths.Paths
	d         *db.DB
	repo      *db.Repo
	cfg       *config.GlobalConfig
	cfgErr    error
	client    *ipc.Client
	repoPath  string
	gitRoot   string
	closeOnce bool
}

func (e *env) close() {
	if e.closeOnce {
		return
	}
	e.closeOnce = true
	if e.client != nil {
		e.client.Close()
	}
	if e.d != nil {
		e.d.Close()
	}
}

func (s *LocalService) resolvePaths() (*paths.Paths, error) {
	if s.Root != "" {
		return paths.WithRoot(s.Root), nil
	}
	return paths.New()
}

// openEnv resolves the no-mistakes home, opens the database, and finds the
// registered repository for repoPath. It deliberately takes the directory as an
// argument: the MCP gateway is long-lived and serves concurrent calls, so
// resolving through the process working directory would race.
func (s *LocalService) openEnv(repoPath string, ensureDaemon bool) (*env, error) {
	p, err := s.resolvePaths()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	if err := p.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("create directories: %w", err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	e := &env{p: p, d: d, repoPath: repoPath}
	repo, gitRoot, err := resolveRepo(d, repoPath)
	if err != nil {
		e.close()
		return nil, err
	}
	e.repo, e.gitRoot = repo, gitRoot
	cfg, cfgErr := config.LoadGlobal(p.ConfigFile())
	if cfgErr != nil {
		if ensureDaemon {
			if alive, _ := daemon.IsRunning(p); !alive {
				e.close()
				return nil, cfgErr
			}
		}
		cfg = config.DefaultGlobalConfig()
	}
	e.cfg, e.cfgErr = cfg, cfgErr
	if ensureDaemon {
		if err := daemon.EnsureDaemon(p); err != nil {
			e.close()
			return nil, fmt.Errorf("start daemon: %w", err)
		}
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			e.close()
			return nil, fmt.Errorf("connect to daemon: %w", err)
		}
		e.client = client
	}
	return e, nil
}

// resolveRepo finds the registered repository containing dir, following the
// same main-worktree fallback the CLI uses so a linked git worktree resolves to
// the checkout that was initialized.
func resolveRepo(d *db.DB, dir string) (*db.Repo, string, error) {
	gitRoot, err := git.FindGitRoot(dir)
	if err != nil {
		return nil, "", ErrNotAGitRepository
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		return nil, "", fmt.Errorf("get repo: %w", err)
	}
	if repo != nil {
		return repo, gitRoot, nil
	}
	mainRoot, err := git.FindMainRepoRoot(dir)
	if err != nil || mainRoot == gitRoot {
		return nil, gitRoot, ErrRepoNotInitialized
	}
	repo, err = d.GetRepoByPath(mainRoot)
	if err != nil {
		return nil, gitRoot, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, gitRoot, ErrRepoNotInitialized
	}
	return repo, gitRoot, nil
}

// currentBranch returns the checked-out branch, or "" for a detached HEAD -
// which owns no branch, so no run can be attributed to it.
func currentBranch(ctx context.Context, dir string) (string, error) {
	branch, err := git.CurrentBranch(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("determine current branch: %w", err)
	}
	if branch == "HEAD" {
		return "", nil
	}
	if branch == "" {
		return "", fmt.Errorf("determine current branch: git returned an empty branch name")
	}
	return branch, nil
}

// Status returns the run the caller's branch owns, or the explicitly named run.
// It never substitutes another branch's run for the caller's own: one clone
// commonly has several worktrees on different branches.
func (s *LocalService) Status(ctx context.Context, repoPath, runID string) (*RunState, error) {
	e, err := s.openEnv(repoPath, false)
	if err != nil {
		return nil, err
	}
	defer e.close()

	branch, branchErr := currentBranch(ctx, e.repoPath)
	if branchErr != nil && runID == "" {
		return nil, branchErr
	}
	run, err := resolveRun(e, runID, branch)
	if err != nil {
		return nil, err
	}
	if run == nil {
		if runID != "" {
			return nil, fmt.Errorf("run %q not found", runID)
		}
		return &RunState{RepoPath: e.repoPath, Branch: branch}, nil
	}
	steps, err := e.d.GetStepsByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("load steps: %w", err)
	}
	state := stateFromDB(run, steps)
	state.RepoPath = e.repoPath
	return state, nil
}

func resolveRun(e *env, runID, branch string) (*db.Run, error) {
	if runID != "" {
		run, err := e.d.GetRun(runID)
		if err != nil {
			return nil, fmt.Errorf("get run: %w", err)
		}
		return run, nil
	}
	if branch == "" {
		return nil, nil
	}
	active, err := e.d.GetActiveRun(e.repo.ID, branch)
	if err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	}
	if active != nil {
		return active, nil
	}
	runs, err := e.d.GetRunsByRepo(e.repo.ID)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	for _, run := range runs {
		if run.Branch == branch {
			return run, nil
		}
	}
	return nil, nil
}

// stateFromDB projects a database run onto the typed snapshot.
func stateFromDB(run *db.Run, steps []*db.StepResult) *RunState {
	state := &RunState{
		RunID:   run.ID,
		Branch:  run.Branch,
		HeadSHA: run.HeadSHA,
		Status:  string(run.Status),
	}
	if run.PRURL != nil {
		state.PRURL = *run.PRURL
	}
	if run.Error != nil {
		state.RunError = *run.Error
	}
	for _, s := range steps {
		step := StepState{Name: string(s.StepName), Status: string(s.Status)}
		if s.SkipReason != nil {
			step.SkipReason = *s.SkipReason
		}
		state.Steps = append(state.Steps, step)
		// Mirror the executor: a run's override reason is the first step that
		// recorded one, so a deliberate override never reads as a clean pass.
		if state.CIOverrideReason == "" && s.OverrideReason != nil && *s.OverrideReason != "" {
			state.CIOverrideReason = *s.OverrideReason
		}
		if state.Gate == nil && isGateStatus(string(s.Status)) {
			findingsJSON := ""
			if s.FindingsJSON != nil {
				findingsJSON = *s.FindingsJSON
			}
			state.Gate = gateFrom(string(s.StepName), string(s.Status), findingsJSON)
		}
	}
	finish(state, false, false)
	return state
}

// stateFromIPC projects a live daemon run snapshot onto the typed snapshot.
func stateFromIPC(run *ipc.RunInfo) *RunState {
	state := &RunState{
		RunID:            run.ID,
		Branch:           run.Branch,
		HeadSHA:          run.HeadSHA,
		Status:           string(run.Status),
		CIOverrideReason: run.CIOverrideReason,
	}
	if run.PRURL != nil {
		state.PRURL = *run.PRURL
	}
	if run.Error != nil {
		state.RunError = *run.Error
	}
	for _, s := range run.Steps {
		state.Steps = append(state.Steps, StepState{
			Name: string(s.StepName), Status: string(s.Status), SkipReason: s.SkipReason,
		})
		if state.Gate == nil && isGateStatus(string(s.Status)) {
			findingsJSON := ""
			if s.FindingsJSON != nil {
				findingsJSON = *s.FindingsJSON
			}
			state.Gate = gateFrom(string(s.StepName), string(s.Status), findingsJSON)
		}
	}
	finish(state, run.CIReady, run.CIReadyNoCI)
	return state
}

// finish fills the derived fields every snapshot shares, so the database and
// daemon paths cannot disagree about an outcome.
func finish(state *RunState, ciReady, ciReadyNoCI bool) {
	state.AutomaticSkips = AutomaticSkips(state.Steps)
	state.Terminal = TerminalStatus(state.Status)
	if state.Terminal {
		state.Outcome = Outcome(state.Status, state.CIOverrideReason, state.AutomaticSkips)
	}
	state.CIReady = CIReadyToMerge(state.Steps, ciReady, ciReadyNoCI)
}

func isGateStatus(status string) bool {
	return status == string(types.StepStatusAwaitingApproval) || status == string(types.StepStatusFixReview)
}

func gateFrom(step, status, findingsJSON string) *Gate {
	g := &Gate{
		Step:                 step,
		Status:               status,
		ProtectedPathRefusal: pipeline.HasProtectedPathRefusal(findingsJSON),
	}
	if findingsJSON == "" {
		return g
	}
	parsed, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return g
	}
	g.Findings = parsed.Items
	g.Summary = parsed.Summary
	return g
}

// Logs returns one step's log, tail-bounded so a caller cannot be handed an
// unbounded document.
func (s *LocalService) Logs(ctx context.Context, req LogsRequest) (*StepLog, error) {
	step := strings.TrimSpace(req.Step)
	if step == "" {
		return nil, fmt.Errorf("step is required")
	}
	if !validStep(types.StepName(step)) {
		return nil, fmt.Errorf("unknown step %q", step)
	}
	e, err := s.openEnv(req.RepoPath, false)
	if err != nil {
		return nil, err
	}
	defer e.close()

	branch, branchErr := currentBranch(ctx, e.repoPath)
	if branchErr != nil && req.RunID == "" {
		return nil, branchErr
	}
	run, err := resolveRun(e, req.RunID, branch)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNoRunForBranch
	}

	out := &StepLog{RunID: run.ID, Step: step}
	data, err := os.ReadFile(filepath.Join(e.p.RunLogDir(run.ID), step+".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read log: %w", err)
	}
	lines := splitLogLines(string(data))
	out.TotalLines = len(lines)
	out.Lines = lines
	if req.TailLines > 0 && len(lines) > req.TailLines {
		out.Lines = lines[len(lines)-req.TailLines:]
		out.Truncated = true
	}
	return out, nil
}

func validStep(step types.StepName) bool {
	for _, known := range types.AllSteps() {
		if step == known {
			return true
		}
	}
	return false
}

func splitLogLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Sync inspects or applies guarded branch synchronization. It performs no
// authorization of its own: the caller decides whether AXI's reported
// next_action permits the mutation it is about to request.
func (s *LocalService) Sync(ctx context.Context, req SyncRequest) (*SyncState, error) {
	e, err := s.openEnv(req.RepoPath, false)
	if err != nil {
		return nil, err
	}
	defer e.close()

	service := &branchsync.Service{
		DB: e.d, Repo: e.repo, WorkDir: e.repoPath, GateDir: e.p.RepoDir(e.repo.ID),
		Paths: e.p, RemoteTimeout: e.cfg.BranchSyncRemoteTimeout,
	}
	var state branchsync.State
	switch {
	case req.Recover:
		state = service.Recover(ctx, req.KeepLocal)
	case req.Apply:
		state = service.Apply(ctx)
	default:
		state = service.Refresh(ctx)
	}
	return syncStateFrom(state), nil
}

func syncStateFrom(state branchsync.State) *SyncState {
	out := &SyncState{
		State:        state.State,
		Relation:     state.Relation,
		Safety:       state.Safety,
		LocalHead:    state.Local.Head,
		PipelineHead: state.Pipeline.PushedHead,
		Changed:      state.Changed,
		Recovered:    state.Recovered,
		SyncError:    state.Error,
	}
	if state.NextAction != nil {
		out.NextActionCode = state.NextAction.Code
		out.NextActionCommand = state.NextAction.Command
	}
	return out
}

// Doctor reports the facts a caller needs to diagnose a repository that cannot
// be driven. It is deliberately a small subset of `no-mistakes doctor`, which
// stays the full human diagnostic.
func (s *LocalService) Doctor(ctx context.Context, repoPath string) (*DoctorReport, error) {
	p, err := s.resolvePaths()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	report := &DoctorReport{RepoPath: repoPath}
	alive, _ := daemon.IsRunning(p)
	report.DaemonRunning = alive
	if _, cfgErr := config.LoadGlobal(p.ConfigFile()); cfgErr != nil {
		report.ConfigError = cfgErr.Error()
	}
	report.Agents = AgentAvailability()
	e, err := s.openEnv(repoPath, false)
	if err != nil {
		if errors.Is(err, ErrRepoNotInitialized) || errors.Is(err, ErrNotAGitRepository) {
			return report, nil
		}
		return nil, err
	}
	defer e.close()
	report.RepoInitialized = true
	report.DefaultBranch = e.repo.DefaultBranch
	return report, nil
}

// AgentCandidates lists the pipeline agents this build supports and the
// binaries each one can launch through. It is the single inventory `doctor`
// reports from, whichever surface asks: a second hand-written list would name
// agents by their binaries instead of their configured names and would silently
// omit every ACP alias.
func AgentCandidates() []AgentCandidate {
	agents := []AgentCandidate{
		{"claude", []string{"claude"}},
		{"codex", []string{"codex"}},
		{"grok", []string{"grok"}},
		{"rovodev", []string{"acli"}},
		{"opencode", []string{"opencode"}},
		{"pi", []string{"pi"}},
		{"copilot", []string{"copilot"}},
		{"antigravity", []string{"agy"}},
		{"acpx", []string{"acpx"}},
	}
	for _, alias := range types.ACPAliases() {
		agents = append(agents, AgentCandidate{
			Name:     string(alias.Name),
			Binaries: []string{alias.DefaultCommandBinary(), "acpx"},
		})
	}
	return agents
}

// AgentAvailability reports which of those agents this machine can launch. The
// daemon needs at least one; a repository with none fails before its first
// step, which is the diagnosis this answers.
func AgentAvailability() []AgentCheck {
	candidates := AgentCandidates()
	checks := make([]AgentCheck, 0, len(candidates))
	for _, candidate := range candidates {
		available := false
		for _, binary := range candidate.Binaries {
			if _, err := exec.LookPath(binary); err == nil {
				available = true
				break
			}
		}
		checks = append(checks, AgentCheck{Name: candidate.Name, Available: available})
	}
	return checks
}

// GateContext classifies the calling process against the daemon, so a caller
// running inside an active validation step can be refused before it mutates
// anything.
func (s *LocalService) GateContext(ctx context.Context, repoPath string) (gatecontext.Result, error) {
	p, err := s.resolvePaths()
	if err != nil {
		return gatecontext.Result{}, fmt.Errorf("resolve paths: %w", err)
	}
	marker := gatecontext.MarkerPresent()
	alive, _ := daemon.IsRunning(p)
	if alive {
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			return gatecontext.Result{}, fmt.Errorf("connect to daemon for gate execution context: %w", err)
		}
		defer client.Close()
		var wire ipc.GateContextResult
		if err := client.Call(ipc.MethodGateContext, &ipc.GateContextParams{CWD: repoPath, MarkerPresent: marker}, &wire); err != nil {
			return gatecontext.Result{}, fmt.Errorf("classify gate execution context: %w", err)
		}
		return gatecontext.Result{
			Nested:           wire.Nested,
			ManagedGit:       wire.ManagedGit,
			AgentDescendant:  wire.AgentDescendant,
			DaemonDescendant: wire.DaemonDescendant,
			MarkerPresent:    wire.MarkerPresent,
			RunID:            wire.RunID,
			Phase:            wire.Phase,
		}, nil
	}
	var database *db.DB
	if opened, openErr := db.OpenReadOnly(p.DB()); openErr == nil {
		database = opened
		defer database.Close()
	} else if !os.IsNotExist(openErr) {
		return gatecontext.Result{}, fmt.Errorf("open gate registry read-only: %w", openErr)
	}
	return (gatecontext.Inspector{DB: database, Paths: p}).Inspect(ctx, gatecontext.Request{CWD: repoPath, MarkerPresent: marker})
}
