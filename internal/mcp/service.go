package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/axiapi"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Additional error codes. Together with the repository-policy codes these are
// the gateway's machine contract.
const (
	CodeIntentRequired       = "intent_required"
	CodeInvalidAction        = "invalid_action"
	CodeNoGate               = "no_gate_open"
	CodeUserDecisionRequired = "user_decision_required"
	CodeNestedGateContext    = "nested_gate_context"
	CodeSyncNotAuthorized    = "sync_not_authorized"
	CodeRepoNotInitialized   = "repo_not_initialized"
	CodeNotAGitRepo          = "not_a_git_repository"
	CodeDetachedHead         = "detached_head"
	CodeDirtyWorktree        = "dirty_worktree"
	CodeBranchOwnedByGate    = "branch_owned_by_pipeline"
	CodeNoRunForBranch       = "no_run_for_branch"
	CodeInvalidStep          = "invalid_step"
	CodeAXIError             = "axi_error"
)

// Log bounds. A caller asks for a tail; the gateway decides how long a tail it
// is willing to hand back, so no request can pull an unbounded document through
// the protocol.
const (
	DefaultLogTailLines = 80
	MaxLogTailLines     = 500
)

// maxWait bounds a caller-supplied hold. The point of the hold is to return
// before the caller's own tool budget expires, so an unbounded one defeats it.
const maxWait = 30 * time.Minute

// Sync next-action codes AXI uses to authorize a mutating synchronization.
// Anything else is a state the gateway reports and does not act on.
const (
	nextActionSync    = "sync"
	nextActionRecover = "recover_custody"
)

// Service backs the v1 tools. It holds no run state of its own: AXI owns every
// state transition, and stopping this process leaves an active run running.
type Service struct {
	AXI    axiapi.Service
	Policy *RepositoryPolicy
}

type StatusInput struct {
	RepoPath string `json:"repo_path"`
	RunID    string `json:"run_id,omitempty"`
}

type RunInput struct {
	RepoPath string `json:"repo_path"`
	// Intent is the user's goal, constraints, exclusions, acceptance criteria,
	// and material decisions. It is mandatory and must not be lossily
	// summarized: the pipeline uses it directly instead of inferring one.
	Intent      string   `json:"intent"`
	Skip        []string `json:"skip,omitempty"`
	BaseBranch  string   `json:"base_branch,omitempty"`
	WaitSeconds int      `json:"wait_seconds,omitempty"`
}

type RespondInput struct {
	RepoPath   string   `json:"repo_path"`
	RunID      string   `json:"run_id,omitempty"`
	Action     string   `json:"action"`
	FindingIDs []string `json:"finding_ids,omitempty"`
	// Instructions are per-finding notes forwarded to a fix round.
	Instructions map[string]string `json:"instructions,omitempty"`
	// UserDecision records a decision a human actually made, and is the only
	// thing that lets a gate holding ask-user findings proceed. An agent must
	// not fill it in on a human's behalf; without it such a gate is returned
	// for a decision.
	UserDecision string `json:"user_decision,omitempty"`
	WaitSeconds  int    `json:"wait_seconds,omitempty"`
}

type LogsInput struct {
	RepoPath  string `json:"repo_path"`
	RunID     string `json:"run_id,omitempty"`
	Step      string `json:"step"`
	TailLines int    `json:"tail_lines,omitempty"`
}

type SyncInput struct {
	RepoPath string `json:"repo_path"`
	// Apply performs the synchronization AXI's next action authorizes.
	Apply bool `json:"apply,omitempty"`
	// Recover returns custody of a branch stranded by a terminal run, and is
	// authorized only by AXI's own recovery next action.
	Recover   bool `json:"recover,omitempty"`
	KeepLocal bool `json:"keep_local,omitempty"`
}

type DoctorInput struct {
	RepoPath string `json:"repo_path"`
}

// Status is read-only.
func (s *Service) Status(ctx context.Context, in StatusInput) *Receipt {
	repoPath, policyErr := s.Policy.Resolve(in.RepoPath)
	if policyErr != nil {
		return NewErrorReceipt(OpStatus, in.RepoPath, policyErr)
	}
	state, err := s.AXI.Status(ctx, repoPath, strings.TrimSpace(in.RunID))
	if err != nil {
		return s.fail(OpStatus, repoPath, err)
	}
	return NewReceipt(OpStatus, repoPath, state)
}

