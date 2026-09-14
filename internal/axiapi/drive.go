package axiapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// triggerWaitTimeout bounds how long a launcher waits for the daemon to
// register a run after pushing to the gate before falling back to a rerun.
const triggerWaitTimeout = 5 * time.Second

// DefaultWait is the hold cap for Run and Respond. It sits under a typical
// ten-minute agent tool budget so the call returns a still-running snapshot the
// caller can reattach to, rather than hanging until its harness kills it.
const DefaultWait = 8 * time.Minute

// Run starts a run for the caller's branch, or reattaches to the one already in
// flight, and drives it until the next decision point, a terminal outcome, or
// the bounded wait.
//
// It never resolves a gate. Gate decisions belong to the caller (and, for an
// ask-user finding, to a human): this boundary exists to hand the gate back,
// not to answer it. The CLI's --yes auto-resolution has no equivalent here.
func (s *LocalService) Run(ctx context.Context, req RunRequest) (*RunState, error) {
	wait := req.Wait
	if wait <= 0 {
		wait = DefaultWait
	}
	e, err := s.openEnv(req.RepoPath, true)
	if err != nil {
		return nil, err
	}
	defer e.close()

	branch, err := currentBranch(ctx, e.repoPath)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, ErrDetachedHEAD
	}
	headSHA, err := git.Run(ctx, e.repoPath, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("get current HEAD: %w", err)
	}
	driveCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	runID, err := s.attach(driveCtx, e, branch, headSHA)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		if e.cfgErr != nil {
			return nil, e.cfgErr
		}
		// Intent is mandatory when starting a run: the caller knows why the
		// change exists, so it is taken directly instead of inferred.
		if strings.TrimSpace(req.Intent) == "" {
			return nil, ErrIntentRequired
		}
		if err := preflight(driveCtx, e, branch); err != nil {
			return nil, err
		}
		runID, err = trigger(driveCtx, e, branch, headSHA, req)
		if err != nil {
			return nil, err
		}
	}
	return s.drive(ctx, driveCtx, e, runID)
}

// attach returns the id of an in-flight run for this exact head, or "" when a
// fresh run has to be started.
func (s *LocalService) attach(ctx context.Context, e *env, branch, headSHA string) (string, error) {
	var active ipc.GetActiveRunResult
	source := &IPCRunStateSource{SocketPath: e.p.Socket()}
	if err := source.CallWithSlowReplyRetry(ctx, ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: e.repo.ID, Branch: branch}, &active); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", fmt.Errorf("get active run: %w", err)
	}
	if run := activeRunForHead(active.Run, headSHA); run != nil {
		return run.ID, nil
	}
	return "", nil
}

func activeRunForHead(run *ipc.RunInfo, headSHA string) *ipc.RunInfo {
	if run == nil || TerminalStatus(string(run.Status)) {
		return nil
	}
	matchesSubmitted := run.SubmittedHeadSHA != nil && *run.SubmittedHeadSHA == headSHA
	if run.HeadSHA != headSHA && !matchesSubmitted {
		return nil
	}
	return run
}

// preflight mirrors the branch and commit hygiene AXI enforces before a fresh
// run. The gate validates committed history, so a default-branch or dirty
// working tree would otherwise validate the wrong thing.
func preflight(ctx context.Context, e *env, branch string) error {
	if e.repo.DefaultBranch != "" && branch == e.repo.DefaultBranch {
		return fmt.Errorf("%w: %q is the default branch", ErrDefaultBranch, branch)
	}
	dirty, err := git.HasUncommittedChanges(ctx, e.repoPath)
	if err != nil {
		return fmt.Errorf("inspect working tree: %w", err)
	}
	if !dirty {
		return nil
	}
	untracked, _ := git.UntrackedFiles(ctx, e.repoPath)
	return &DirtyWorktreeError{Untracked: untracked}
}

// branchOwnership reports the pipeline's claim on the branch when a fresh push
// would discard commits that live only in the gate.
func branchOwnership(ctx context.Context, e *env) *branchsync.State {
	service := &branchsync.Service{
		DB: e.d, Repo: e.repo, WorkDir: e.repoPath, GateDir: e.p.RepoDir(e.repo.ID),
		Paths: e.p, RemoteTimeout: e.cfg.BranchSyncRemoteTimeout,
	}
	state := service.InspectCached(ctx)
	switch state.State {
	case branchsync.StatePipelineOwned:
		// An active run whose head has not moved holds no gate-only commits,
		// so superseding it with a new push stays available.
		if branchsync.RunHeadUnmoved(state) {
			return nil
		}
		return &state
	case branchsync.StatePushInProgress:
		return &state
	default:
		return nil
	}
}

// trigger starts a fresh run by pushing the caller's HEAD through the gate,
// falling back to a rerun when the push was a no-op because the gate already
// holds this commit.
func trigger(ctx context.Context, e *env, branch, headSHA string, req RunRequest) (string, error) {
	pushOptions := FormatSkipPushOptions(req.Skip)
	if opt := FormatIntentPushOption(req.Intent); opt != "" {
		pushOptions = append(pushOptions, opt)
	}
	priorRunIDs, err := runIDsForHead(ctx, e.client, e.repo.ID, branch, headSHA)
	if err != nil {
		// Without a baseline a matching terminal run may predate this push, so
		// do not attach to one; the active-run lookup below still applies.
		priorRunIDs = nil
	}
	if state := branchOwnership(ctx, e); state != nil {
		return "", &BranchOwnershipError{State: *state}
	}
	pushErr := git.PushWithOptions(ctx, e.repoPath, gate.RemoteName, "refs/heads/"+branch, "", false, pushOptions)
	if pushErr != nil {
		// Close the inspection-to-push race: if the pipeline took ownership
		// after the check above, keep the structured refusal rather than
		// leaking the resulting non-fast-forward.
		if state := branchOwnership(ctx, e); state != nil {
			return "", &BranchOwnershipError{State: *state}
		}
	}
	run, waitErr := waitForTriggeredRun(ctx, e.client, e.repo.ID, branch, headSHA, priorRunIDs)
	if waitErr != nil {
		return "", fmt.Errorf("wait for triggered run: %w", waitErr)
	}
	if run != nil {
		return run.ID, nil
	}
	if pushErr != nil {
		return "", fmt.Errorf("push %q to gate: %w", branch, pushErr)
	}
	callerHead, err := cleanCallerHead(ctx, e.repoPath)
	if err != nil {
		return "", err
	}
	var rr ipc.RerunResult
	params := &ipc.RerunParams{
		RepoID: e.repo.ID, Branch: branch, SkipSteps: req.Skip,
		Intent: req.Intent, CallerHeadSHA: callerHead,
	}
	if err := callIPC(ctx, e.client, ipc.MethodRerun, params, &rr); err != nil {
		return "", fmt.Errorf("no run started for %q: %w", branch, err)
	}
	return rr.RunID, nil
}

func runIDsForHead(ctx context.Context, client *ipc.Client, repoID, branch, headSHA string) (map[string]struct{}, error) {
	runs, err := runsForHead(ctx, client, repoID, branch, headSHA)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		ids[run.ID] = struct{}{}
	}
	return ids, nil
}

func runsForHead(ctx context.Context, client *ipc.Client, repoID, branch, headSHA string) ([]ipc.RunInfo, error) {
	var result ipc.GetRunsResult
	if err := callIPC(ctx, client, ipc.MethodGetRunsForHead, &ipc.GetRunsForHeadParams{RepoID: repoID, Branch: branch, HeadSHA: headSHA}, &result); err != nil {
		return nil, err
	}
	return result.Runs, nil
}