// Run starts or reattaches to a run, and returns at the first gate, the
// terminal outcome, established CI readiness, or the bounded wait.
//
// There is deliberately no auto-approve input: a gate always comes back to the
// caller.
func (s *Service) Run(ctx context.Context, in RunInput) *Receipt {
	repoPath, policyErr := s.Policy.Resolve(in.RepoPath)
	if policyErr != nil {
		return NewErrorReceipt(OpRun, in.RepoPath, policyErr)
	}
	if strings.TrimSpace(in.Intent) == "" {
		return NewErrorReceipt(OpRun, repoPath, &PolicyError{
			Code:        CodeIntentRequired,
			Message:     "intent is required to start a run.",
			Remediation: "Pass what the user set out to accomplish - goal, constraints, exclusions, acceptance criteria - not a description of the diff.",
		})
	}
	skip, err := parseSteps(in.Skip)
	if err != nil {
		return NewErrorReceipt(OpRun, repoPath, &PolicyError{
			Code: CodeInvalidStep, Message: err.Error(),
			Remediation: "Valid steps: " + strings.Join(stepNames(), ", "),
		})
	}
	if refusal := s.refuseNested(ctx, OpRun, repoPath); refusal != nil {
		return refusal
	}
	state, err := s.AXI.Run(ctx, axiapi.RunRequest{
		RepoPath: repoPath, Intent: in.Intent, BaseBranch: in.BaseBranch,
		Skip: skip, Wait: boundedWait(in.WaitSeconds),
	})
	if err != nil {
		return s.fail(OpRun, repoPath, err)
	}
	return NewReceipt(OpRun, repoPath, state)
}

// Respond forwards one gate decision.
//
// A gate holding ask-user findings, or a protected-path refusal, is returned
// for an external decision instead: the gateway never resolves what the
// pipeline referred to a human, and the refusal happens here, before the
// daemon is contacted at all.
func (s *Service) Respond(ctx context.Context, in RespondInput) *Receipt {
	repoPath, policyErr := s.Policy.Resolve(in.RepoPath)
	if policyErr != nil {
		return NewErrorReceipt(OpRespond, in.RepoPath, policyErr)
	}
	action, ok := approvalAction(in.Action)
	if !ok {
		return NewErrorReceipt(OpRespond, repoPath, &PolicyError{
			Code:        CodeInvalidAction,
			Message:     fmt.Sprintf("action %q is not supported.", in.Action),
			Remediation: "Use approve, fix, or skip. Aborting a run is not an MCP operation.",
		})
	}
	if refusal := s.refuseNested(ctx, OpRespond, repoPath); refusal != nil {
		return refusal
	}
	state, err := s.AXI.Status(ctx, repoPath, strings.TrimSpace(in.RunID))
	if err != nil {
		return s.fail(OpRespond, repoPath, err)
	}
	if state == nil || state.Gate == nil {
		return NewErrorReceipt(OpRespond, repoPath, &PolicyError{
			Code:        CodeNoGate,
			Message:     "the run is not awaiting a decision.",
			Remediation: "Call nomistakes_status to see what the run is doing.",
		})
	}
	if requiresUserDecision(state.Gate) && strings.TrimSpace(in.UserDecision) == "" {
		receipt := NewReceipt(OpRespond, repoPath, state)
		receipt.OK = false
		receipt.Error = &ErrorBody{
			Code:    CodeUserDecisionRequired,
			Message: "this gate holds findings the pipeline referred to a human; it cannot be progressed without an explicit decision.",
			Remediation: "Return the findings to the user, then call nomistakes_respond again with user_decision set to the decision they gave. " +
				"Do not fill user_decision in on their behalf.",
		}
		return receipt
	}
	next, err := s.AXI.Respond(ctx, axiapi.RespondRequest{
		RepoPath: repoPath, RunID: state.RunID, Action: action,
		FindingIDs: in.FindingIDs, Instructions: in.Instructions, Wait: boundedWait(in.WaitSeconds),
	})
	if err != nil {
		return s.fail(OpRespond, repoPath, err)
	}
	return NewReceipt(OpRespond, repoPath, next)
}

// Logs is read-only and bounded.
func (s *Service) Logs(ctx context.Context, in LogsInput) *Receipt {
	repoPath, policyErr := s.Policy.Resolve(in.RepoPath)
	if policyErr != nil {
		return NewErrorReceipt(OpLogs, in.RepoPath, policyErr)
	}
	log, err := s.AXI.Logs(ctx, axiapi.LogsRequest{
		RepoPath: repoPath, RunID: strings.TrimSpace(in.RunID),
		Step: in.Step, TailLines: s.logTail(in.TailLines),
	})
	if err != nil {
		return s.fail(OpLogs, repoPath, err)
	}
	receipt := &Receipt{OK: true, Operation: OpLogs, RepoPath: repoPath, RunID: log.RunID, Step: log.Step}
	receipt.Extra = map[string]any{
		"lines":       log.Lines,
		"total_lines": log.TotalLines,
		"truncated":   log.Truncated,
	}
	return receipt
}