// waitForTriggeredRun waits for the run this trigger created. priorRunIDs keeps
// an up-to-date push from attaching to a terminal run an earlier one left.
func waitForTriggeredRun(ctx context.Context, client *ipc.Client, repoID, branch, headSHA string, priorRunIDs map[string]struct{}) (*ipc.RunInfo, error) {
	deadline := time.NewTimer(triggerWaitTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var result ipc.GetActiveRunResult
		if err := callIPC(ctx, client, ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repoID, Branch: branch}, &result); err != nil {
			return nil, err
		}
		if run := activeRunForHead(result.Run, headSHA); run != nil {
			return run, nil
		}
		if priorRunIDs != nil {
			runs, err := runsForHead(ctx, client, repoID, branch, headSHA)
			if err != nil {
				return nil, err
			}
			for i := range runs {
				run := &runs[i]
				if _, existed := priorRunIDs[run.ID]; !existed {
					return run, nil
				}
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, nil
		case <-poll.C:
		}
	}
}

func ipcCallTimeout(ctx context.Context) time.Duration {
	timeout := triggerWaitTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			return remaining
		}
	}
	return timeout
}

func callIPC(ctx context.Context, client *ipc.Client, method string, params, result interface{}) error {
	err := client.CallWithContext(ctx, method, params, result, ipcCallTimeout(ctx))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return err
}

// cleanCallerHead captures clean-head evidence at the request boundary. A dirty
// caller supplies none, preserving the daemon's existing behaviour.
func cleanCallerHead(ctx context.Context, dir string) (string, error) {
	// Read both facts from one status observation: a later HEAD lookup could
	// name a different commit whose worktree was never clean.
	out, err := git.Run(ctx, dir, "status", "--porcelain=v2", "--branch", "-z")
	if err != nil {
		return "", fmt.Errorf("check working tree: %w", err)
	}
	var head string
	for _, entry := range strings.Split(out, "\x00") {
		if oid, ok := strings.CutPrefix(entry, "# branch.oid "); ok {
			head = oid
		} else if entry != "" && !strings.HasPrefix(entry, "# ") {
			return "", nil
		}
	}
	if head == "" || head == "(initial)" {
		return "", fmt.Errorf("get caller head: no HEAD commit")
	}
	return head, nil
}

// Respond sends one gate decision and drives the run to its next decision
// point, terminal outcome, or the bounded wait.
//
// The decision itself is the caller's: this boundary applies no policy to the
// findings it forwards and never substitutes an action of its own.
func (s *LocalService) Respond(ctx context.Context, req RespondRequest) (*RunState, error) {
	wait := req.Wait
	if wait <= 0 {
		wait = DefaultWait
	}
	e, err := s.openEnv(req.RepoPath, true)
	if err != nil {
		return nil, err
	}
	defer e.close()

	driveCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	runID := req.RunID
	explicitRunID := runID != ""
	if runID == "" {
		branch, err := currentBranch(ctx, e.repoPath)
		if err != nil {
			return nil, err
		}
		if branch == "" {
			return nil, ErrDetachedHEAD
		}
		var active ipc.GetActiveRunResult
		source := &IPCRunStateSource{SocketPath: e.p.Socket()}
		if err := source.CallWithSlowReplyRetry(driveCtx, ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: e.repo.ID, Branch: branch}, &active); err != nil {
			return nil, fmt.Errorf("get active run: %w", err)
		}
		if active.Run == nil {
			return nil, ErrNoRunForBranch
		}
		runID = active.Run.ID
	}
	if explicitRunID {
		run, err := resolveRun(e, runID, "")
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, fmt.Errorf("run %s not found", runID)
		}
	}

	run, err := getRun(driveCtx, e.p.Socket(), runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("run %s not found", runID)
	}
	state := stateFromIPC(run)
	if state.Gate == nil {
		return nil, ErrNoGate
	}
	// The decision check and the step the action lands on must come from the
	// same snapshot. Re-checking here - not only in the caller, which read the
	// gate earlier and separately - is what stops a response aimed at a
	// mechanical gate from answering an ask-user gate the run advanced into in
	// between.
	if RequiresUserDecision(state.Gate) && !req.UserDecisionGiven {
		return nil, ErrUserDecisionRequired
	}
	gateStep, gateStatus := state.Gate.Step, state.Gate.Status

	var result ipc.RespondResult
	params := &ipc.RespondParams{
		RunID: runID, Step: types.StepName(gateStep), Action: req.Action,
		FindingIDs: req.FindingIDs, Instructions: req.Instructions,
	}
	if err := e.client.Call(ipc.MethodRespond, params, &result); err != nil {
		return nil, fmt.Errorf("respond to %s: %w", gateStep, err)
	}
	if !result.OK {
		return nil, fmt.Errorf("daemon rejected the response")
	}
	// respond is asynchronous. Wait for the step to actually leave the gate
	// before watching, so the drive loop cannot observe - and report - the same
	// gate the caller just answered.
	if err := waitStepLeavesGate(driveCtx, e.p.Socket(), runID, gateStep, gateStatus); err != nil {
		if state, ok := elapsed(ctx, driveCtx, err, e, runID); ok {
			return state, nil
		}
		return nil, err
	}
	return s.drive(ctx, driveCtx, e, runID)
}