// logTail applies the gateway's own bounds to a caller's request.
func (s *Service) logTail(requested int) int {
	if requested <= 0 {
		return DefaultLogTailLines
	}
	if requested > MaxLogTailLines {
		return MaxLogTailLines
	}
	return requested
}

// Sync inspects branch synchronization, and mutates only when AXI's own current
// next action authorizes exactly the mutation requested. A speculative sync -
// one the gateway decided was probably a good idea - is refused.
func (s *Service) Sync(ctx context.Context, in SyncInput) *Receipt {
	repoPath, policyErr := s.Policy.Resolve(in.RepoPath)
	if policyErr != nil {
		return NewErrorReceipt(OpSync, in.RepoPath, policyErr)
	}
	mutating := in.Apply || in.Recover
	if mutating {
		if refusal := s.refuseNested(ctx, OpSync, repoPath); refusal != nil {
			return refusal
		}
	}
	// Always read first. The authorization is AXI's live next action, not
	// anything the caller asserted.
	state, err := s.AXI.Sync(ctx, axiapi.SyncRequest{RepoPath: repoPath})
	if err != nil {
		return s.fail(OpSync, repoPath, err)
	}
	if !mutating {
		return syncReceipt(repoPath, state)
	}
	want := nextActionSync
	if in.Recover {
		want = nextActionRecover
	}
	if state.NextActionCode != want {
		receipt := syncReceipt(repoPath, state)
		receipt.OK = false
		receipt.Error = &ErrorBody{
			Code: CodeSyncNotAuthorized,
			Message: fmt.Sprintf("no-mistakes reports next action %q, which does not authorize this synchronization.",
				state.NextActionCode),
			Remediation: "Follow the next action no-mistakes reported; see data.next_action.",
		}
		return receipt
	}
	applied, err := s.AXI.Sync(ctx, axiapi.SyncRequest{
		RepoPath: repoPath, Apply: in.Apply && !in.Recover, Recover: in.Recover, KeepLocal: in.KeepLocal,
	})
	if err != nil {
		return s.fail(OpSync, repoPath, err)
	}
	return syncReceipt(repoPath, applied)
}

func syncReceipt(repoPath string, state *axiapi.SyncState) *Receipt {
	receipt := &Receipt{OK: true, Operation: OpSync, RepoPath: repoPath, State: state.State}
	receipt.Extra = map[string]any{
		"relation":      state.Relation,
		"safety":        state.Safety,
		"local_head":    state.LocalHead,
		"pipeline_head": state.PipelineHead,
		"changed":       state.Changed,
		"recovered":     state.Recovered,
		"next_action":   map[string]any{"code": state.NextActionCode, "command": state.NextActionCommand},
	}
	if state.SyncError != "" {
		receipt.Warnings = append(receipt.Warnings, state.SyncError)
	}
	return receipt
}

// Doctor is read-only.
func (s *Service) Doctor(ctx context.Context, in DoctorInput) *Receipt {
	repoPath, policyErr := s.Policy.Resolve(in.RepoPath)
	if policyErr != nil {
		return NewErrorReceipt(OpDoctor, in.RepoPath, policyErr)
	}
	report, err := s.AXI.Doctor(ctx, repoPath)
	if err != nil {
		return s.fail(OpDoctor, repoPath, err)
	}
	agents := make([]map[string]any, 0, len(report.Agents))
	for _, agent := range report.Agents {
		agents = append(agents, map[string]any{"name": agent.Name, "available": agent.Available})
	}
	receipt := &Receipt{OK: true, Operation: OpDoctor, RepoPath: repoPath}
	receipt.Extra = map[string]any{
		"repo_initialized": report.RepoInitialized,
		"daemon_running":   report.DaemonRunning,
		"default_branch":   report.DefaultBranch,
		"agents":           agents,
	}
	if report.ConfigError != "" {
		receipt.Warnings = append(receipt.Warnings, "global config: "+report.ConfigError)
	}
	if !report.RepoInitialized {
		receipt.Warnings = append(receipt.Warnings, "This repository is not initialized for no-mistakes; run `no-mistakes init` in it.")
	}
	return receipt
}