func getRun(ctx context.Context, socketPath, runID string) (*ipc.RunInfo, error) {
	return (&IPCRunStateSource{SocketPath: socketPath}).Reconcile(ctx, runID)
}

// waitStepLeavesGate blocks until the answered step's status changes, or the
// run terminates.
func waitStepLeavesGate(ctx context.Context, socketPath, runID, step, gateStatus string) error {
	reconciler := NewRunReconciler(&IPCRunStateSource{SocketPath: socketPath}, runID)
	defer reconciler.Close()
	for {
		run, err := reconciler.Next(ctx)
		if err != nil {
			return err
		}
		if run == nil || TerminalStatus(string(run.Status)) {
			return nil
		}
		for _, s := range run.Steps {
			if string(s.StepName) == step {
				if string(s.Status) != gateStatus {
					return nil
				}
				break
			}
		}
	}
}

// drive watches a run until it parks at a gate, reaches a terminal outcome,
// establishes CI readiness with the PR left for a human to merge, or the
// bounded wait ends.
func (s *LocalService) drive(parent, driveCtx context.Context, e *env, runID string) (*RunState, error) {
	reconciler := NewRunReconciler(&IPCRunStateSource{SocketPath: e.p.Socket()}, runID)
	defer reconciler.Close()
	for {
		run, err := reconciler.Next(driveCtx)
		if err != nil {
			if state, ok := elapsed(parent, driveCtx, err, e, runID); ok {
				return state, nil
			}
			return nil, err
		}
		if run == nil {
			return nil, fmt.Errorf("run %s not found", runID)
		}
		state := stateFromIPC(run)
		state.RepoPath = e.repoPath
		switch {
		case state.Terminal, state.Gate != nil, state.CIReady:
			return state, nil
		}
	}
}

// elapsed converts a hold that ran out of its own budget - as opposed to a
// cancelled caller - into a still-running snapshot. A bounded wait ending is
// not a pipeline failure: the run keeps going and the caller reattaches.
func elapsed(parent, driveCtx context.Context, err error, e *env, runID string) (*RunState, bool) {
	if err == nil || parent.Err() != nil || driveCtx.Err() != context.DeadlineExceeded {
		return nil, false
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return nil, false
	}
	state := &RunState{RepoPath: e.repoPath, RunID: runID, WaitElapsed: true}
	// Report the last durable state rather than nothing, so the caller still
	// learns the branch and head the run is working on.
	if run, runErr := e.d.GetRun(runID); runErr == nil && run != nil {
		steps, stepErr := e.d.GetStepsByRun(runID)
		if stepErr == nil {
			durable := stateFromDB(run, steps)
			durable.RepoPath = e.repoPath
			durable.WaitElapsed = true
			return durable, true
		}
	}
	return state, true
}