// refuseNested stops a mutating tool when the caller is itself an active
// no-mistakes validation step. Such a caller owns one phase, not the pipeline.
func (s *Service) refuseNested(ctx context.Context, operation, repoPath string) *Receipt {
	result, err := s.AXI.GateContext(ctx, repoPath)
	if err != nil {
		// Fail closed: an unclassifiable caller does not get to mutate.
		return NewErrorReceipt(operation, repoPath, &PolicyError{
			Code:        CodeNestedGateContext,
			Message:     "could not establish the calling execution context: " + err.Error(),
			Remediation: "Retry once the no-mistakes daemon is reachable.",
		})
	}
	if !result.Nested {
		return nil
	}
	message := "refusing pipeline control from an active no-mistakes validation step."
	if result.RunID != "" {
		message = fmt.Sprintf("%s (run %s, phase %s)", strings.TrimSuffix(message, "."), result.RunID, result.Phase)
	}
	return NewErrorReceipt(operation, repoPath, &PolicyError{
		Code:        CodeNestedGateContext,
		Message:     message,
		Remediation: "Return control to the enclosing executor; it owns validation, push, PR, and CI. Read-only tools remain available.",
	})
}

// fail maps an AXI error onto a typed receipt, preserving no-mistakes' own
// remediation where there is one.
func (s *Service) fail(operation, repoPath string, err error) *Receipt {
	body := &ErrorBody{Code: CodeAXIError, Message: err.Error()}
	switch {
	case errors.Is(err, axiapi.ErrRepoNotInitialized):
		body.Code = CodeRepoNotInitialized
		body.Remediation = "Run `no-mistakes init` in the repository to set up the gate."
	case errors.Is(err, axiapi.ErrNotAGitRepository):
		body.Code = CodeNotAGitRepo
		body.Remediation = "Point repo_path at a git working tree."
	case errors.Is(err, axiapi.ErrDetachedHEAD):
		body.Code = CodeDetachedHead
		body.Remediation = "Check out a branch before validating: `git switch -c <branch>`."
	case errors.Is(err, axiapi.ErrIntentRequired):
		body.Code = CodeIntentRequired
		body.Remediation = "Pass what the user set out to accomplish."
	case errors.Is(err, axiapi.ErrNoRunForBranch):
		body.Code = CodeNoRunForBranch
		body.Remediation = "Start one with nomistakes_run, or name an existing run with run_id."
	case errors.Is(err, axiapi.ErrNoGate):
		body.Code = CodeNoGate
		body.Remediation = "Call nomistakes_status to see what the run is doing."
	}
	var dirty *axiapi.DirtyWorktreeError
	if errors.As(err, &dirty) {
		body.Code = CodeDirtyWorktree
		body.Remediation = "Commit the files that belong to this change, or exclude the rest; the gate validates committed history."
		if len(dirty.Untracked) > 0 {
			body.Message = fmt.Sprintf("%s Untracked: %s", body.Message, strings.Join(boundedPaths(dirty.Untracked), ", "))
		}
	}
	var owned *axiapi.BranchOwnershipError
	if errors.As(err, &owned) {
		body.Code = CodeBranchOwnedByGate
		body.Remediation = "Call nomistakes_sync to see the branch state and the action no-mistakes reports."
	}
	var nested *axiapi.NestedGateError
	if errors.As(err, &nested) {
		body.Code = CodeNestedGateContext
		body.Remediation = "Return control to the enclosing executor."
	}
	return &Receipt{OK: false, Operation: operation, RepoPath: repoPath, Error: body}
}

// boundedPaths caps how many paths an error names, so a large untracked tree
// cannot bloat a receipt.
func boundedPaths(paths []string) []string {
	const max = 5
	if len(paths) <= max {
		return paths
	}
	out := append([]string(nil), paths[:max]...)
	return append(out, fmt.Sprintf("(+%d more)", len(paths)-max))
}

// approvalAction accepts only the three decisions a gate takes. Abort is
// deliberately not an MCP operation: it is easy to misuse mid-run and
// no-mistakes treats it as a between-runs action.
func approvalAction(action string) (types.ApprovalAction, bool) {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case string(types.ActionApprove):
		return types.ActionApprove, true
	case string(types.ActionFix):
		return types.ActionFix, true
	case string(types.ActionSkip):
		return types.ActionSkip, true
	default:
		return "", false
	}
}

func parseSteps(values []string) ([]types.StepName, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]types.StepName, 0, len(values))
	for _, value := range values {
		step := types.StepName(strings.ToLower(strings.TrimSpace(value)))
		if !knownStep(step) {
			return nil, fmt.Errorf("unknown step %q", value)
		}
		out = append(out, step)
	}
	return out, nil
}

func knownStep(step types.StepName) bool {
	for _, known := range types.AllSteps() {
		if step == known {
			return true
		}
	}
	return false
}

func stepNames() []string {
	all := types.AllSteps()
	out := make([]string, 0, len(all))
	for _, step := range all {
		out = append(out, string(step))
	}
	return out
}

func boundedWait(seconds int) time.Duration {
	if seconds <= 0 {
		return axiapi.DefaultWait
	}
	wait := time.Duration(seconds) * time.Second
	if wait > maxWait {
		return maxWait
	}
	return wait
}
